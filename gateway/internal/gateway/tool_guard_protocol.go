package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

type guardedTool struct {
	name, args string
	custom     bool
}
type toolGuardFrame struct {
	raw    []byte
	obj    map[string]any
	header string
}

// All wire paths that can carry executable tool input are collected, including
// repeated final snapshots. A safe snapshot never erases an unsafe delta.
type toolCollector struct {
	calls     map[string]*guardedTool
	snapshots []guardedTool
	seen      bool
	bytes     int
	arguments map[string]*strings.Builder
	names     map[string]map[string]bool
	invalid   bool
}

func (c *toolCollector) add(key, name string, args any, snapshot, custom bool) {
	c.seen = true
	if c.calls == nil {
		c.calls = map[string]*guardedTool{}
		c.arguments = map[string]*strings.Builder{}
		c.names = map[string]map[string]bool{}
	}
	call := c.calls[key]
	if call == nil {
		call = &guardedTool{}
		c.calls[key] = call
	}
	if name != "" {
		if len(name) > 256 || len(call.name) > 256 || len(c.names[key]) >= 16 {
			c.invalid = true
			return
		}
		c.bytes += len(name)
		if c.names[key] == nil {
			c.names[key] = map[string]bool{}
		}
		c.names[key][name] = true
		if call.name == "" {
			call.name = name
		} else if !snapshot && call.name != name && !strings.HasSuffix(call.name, name) {
			call.name += name
		}
		c.names[key][call.name] = true
	}
	call.custom = call.custom || custom
	if args == nil {
		return
	}
	text, ok := args.(string)
	if !ok {
		data, _ := json.Marshal(args)
		text = string(data)
	}
	c.bytes += len(text)
	if snapshot {
		if text != "" {
			snapshotName := name
			if snapshotName == "" {
				snapshotName = call.name
			}
			c.snapshots = append(c.snapshots, guardedTool{snapshotName, text, custom})
		}
	} else {
		if c.arguments[key] == nil {
			c.arguments[key] = &strings.Builder{}
		}
		c.arguments[key].WriteString(text)
	}
}

func (c *toolCollector) collect(obj map[string]any) {
	if choices, ok := obj["choices"].([]any); ok {
		for i, raw := range choices {
			choice, _ := raw.(map[string]any)
			prefix := "choice." + protocolIndex(choice["index"], i)
			for _, field := range []string{"delta", "message"} {
				m, _ := choice[field].(map[string]any)
				if tools, ok := m["tool_calls"].([]any); ok {
					for j, raw := range tools {
						tool, _ := raw.(map[string]any)
						f, _ := tool["function"].(map[string]any)
						name, _ := f["name"].(string)
						c.add(prefix+".tool."+protocolIndex(tool["index"], j), name, f["arguments"], field == "message", false)
					}
				}
				if f, ok := m["function_call"].(map[string]any); ok {
					name, _ := f["name"].(string)
					c.add(prefix+".legacy", name, f["arguments"], field == "message", false)
				}
			}
		}
	}
	typ, _ := obj["type"].(string)
	if typ == "content_block_start" {
		block, _ := obj["content_block"].(map[string]any)
		if block["type"] == "tool_use" || block["type"] == "server_tool_use" {
			name, _ := block["name"].(string)
			c.add("block."+protocolIndex(obj["index"], 0), name, block["input"], true, false)
		}
	}
	if typ == "content_block_delta" {
		delta, _ := obj["delta"].(map[string]any)
		if args, ok := delta["partial_json"].(string); ok {
			c.add("block."+protocolIndex(obj["index"], 0), "", args, false, false)
		}
	}
	if strings.HasPrefix(typ, "response.") {
		key := "response." + protocolIndex(obj["output_index"], 0)
		if typ == "response.output_item.added" || typ == "response.output_item.done" {
			if item, ok := obj["item"].(map[string]any); ok {
				c.collectItem(key, item)
			}
		}
		if strings.Contains(typ, "function_call_arguments.") || strings.Contains(typ, "custom_tool_call_input.") {
			custom := strings.Contains(typ, "custom_tool_call_input.")
			field := "arguments"
			if custom {
				field = "input"
			}
			args := obj["delta"]
			snapshot := strings.HasSuffix(typ, ".done")
			if snapshot {
				args = obj[field]
			}
			c.add(key, "", args, snapshot, custom)
		}
	}
	if message, ok := obj["message"].(map[string]any); ok {
		if tools, ok := message["tool_calls"].([]any); ok {
			for i, raw := range tools {
				tool, _ := raw.(map[string]any)
				f, _ := tool["function"].(map[string]any)
				name, _ := f["name"].(string)
				c.add(fmt.Sprint("ollama.", i), name, f["arguments"], true, false)
			}
		}
		if content, ok := message["content"].([]any); ok {
			for i, raw := range content {
				if item, ok := raw.(map[string]any); ok {
					c.collectItem(fmt.Sprint("message.content.", i), item)
				}
			}
		}
	}
	if parts, ok := obj["content"].([]any); ok {
		for i, raw := range parts {
			if part, ok := raw.(map[string]any); ok {
				c.collectItem(fmt.Sprint("content.", i), part)
			}
		}
	}
	if output, ok := obj["output"].([]any); ok {
		for i, raw := range output {
			if item, ok := raw.(map[string]any); ok {
				c.collectItem(fmt.Sprint("response.", i), item)
			}
		}
	}
	if response, ok := obj["response"].(map[string]any); ok {
		c.collect(response)
	}
	if candidates, ok := obj["candidates"].([]any); ok {
		for i, raw := range candidates {
			candidate, _ := raw.(map[string]any)
			parts, _ := protocolGet(candidate, []any{"content", "parts"}).([]any)
			for j, raw := range parts {
				part, _ := raw.(map[string]any)
				if f, ok := part["functionCall"].(map[string]any); ok {
					name, _ := f["name"].(string)
					c.add(fmt.Sprint("gemini.", i, ".", j), name, f["args"], true, false)
				}
			}
		}
	}
}

func (c *toolCollector) collectItem(key string, item map[string]any) {
	name, _ := item["name"].(string)
	switch item["type"] {
	case "function_call":
		c.add(key, name, item["arguments"], true, false)
	case "custom_tool_call":
		c.add(key, name, item["input"], true, true)
	case "tool_use", "server_tool_use":
		c.add(key, name, item["input"], true, false)
	}
}

func (c *toolCollector) findings() []toolFinding {
	var findings []toolFinding
	if c.invalid {
		findings = append(findings, toolFinding{"inspection_failed", "工具调用标识异常", ""})
	}
	seen := map[string]bool{}
	inspect := func(call guardedTool) {
		if call.args == "" {
			call.args = "{}"
		}
		for _, f := range inspectTool(call.name, call.args, call.custom) {
			key := f.rule + "\x00" + f.command
			if !seen[key] && len(findings) < 32 {
				findings = append(findings, f)
				seen[key] = true
			}
		}
	}
	keys := make([]string, 0, len(c.calls))
	for key := range c.calls {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		call := c.calls[key]
		if b := c.arguments[key]; b != nil {
			call.args = b.String()
		}
		inspect(*call)
		for name := range c.names[key] {
			variant := *call
			variant.name = name
			inspect(variant)
		}
	}
	for _, call := range c.snapshots {
		inspect(call)
	}
	return findings
}

type toolGuardSession struct {
	inner                        RestoreSession
	kind, mode                   string
	pending, held                []byte
	frames                       []toolGuardFrame
	collector                    toolCollector
	holding, decided, terminated bool
	findings                     []toolFinding
	onDecision                   func([]toolFinding, string)
	lastSequence                 int
	prefixItems                  map[int]any
	prefixBytes                  int
	prefixOverflow               bool
}

func newToolGuardSession(inner RestoreSession, kind, mode string, callback func([]toolFinding, string)) *toolGuardSession {
	return &toolGuardSession{inner: inner, kind: kind, mode: mode, onDecision: callback, lastSequence: -1, prefixItems: map[int]any{}}
}
func (s *toolGuardSession) StreamComplete() bool {
	if s.terminated {
		return true
	}
	t, ok := s.inner.(interface{ StreamComplete() bool })
	return ok && t.StreamComplete()
}
func (s *toolGuardSession) RestoreCounts() restoreCounts {
	if counted, ok := s.inner.(countedRestoreSession); ok {
		return counted.RestoreCounts()
	}
	return restoreCounts{}
}
func (s *toolGuardSession) Abort() {
	if a, ok := s.inner.(AbortableRestoreSession); ok {
		a.Abort()
	}
	s.pending = nil
	s.held = nil
	s.frames = nil
}
func (s *toolGuardSession) Push(ctx context.Context, data []byte) ([]byte, error) {
	out, err := s.inner.Push(ctx, data)
	if err != nil {
		return nil, err
	}
	return s.process(out, false)
}
func (s *toolGuardSession) Finish(ctx context.Context) ([]byte, error) {
	if s.terminated {
		return nil, nil
	}
	out, err := s.inner.Finish(ctx)
	if err != nil {
		return nil, err
	}
	return s.process(out, true)
}

func decodeToolGuardFrame(kind string, raw []byte) (map[string]any, string, error) {
	payload := bytes.TrimSpace(raw)
	header := ""
	if kind == "sse" {
		var data []string
		for _, line := range strings.Split(strings.ReplaceAll(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\r", "\n"), "\n") {
			if line == "data" || strings.HasPrefix(line, "data:") {
				data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			} else if line != "" {
				header += line + "\n"
			}
		}
		payload = []byte(strings.Join(data, "\n"))
	}
	if len(payload) == 0 || string(payload) == "[DONE]" {
		return nil, header, nil
	}
	obj, err := decodeProtocolJSON(payload)
	if err != nil {
		return nil, header, err
	}
	return protocolFrameObject(obj, header), header, nil
}

func (s *toolGuardSession) process(data []byte, final bool) ([]byte, error) {
	if s.decided {
		return data, nil
	}
	s.pending = append(s.pending, data...)
	var output []byte
	for len(s.pending) > 0 {
		parser := protocolRestoreSession{kind: s.kind, pending: s.pending}
		end := parser.frameEnd()
		if s.kind == "json" || end == 0 {
			if !final {
				break
			}
			end = len(s.pending)
		}
		raw := append([]byte(nil), s.pending[:end]...)
		s.pending = s.pending[end:]
		obj, header, err := decodeToolGuardFrame(s.kind, raw)
		if err != nil {
			if s.mode != "observe" && (s.holding || toolEnvelopeHint(raw)) {
				return s.stopForInspection("工具响应格式无法验证"), nil
			}
		} else if obj != nil {
			s.collector.collect(obj)
		}
		if s.collector.seen {
			s.holding = true
		}
		if s.holding && s.mode != "observe" {
			s.held = append(s.held, raw...)
			s.frames = append(s.frames, toolGuardFrame{raw, obj, header})
		} else {
			output = append(output, raw...)
			if s.mode != "observe" && obj != nil {
				if n, ok := numericMetaInt(obj["sequence_number"]); ok {
					s.lastSequence = n
				}
				if obj["type"] == "response.output_item.done" && !s.prefixOverflow {
					if index, ok := numericMetaInt(obj["output_index"]); ok && index >= 0 && index < 128 {
						s.prefixBytes += len(raw)
						if s.prefixBytes <= maxRestoreFrameBytes {
							s.prefixItems[index] = obj["item"]
						} else {
							s.prefixOverflow = true
							s.prefixItems = nil
						}
					}
				}
			}
		}
		if len(s.held) > maxRestoreFrameBytes || len(s.pending) > maxRestoreFrameBytes || s.collector.bytes > maxRestoreFrameBytes || len(s.collector.calls) > 128 || len(s.collector.snapshots) > 512 || len(s.frames) > 8192 {
			if s.mode != "observe" {
				return append(output, s.stopForInspection("工具响应超过安全检查上限")...), nil
			}
			s.report([]toolFinding{{"inspection_limit", "工具响应超过安全检查上限", ""}})
			s.decided = true
			s.collector = toolCollector{}
			output = append(output, s.pending...)
			s.pending = nil
			return output, nil
		}
	}
	if final {
		s.findings = s.collector.findings()
		s.decided = true
		if s.holding && s.mode != "observe" {
			if len(s.findings) > 0 {
				output = append(output, s.replacement()...)
			} else {
				output = append(output, s.held...)
			}
		}
		s.report(s.findings)
		s.held = nil
		s.frames = nil
		s.collector = toolCollector{}
	}
	return output, nil
}
func (s *toolGuardSession) report(findings []toolFinding) {
	if len(findings) > 0 && s.onDecision != nil {
		s.onDecision(findings, s.mode)
	}
}
func (s *toolGuardSession) stopForInspection(reason string) []byte {
	s.findings = []toolFinding{{"inspection_failed", reason, ""}}
	s.mode = "block"
	s.report(s.findings)
	s.terminated = true
	s.decided = true
	out := s.replacement()
	s.Abort()
	return out
}

func encodeGuardFrame(kind, header string, obj map[string]any) []byte {
	data := encodeProtocolJSON(obj)
	if kind == "sse" {
		return []byte(header + "data: " + string(data) + "\n\n")
	}
	if kind == "ndjson" {
		return append(data, '\n')
	}
	return data
}

func (s *toolGuardSession) replacement() []byte {
	notice := toolGuardNotice(s.findings)
	if s.mode == "block" {
		obj := map[string]any{"type": "error", "error": map[string]any{"type": "silencer_tool_blocked", "code": "silencer_tool_blocked", "message": notice}, "confirmation_required": true}
		header := ""
		if s.kind == "sse" {
			header = "event: error\n"
		}
		return encodeGuardFrame(s.kind, header, obj)
	}
	out, err := rewriteGuardFrames(s.kind, s.frames, notice, s.prefixItems, s.lastSequence)
	if err == nil {
		return out
	}
	// Unsupported or incomplete tool envelopes cannot be safely rewritten.
	s.mode = "block"
	return s.replacement()
}

var errUnknownToolProtocol = errors.New("unsupported tool response protocol")

func toolEnvelopeHint(raw []byte) bool {
	for _, token := range []string{`"tool_calls"`, `"function_call"`, `"tool_use"`, `"functionCall"`, `function_call_arguments`, `custom_tool_call`} {
		if bytes.Contains(raw, []byte(token)) {
			return true
		}
	}
	return false
}
