package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var protocolPlaceholderLike = regexp.MustCompile(`(?i)(?:\\?\{\s*){0,3}[A-Za-z][A-Za-z0-9_]{0,12}\s*_\s*(?:[0-9a-f]{16}|[bcdfghjkmnpqrstvwxz]{6})\s*(?:\\?\}\s*){0,3}`)

func hasUnresolvedPlaceholder(text string) bool { return protocolPlaceholderLike.MatchString(text) }

func isRestorableContentType(contentType string) bool {
	return strings.HasPrefix(contentType, "text/") || strings.Contains(contentType, "json") || strings.Contains(contentType, "event-stream")
}

// Inspect only an initial prefix and replay it through the buffered reader.
// Avoid Peek(512): a streaming upstream may flush a short event and then wait
// indefinitely, so waiting to fill a sniff buffer would stall the client.
func sniffResponseContentType(reader *bufio.Reader) (string, error) {
	first, err := reader.Peek(1)
	if err == io.EOF {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	size := reader.Buffered()
	if size > 512 {
		size = 512
	}
	prefix, _ := reader.Peek(size)
	trimmed := bytes.TrimSpace(prefix)
	if len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') {
		return "application/json", nil
	}
	if first[0] == 'd' || first[0] == 'e' || first[0] == ':' {
		if len(prefix) < 6 {
			prefix, err = reader.Peek(6)
			if err != nil && err != io.EOF {
				return "", err
			}
		}
		if bytes.HasPrefix(prefix, []byte("data:")) || bytes.HasPrefix(prefix, []byte("event:")) || bytes.HasPrefix(prefix, []byte(":")) {
			return "text/event-stream", nil
		}
	}
	return strings.ToLower(http.DetectContentType(prefix)), nil
}

// Protocol deltas are logical text fragments, not adjacent wire bytes. Keep
// independent native streams for each choice/content block/tool call, then
// encode the restored fragment into its original event. In particular, tool
// arguments require inner JSON escaping before the outer event is serialized.
const maxRestoreFrameBytes = 32 << 20
const maxRestoreChannels = 1024

type restoreFactory func(context.Context) (RestoreSession, error)
type restoreTokenEvidence struct {
	Observed  int `json:"observed"`
	Remaining int `json:"remaining"`
}
type restoreCounts struct {
	Total, Restored, Unresolved int
	Tokens                      map[string]restoreTokenEvidence
	Complete                    bool
}
type countedRestoreSession interface{ RestoreCounts() restoreCounts }

type protocolSlot struct {
	path      []any
	channel   string
	arguments bool
	snapshot  bool
}

type protocolChannel struct {
	session               RestoreSession
	arguments             bool
	toolInput             bool
	argumentEscapes       protocolArgumentEscapes
	argumentGuard         protocolArgumentEscapes
	argumentGuardTail     string
	frame                 map[string]any
	path                  []any
	header                string
	inputTail, outputTail string
}

// Tool arguments are an inner JSON document. Normalize printable Unicode
// escapes inside its strings before native matching, including escapes split
// across deltas. Preserve escaped quotes/backslashes and non-ASCII values so
// the inner document remains valid and has exactly the same string values.
type protocolArgumentEscapes struct {
	pending    string
	inString   bool
	inspection bool // Decode controls only in the private tool guard, never on the wire.
}

func (n *protocolArgumentEscapes) push(text string, final bool) string {
	text = n.pending + text
	n.pending = ""
	var out strings.Builder
	for i := 0; i < len(text); {
		c := text[i]
		if n.inString && c == '\\' {
			need := 2
			if i+1 < len(text) && text[i+1] == 'u' {
				need = 6
			}
			if len(text)-i < need {
				if !final {
					n.pending = text[i:]
					break
				}
				out.WriteString(text[i:])
				break
			}
			if need == 6 {
				value, err := strconv.ParseUint(text[i+2:i+6], 16, 16)
				if err == nil && value <= 0x7e && (n.inspection || (value >= 0x20 && value != '"' && value != '\\')) {
					out.WriteByte(byte(value))
					i += 6
					continue
				}
			}
			if n.inspection && need == 2 {
				if value, ok := map[byte]byte{'t': '\t', 'n': '\n', 'r': '\r', 'b': '\b', 'f': '\f', '"': '"', '\\': '\\', '/': '/'}[text[i+1]]; ok {
					out.WriteByte(value)
					i += 2
					continue
				}
			}
			out.WriteString(text[i : i+need])
			i += need
			continue
		}
		if c == '"' {
			n.inString = !n.inString
		}
		out.WriteByte(c)
		i++
	}
	return out.String()
}

func (c *protocolChannel) checkArguments(out []byte, final bool) error {
	if !c.arguments && !c.toolInput {
		return nil
	}
	decoded := c.argumentGuard.push(string(out), final)
	// Check the full fragment plus its prior tail before truncating the tail:
	// a token can occur near the beginning of a large tool delta.
	combined := c.argumentGuardTail + decoded
	if hasUnresolvedPlaceholder(combined) {
		return errors.New("unresolved placeholder in tool arguments")
	}
	if len(combined) > restoreTokenTailLimit {
		combined = combined[len(combined)-restoreTokenTailLimit:]
	}
	c.argumentGuardTail = combined
	return nil
}

func hasUnresolvedToolPlaceholder(text string) bool {
	if hasUnresolvedPlaceholder(text) {
		return true
	}
	guard := protocolArgumentEscapes{inspection: true}
	return hasUnresolvedPlaceholder(guard.push(text, true))
}

func (c *protocolChannel) finish(ctx context.Context) ([]byte, error) {
	var out []byte
	if c.arguments {
		if pending := c.argumentEscapes.push("", true); pending != "" {
			var err error
			out, err = c.session.Push(ctx, []byte(pending))
			if err != nil {
				return nil, err
			}
		}
	}
	tail, err := c.session.Finish(ctx)
	return append(out, tail...), err
}

type protocolRestoreSession struct {
	skipLF            bool
	factory           restoreFactory
	kind              string
	pending           []byte
	channels          map[string]*protocolChannel
	counts            restoreCounts
	finished          bool
	completed         bool
	terminal          bool
	evidenceTruncated bool
}

func newProtocolRestoreSession(kind string, factory restoreFactory) *protocolRestoreSession {
	return &protocolRestoreSession{kind: kind, factory: factory, channels: map[string]*protocolChannel{}}
}

// StreamComplete recognizes whole-response markers, never per-choice or
// per-tool completion. Clients may close immediately after these markers.
func (s *protocolRestoreSession) StreamComplete() bool { return s.terminal }

func (s *protocolRestoreSession) RestoreCounts() restoreCounts {
	c := s.counts
	c.Complete = s.completed && !s.evidenceTruncated
	// Intermediate reads need only aggregate counters. Copy the bounded evidence
	// once at successful EOF, rather than once per tiny upstream SSE chunk.
	c.Tokens = nil
	if c.Complete {
		c.Tokens = make(map[string]restoreTokenEvidence, len(s.counts.Tokens))
		for token, evidence := range s.counts.Tokens {
			c.Tokens[token] = evidence
		}
	}
	if c.Unresolved > c.Total {
		c.Unresolved = c.Total
	}
	c.Restored = c.Total - c.Unresolved
	if !s.completed {
		// Input still buffered, cancelled, or failed is not proof of restoration.
		c.Restored = 0
	}
	return c
}

func (s *protocolRestoreSession) observe(tail *string, text []byte, output bool) {
	n := observeMaskedTokens(tail, text, func(token string) {
		if s.counts.Tokens == nil {
			s.counts.Tokens = map[string]restoreTokenEvidence{}
		}
		if _, exists := s.counts.Tokens[token]; !exists && len(s.counts.Tokens) >= 100000 {
			s.evidenceTruncated = true
			return
		}
		e := s.counts.Tokens[token]
		if output {
			e.Remaining++
		} else {
			e.Observed++
		}
		s.counts.Tokens[token] = e
	})
	if output {
		s.counts.Unresolved += n
	} else {
		s.counts.Total += n
	}
}

func (s *protocolRestoreSession) Push(ctx context.Context, data []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.finished {
		return nil, errors.New("protocol restore stream is closed")
	}
	if s.skipLF && len(data) > 0 {
		s.skipLF = false
		if data[0] == '\n' {
			data = data[1:]
		}
	}
	s.pending = append(s.pending, data...)
	var output []byte
	for {
		end := s.frameEnd()
		if end == 0 {
			if len(s.pending) > maxRestoreFrameBytes {
				return nil, errors.New("response frame exceeds restore limit")
			}
			break
		}
		if end > maxRestoreFrameBytes {
			return nil, errors.New("response frame exceeds restore limit")
		}
		frame := s.pending[:end]
		out, err := s.processFrame(ctx, frame)
		if err != nil {
			return nil, err
		}
		output = append(output, out...)
		s.pending = s.pending[end:]
		if s.terminal {
			s.pending = nil
			break
		}
	}
	return output, nil
}

func (s *protocolRestoreSession) frameEnd() int {
	if s.kind == "json" {
		return 0
	}
	start := 0
	for start < len(s.pending) {
		n := bytes.IndexAny(s.pending[start:], "\r\n")
		if n < 0 {
			return 0
		}
		lineEnd := start + n
		end := lineEnd + 1
		if s.pending[lineEnd] == '\r' && end < len(s.pending) && s.pending[end] == '\n' {
			end++
		}
		if s.kind == "ndjson" || lineEnd == start {
			s.skipLF = s.pending[lineEnd] == '\r' && end == len(s.pending)
			return end
		}
		start = end
	}
	return 0
}

func (s *protocolRestoreSession) Finish(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.finished {
		return nil, errors.New("protocol restore stream is closed")
	}
	var output []byte
	if len(s.pending) > 0 {
		out, err := s.processFrame(ctx, s.pending)
		if err != nil {
			return nil, err
		}
		output = append(output, out...)
		s.pending = nil
	}
	flushed, err := s.flush(ctx, func(string) bool { return true })
	if err != nil {
		return nil, err
	}
	output = append(output, flushed...)
	s.finished = true
	s.completed = true
	return output, nil
}

func (s *protocolRestoreSession) Abort() {
	for _, c := range s.channels {
		if a, ok := c.session.(AbortableRestoreSession); ok {
			a.Abort()
		}
	}
	s.channels = map[string]*protocolChannel{}
	s.pending = nil
	s.finished = true
}

func decodeProtocolJSON(payload []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return nil, err
	}
	if obj == nil {
		return nil, errors.New("empty protocol object")
	}
	// Unmarshal also rejects trailing data; retain json.Number in the tree.
	if !json.Valid(payload) {
		return nil, errors.New("invalid protocol JSON")
	}
	return obj, nil
}

func encodeProtocolJSON(obj map[string]any) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(obj)
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}

func (s *protocolRestoreSession) processFrame(ctx context.Context, frame []byte) ([]byte, error) {
	payload := bytes.TrimSpace(frame)
	header := ""
	if s.kind == "sse" {
		var data []string
		normalized := strings.ReplaceAll(strings.ReplaceAll(string(frame), "\r\n", "\n"), "\r", "\n")
		for _, line := range strings.Split(normalized, "\n") {
			if line == "data" || strings.HasPrefix(line, "data:") {
				value := strings.TrimPrefix(line, "data:")
				if line == "data" {
					value = ""
				}
				value = strings.TrimPrefix(value, " ")
				data = append(data, value)
			} else if line != "" {
				header += line + "\n"
			}
		}
		if len(data) == 0 {
			return frame, nil
		}
		payload = []byte(strings.Join(data, "\n"))
	}
	if s.kind == "sse" && string(payload) == "[DONE]" {
		out, err := s.flush(ctx, func(string) bool { return true })
		s.terminal = err == nil
		return append(out, frame...), err
	}
	if s.kind == "json" && json.Valid(payload) && len(payload) > 0 && payload[0] != '{' {
		dec := json.NewDecoder(bytes.NewReader(payload))
		dec.UseNumber()
		var value any
		_ = dec.Decode(&value)
		if err := s.restoreJSONValue(ctx, value); err != nil {
			return nil, err
		}
		if text, ok := value.(string); ok {
			restored, err := s.oneShot(ctx, text, false)
			if err != nil {
				return nil, err
			}
			b, _ := json.Marshal(restored)
			return b, nil
		}
		return replaceProtocolStrings(payload, value), nil
	}
	obj, err := decodeProtocolJSON(payload)
	if err != nil {
		// Non-JSON SSE payloads are independent events; they still use native
		// restoration, but must not be combined with a provider JSON channel.
		if s.kind == "json" {
			return nil, fmt.Errorf("invalid JSON response: %w", err)
		}
		restored, err := s.oneShot(ctx, string(payload), false)
		if err != nil {
			return nil, err
		}
		if restored == string(payload) {
			return frame, nil
		}
		if s.kind == "sse" {
			return []byte(header + "data: " + strings.ReplaceAll(restored, "\n", "\ndata: ") + "\n\n"), nil
		}
		return []byte(restored + "\n"), nil
	}
	classified := protocolFrameObject(obj, header)
	slots := protocolSlots(classified)
	terminal := protocolTerminal(classified)
	changed := false
	var before []byte
	for _, slot := range slots {
		text, ok := protocolGet(obj, slot.path).(string)
		if !ok {
			continue
		}
		var restored string
		if slot.snapshot || s.kind == "json" {
			if slot.snapshot && slot.channel != "" {
				flushed, flushErr := s.flush(ctx, func(key string) bool { return key == slot.channel })
				if flushErr != nil {
					return nil, flushErr
				}
				before = append(before, flushed...)
			}
			restored, err = s.oneShot(ctx, text, slot.arguments)
		} else {
			c := s.channels[slot.channel]
			if c == nil {
				if len(s.channels) >= maxRestoreChannels {
					return nil, errors.New("too many response restore channels")
				}
				var session RestoreSession
				session, err = s.factory(ctx)
				if err != nil {
					return nil, err
				}
				if configurable, ok := session.(ConfigurableRestoreSession); ok {
					err = configurable.SetJSONEscape(ctx, slot.arguments)
					if err != nil {
						if a, ok := session.(AbortableRestoreSession); ok {
							a.Abort()
						}
						return nil, err
					}
				}
				c = &protocolChannel{session: session, arguments: slot.arguments, toolInput: customToolInputChannel(slot.channel), argumentGuard: protocolArgumentEscapes{inspection: true}}
				s.channels[slot.channel] = c
			}
			input := text
			if c.arguments {
				input = c.argumentEscapes.push(text, false)
			}
			s.observe(&c.inputTail, []byte(input), false)
			var out []byte
			out, err = c.session.Push(ctx, []byte(input))
			if err == nil && terminal(slot.channel) {
				var tail []byte
				tail, err = c.finish(ctx)
				if err != nil {
					// Keep ownership until Abort can release a failed/canceled
					// terminal channel, just as flush does for other endings.
					return nil, err
				}
				out = append(out, tail...)
				delete(s.channels, slot.channel)
			}
			if err == nil {
				s.observe(&c.outputTail, out, true)
				restored = string(out)
				if err := c.checkArguments(out, terminal(slot.channel)); err != nil {
					return nil, err
				}
			}
			c.frame = cloneProtocolFrame(obj)
			c.path = slot.path
			c.header = header
		}
		if err != nil {
			return nil, err
		}
		// Never turn an unresolved placeholder into a command Hermes can run.
		if (slot.arguments || customToolInputChannel(slot.channel) || protocolToolContentPath(slot.path)) && hasUnresolvedToolPlaceholder(restored) {
			return nil, errors.New("unresolved placeholder in tool arguments")
		}
		if restored != text {
			protocolSet(obj, slot.path, restored)
			changed = true
		}
	}
	// A completion marker cannot overtake the final withheld token suffix.
	flushed, err := s.flush(ctx, terminal)
	before = append(before, flushed...)
	if err != nil {
		return nil, err
	}
	if s.kind != "json" {
		switch classified["type"] {
		case "message_stop", "response.completed", "response.incomplete", "response.failed":
			s.terminal = true
		}
		if classified["done"] == true {
			s.terminal = true
		}
	}
	if !changed {
		return append(before, frame...), nil
	}
	encodedPayload := replaceProtocolStrings(payload, obj)
	encoded := encodedPayload
	if s.kind == "sse" {
		encoded = []byte(header + "data: " + string(encodedPayload) + "\n\n")
	}
	if s.kind == "ndjson" {
		encoded = append(encodedPayload, '\n')
	}
	return append(before, encoded...), nil
}

// Preserve provider envelope bytes (key order, numeric precision, whitespace)
// while replacing only decoded string values. Synthetic EOF deltas use the
// regular encoder; existing frames never need to be fully reserialized.
func replaceProtocolStrings(payload []byte, obj any) []byte {
	type edit struct {
		start, end int
		value      []byte
	}
	var edits []edit
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	var walk func([]any)
	walk = func(path []any) {
		before := int(dec.InputOffset())
		token, err := dec.Token()
		if err != nil {
			return
		}
		switch t := token.(type) {
		case json.Delim:
			if t == '{' {
				for dec.More() {
					key, _ := dec.Token()
					walk(append(append([]any{}, path...), key.(string)))
				}
			} else if t == '[' {
				for i := 0; dec.More(); i++ {
					walk(append(append([]any{}, path...), i))
				}
			}
			_, _ = dec.Token()
		case string:
			if value, ok := protocolGet(obj, path).(string); ok && value != t {
				end := int(dec.InputOffset())
				start := before + bytes.IndexByte(payload[before:end], '"')
				var b bytes.Buffer
				e := json.NewEncoder(&b)
				e.SetEscapeHTML(false)
				_ = e.Encode(value)
				edits = append(edits, edit{start, end, bytes.TrimSuffix(b.Bytes(), []byte("\n"))})
			}
		}
	}
	walk(nil)
	if len(edits) == 0 {
		return append([]byte(nil), payload...)
	}
	var out []byte
	cursor := 0
	for _, edit := range edits {
		out = append(out, payload[cursor:edit.start]...)
		out = append(out, edit.value...)
		cursor = edit.end
	}
	return append(out, payload[cursor:]...)
}

func (s *protocolRestoreSession) restoreJSONValue(ctx context.Context, value any) error {
	switch obj := value.(type) {
	case map[string]any:
		for _, slot := range protocolSlots(obj) {
			text, ok := protocolGet(obj, slot.path).(string)
			if !ok {
				continue
			}
			out, err := s.oneShot(ctx, text, slot.arguments)
			if err != nil {
				return err
			}
			if (slot.arguments || customToolInputChannel(slot.channel) || protocolToolContentPath(slot.path)) && hasUnresolvedToolPlaceholder(out) {
				return errors.New("unresolved placeholder in tool arguments")
			}
			protocolSet(obj, slot.path, out)
		}
	case []any:
		for i, child := range obj {
			if text, ok := child.(string); ok {
				out, err := s.oneShot(ctx, text, false)
				if err != nil {
					return err
				}
				obj[i] = out
			} else if err := s.restoreJSONValue(ctx, child); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *protocolRestoreSession) oneShot(ctx context.Context, text string, arguments bool) (string, error) {
	if arguments {
		var normalizer protocolArgumentEscapes
		text = normalizer.push(text, true)
	}
	if !strings.ContainsAny(text, "{_") {
		return text, nil
	}
	session, err := s.factory(ctx)
	if err != nil {
		return "", err
	}
	defer func() {
		if a, ok := session.(AbortableRestoreSession); ok {
			a.Abort()
		}
	}()
	if c, ok := session.(ConfigurableRestoreSession); ok {
		if err := c.SetJSONEscape(ctx, arguments); err != nil {
			return "", err
		}
	}
	a, err := session.Push(ctx, []byte(text))
	if err != nil {
		return "", err
	}
	b, err := session.Finish(ctx)
	if err != nil {
		return "", err
	}
	out := string(append(a, b...))
	var inputTail, outputTail string
	s.observe(&inputTail, []byte(text), false)
	s.observe(&outputTail, []byte(out), true)
	return out, nil
}

func (s *protocolRestoreSession) encode(obj map[string]any, header string) []byte {
	b := encodeProtocolJSON(obj)
	switch s.kind {
	case "sse":
		return []byte(header + "data: " + string(b) + "\n\n")
	case "ndjson":
		return append(b, '\n')
	default:
		return b
	}
}

func (s *protocolRestoreSession) flush(ctx context.Context, which func(string) bool) ([]byte, error) {
	keys := make([]string, 0, len(s.channels))
	for key := range s.channels {
		if which(key) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	var output []byte
	for _, key := range keys {
		c := s.channels[key]
		out, err := c.finish(ctx)
		if err != nil {
			return nil, err
		}
		delete(s.channels, key)
		s.observe(&c.outputTail, out, true)
		if len(out) == 0 {
			continue
		}
		if err := c.checkArguments(out, true); err != nil {
			return nil, err
		}
		obj := cloneProtocolFrame(c.frame)
		for _, slot := range protocolSlots(protocolFrameObject(obj, c.header)) {
			if !slot.snapshot && slot.channel != "" {
				protocolSet(obj, slot.path, "")
			}
		}
		protocolSet(obj, c.path, string(out))
		if choices, ok := obj["choices"].([]any); ok {
			for _, choice := range choices {
				if m, ok := choice.(map[string]any); ok {
					delete(m, "finish_reason")
					if d, ok := m["delta"].(map[string]any); ok {
						delete(d, "role")
						if calls, ok := d["tool_calls"].([]any); ok {
							for _, call := range calls {
								if tool, ok := call.(map[string]any); ok {
									delete(tool, "id")
									delete(tool, "type")
									if f, ok := tool["function"].(map[string]any); ok {
										delete(f, "name")
									}
								}
							}
						}
						if f, ok := d["function_call"].(map[string]any); ok {
							delete(f, "name")
						}
					}
				}
			}
		}
		if _, ok := obj["done"]; ok {
			obj["done"] = false
		}
		output = append(output, s.encode(obj, c.header)...)
	}
	return output, nil
}

func protocolGet(obj any, path []any) any {
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, ok := obj.(map[string]any)
			if !ok {
				return nil
			}
			obj = m[k]
		case int:
			a, ok := obj.([]any)
			if !ok || k >= len(a) {
				return nil
			}
			obj = a[k]
		}
	}
	return obj
}
func protocolSet(obj any, path []any, value any) {
	if len(path) == 0 {
		return
	}
	parent := protocolGet(obj, path[:len(path)-1])
	switch k := path[len(path)-1].(type) {
	case string:
		if m, ok := parent.(map[string]any); ok {
			m[k] = value
		}
	case int:
		if a, ok := parent.([]any); ok && k < len(a) {
			a[k] = value
		}
	}
}
func cloneProtocolFrame(obj map[string]any) map[string]any {
	copy, _ := decodeProtocolJSON(encodeProtocolJSON(obj))
	return copy
}
func protocolIndex(value any, fallback int) string {
	switch v := value.(type) {
	case json.Number:
		if i, err := v.Int64(); err == nil && i >= 0 {
			return v.String()
		}
	case string:
		if v != "" {
			return v
		}
	}
	return fmt.Sprint(fallback)
}

// Some compatible SSE providers put the event type only in the event header.
// Use it for channel selection and completion without adding fields on the wire.
func protocolFrameObject(obj map[string]any, header string) map[string]any {
	if typ, _ := obj["type"].(string); typ != "" {
		return obj
	}
	for _, line := range strings.Split(header, "\n") {
		if strings.HasPrefix(line, "event:") {
			typ := strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			if strings.HasPrefix(typ, "response.") || typ == "content_block_start" || typ == "content_block_delta" || typ == "content_block_stop" || typ == "message_stop" {
				classified := make(map[string]any, len(obj)+1)
				for key, value := range obj {
					classified[key] = value
				}
				classified["type"] = typ
				return classified
			}
		}
	}
	return obj
}

func protocolSlots(obj map[string]any) []protocolSlot {
	var slots []protocolSlot
	covered := map[string]bool{}
	add := func(path []any, channel string, arguments, snapshot bool) {
		if covered[fmt.Sprint(path)] {
			return
		}
		if _, ok := protocolGet(obj, path).(string); ok {
			slots = append(slots, protocolSlot{path, channel, arguments, snapshot})
			covered[fmt.Sprint(path)] = true
		}
	}
	if choices, ok := obj["choices"].([]any); ok {
		for i, c := range choices {
			choice, ok := c.(map[string]any)
			if !ok {
				continue
			}
			prefix := "choice." + protocolIndex(choice["index"], i) + "."
			for _, container := range []string{"delta", "message"} {
				m, ok := choice[container].(map[string]any)
				if !ok {
					continue
				}
				base := []any{"choices", i, container}
				snapshot := container == "message"
				for _, field := range []string{"content", "reasoning_content", "reasoning", "refusal", "reasoning_details"} {
					add(append(append([]any{}, base...), field), prefix+field, false, snapshot)
					if parts, ok := m[field].([]any); ok {
						for j, value := range parts {
							part, ok := value.(map[string]any)
							if !ok {
								continue
							}
							partPrefix := prefix + field + "." + protocolIndex(part["index"], j) + "."
							for _, textField := range []string{"text", "refusal", "thinking", "summary"} {
								path := append(append([]any{}, base...), field, j, textField)
								add(path, partPrefix+textField, false, snapshot)
							}
						}
					}
				}
				if tools, ok := m["tool_calls"].([]any); ok {
					for j, t := range tools {
						tool, ok := t.(map[string]any)
						if !ok {
							continue
						}
						path := append(append([]any{}, base...), "tool_calls", j, "function", "arguments")
						add(path, prefix+"tool."+protocolIndex(tool["index"], j)+".arguments", true, snapshot)
					}
				}
				add(append(append([]any{}, base...), "function_call", "arguments"), prefix+"function.arguments", true, snapshot)
			}
			add([]any{"choices", i, "text"}, prefix+"text", false, false)
		}
	}
	typeName, _ := obj["type"].(string)
	if strings.HasPrefix(typeName, "response.") {
		channel, field, arguments := responseProtocolChannel(obj)
		add([]any{"delta"}, channel, arguments, false)
		if strings.HasSuffix(typeName, ".done") {
			add([]any{field}, channel, arguments, true)
		}
	} else if typeName == "content_block_delta" {
		prefix := "block." + protocolIndex(obj["index"], 0) + "."
		add([]any{"delta", "text"}, prefix+"text", false, false)
		add([]any{"delta", "thinking"}, prefix+"thinking", false, false)
		add([]any{"delta", "partial_json"}, prefix+"arguments", true, false)
	}
	add([]any{"response"}, "ollama.response", false, false)
	for _, field := range []string{"content", "thinking"} {
		add([]any{"message", field}, "ollama."+field, false, false)
	}
	if candidates, ok := obj["candidates"].([]any); ok {
		for i, c := range candidates {
			candidate, ok := c.(map[string]any)
			if !ok {
				continue
			}
			parts, ok := protocolGet(candidate, []any{"content", "parts"}).([]any)
			if !ok {
				continue
			}
			for j := range parts {
				add([]any{"candidates", i, "content", "parts", j, "text"}, "gemini."+protocolIndex(candidate["index"], i)+"."+fmt.Sprint(j)+".text", false, false)
			}
		}
	}
	add([]any{"text"}, "text", false, false)
	// Structured tool inputs and full response snapshots need recursive
	// restoration too. Preserve envelope identity fields outside user content.
	var walk func(any, []any)
	walk = func(v any, path []any) {
		switch x := v.(type) {
		case map[string]any:
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				walk(x[k], append(append([]any{}, path...), k))
			}
		case []any:
			for i, item := range x {
				walk(item, append(append([]any{}, path...), i))
			}
		case string:
			if !covered[fmt.Sprint(path)] && protocolContentPath(path) {
				arguments := len(path) > 0 && path[len(path)-1] == "arguments"
				add(path, "", arguments, true)
			}
		}
	}
	walk(obj, nil)
	return slots
}

func customToolInputChannel(channel string) bool {
	return strings.HasPrefix(channel, "response.") && strings.Contains(channel, ".custom_tool_call_input.")
}

func responseProtocolChannel(obj map[string]any) (string, string, bool) {
	typeName, _ := obj["type"].(string)
	kind := strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(typeName, "response."), ".delta"), ".done")
	prefix := "response." + protocolIndex(obj["output_index"], 0) + "." + protocolIndex(obj["content_index"], 0) + "."
	if summary, ok := obj["summary_index"]; ok {
		prefix += "summary." + protocolIndex(summary, 0) + "."
	}
	field := "text"
	arguments := kind == "function_call_arguments"
	if arguments {
		field = "arguments"
	} else if kind == "custom_tool_call_input" {
		field = "input"
	} else if kind == "refusal" {
		field = "refusal"
	}
	// Reasoning text and reasoning summary text can share output/content
	// indexes; event kind is part of the logical channel identity.
	return prefix + kind + "." + field, field, arguments
}

func protocolToolContentPath(path []any) bool {
	for _, p := range path {
		if p == "arguments" || p == "args" || p == "input" || p == "partial_json" {
			return true
		}
	}
	return false
}

func protocolContentPath(path []any) bool {
	if len(path) == 0 {
		return true
	}
	if protocolToolContentPath(path) {
		return true
	}
	switch path[len(path)-1] {
	case "id", "item_id", "call_id", "tool_call_id", "model", "object", "type", "role", "name", "system_fingerprint", "finish_reason", "finishReason", "stop_reason", "status", "signature":
		return false
	}
	return true
}

func protocolTerminal(obj map[string]any) func(string) bool {
	all := obj["done"] == true
	typeName, _ := obj["type"].(string)
	switch typeName {
	case "message_stop", "response.completed", "response.incomplete", "response.failed":
		all = true
	}
	var prefixes []string
	var exact string
	if strings.HasPrefix(typeName, "response.") && strings.HasSuffix(typeName, ".done") {
		exact, _, _ = responseProtocolChannel(obj)
		prefix := "response." + protocolIndex(obj["output_index"], 0) + "."
		switch typeName {
		case "response.output_item.done":
			prefixes = append(prefixes, prefix)
		case "response.content_part.done", "response.reasoning_summary_part.done":
			prefix += protocolIndex(obj["content_index"], 0) + "."
			if summary, ok := obj["summary_index"]; ok {
				prefix += "summary." + protocolIndex(summary, 0) + "."
			}
			prefixes = append(prefixes, prefix)
		}
	}
	if typeName == "content_block_stop" {
		prefixes = append(prefixes, "block."+protocolIndex(obj["index"], 0)+".")
	}
	if choices, ok := obj["choices"].([]any); ok {
		for i, c := range choices {
			if choice, ok := c.(map[string]any); ok && choice["finish_reason"] != nil && choice["finish_reason"] != "" {
				prefixes = append(prefixes, "choice."+protocolIndex(choice["index"], i)+".")
			}
		}
	}
	if candidates, ok := obj["candidates"].([]any); ok {
		for i, c := range candidates {
			if candidate, ok := c.(map[string]any); ok && candidate["finishReason"] != nil {
				prefixes = append(prefixes, "gemini."+protocolIndex(candidate["index"], i)+".")
			}
		}
	}
	return func(key string) bool {
		if all || key == exact {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(key, p) {
				return true
			}
		}
		return false
	}
}
