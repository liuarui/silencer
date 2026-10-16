//go:build silencer_engine && cgo

package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/silencer/gateway/internal/storage"
)

// Each delta below is a complete protocol event, rather than an HTTP read
// chunk. Hermes concatenates tool arguments only after decoding those events.
func TestNativeHermesToolArgumentsAcrossEvents(t *testing.T) {
	for _, protocol := range []string{"openai", "anthropic", "responses", "ollama", "gemini", "json"} {
		t.Run(protocol, func(t *testing.T) {
			const original = "host=203.0.113.42; quote=\"a\"\npath=C:\\tmp"
			engine, err := newNativeEngine(Config{Engine: "cpp", EngineConfig: `{"field_paths":["secret"]}`})
			if err != nil {
				t.Fatal(err)
			}
			defer engine.(CloseableEngine).Close()
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req map[string]string
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					return
				}
				token := req["secret"]
				if !strings.HasPrefix(token, "{{JSON_") {
					t.Errorf("unmasked upstream: %q", token)
					return
				}
				args, _ := json.Marshal(map[string]string{"command": token})
				parts := []string{string(args[:15]), string(args[15:25]), string(args[25:])}
				write := func(v any, event string) {
					b, _ := json.Marshal(v)
					if protocol == "ollama" {
						_, _ = w.Write(append(b, '\n'))
					} else if protocol == "json" {
						_, _ = w.Write(b)
					} else {
						if event != "" {
							_, _ = fmt.Fprintf(w, "event: %s\n", event)
						}
						_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
					}
					w.(http.Flusher).Flush()
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if protocol == "ollama" {
					w.Header().Set("Content-Type", "application/x-ndjson")
				}
				if protocol == "json" {
					w.Header().Set("Content-Type", "application/json")
					parts = []string{string(args)}
				}
				for _, part := range parts {
					switch protocol {
					case "openai", "json":
						write(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"arguments": part}}}}}}}, "")
					case "anthropic":
						write(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": part}}, "content_block_delta")
					case "responses":
						write(map[string]any{"type": "response.function_call_arguments.delta", "output_index": 0, "delta": part}, "response.function_call_arguments.delta")
					case "ollama":
						write(map[string]any{"response": part, "done": false}, "")
					case "gemini":
						write(map[string]any{"candidates": []any{map[string]any{"index": 0, "content": map[string]any{"parts": []any{map[string]any{"text": part}}}}}}, "")
					}
				}
				if protocol == "ollama" {
					write(map[string]any{"response": "", "done": true}, "")
				} else if protocol != "json" {
					_, _ = io.WriteString(w, "data: [DONE]\n\n")
				}
			}))
			defer upstream.Close()
			server, err := NewServer(Config{Engine: "cpp", UpstreamURL: upstream.URL, MaxBodyBytes: 1 << 20}, engine)
			if err != nil {
				t.Fatal(err)
			}
			gateway := httptest.NewServer(server.Handler())
			defer gateway.Close()
			body, _ := json.Marshal(map[string]string{"secret": original})
			resp, err := gateway.Client().Post(gateway.URL+"/api/requests/v1/chat/completions", "application/json", bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			wire, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			var combined strings.Builder
			decode := func(b []byte) {
				var v map[string]any
				if err := json.Unmarshal(b, &v); err != nil {
					t.Fatalf("invalid event %q: %v", b, err)
				}
				switch protocol {
				case "openai", "json":
					c := v["choices"].([]any)[0].(map[string]any)
					if d, ok := c["delta"].(map[string]any); ok {
						if tools, ok := d["tool_calls"].([]any); ok {
							combined.WriteString(tools[0].(map[string]any)["function"].(map[string]any)["arguments"].(string))
						}
					}
				case "anthropic":
					if d, ok := v["delta"].(map[string]any); ok {
						combined.WriteString(d["partial_json"].(string))
					}
				case "responses":
					if d, ok := v["delta"].(string); ok {
						combined.WriteString(d)
					}
				case "ollama":
					combined.WriteString(v["response"].(string))
				case "gemini":
					c := v["candidates"].([]any)[0].(map[string]any)
					combined.WriteString(c["content"].(map[string]any)["parts"].([]any)[0].(map[string]any)["text"].(string))
				}
			}
			if protocol == "json" {
				decode(wire)
			} else {
				scan := bufio.NewScanner(bytes.NewReader(wire))
				for scan.Scan() {
					line := scan.Bytes()
					if protocol != "ollama" {
						if !bytes.HasPrefix(line, []byte("data: ")) {
							continue
						}
						line = line[6:]
					}
					if len(line) > 0 && string(line) != "[DONE]" {
						decode(line)
					}
				}
			}
			// Text channels carry normal text; tool channels carry JSON string
			// contents and must apply the inner escaping before outer serialization.
			if protocol == "ollama" || protocol == "gemini" {
				if !strings.Contains(combined.String(), original) {
					t.Fatalf("text not restored: %q", combined.String())
				}
				return
			}
			var result map[string]string
			if err := json.Unmarshal([]byte(combined.String()), &result); err != nil {
				t.Fatalf("invalid tool arguments: %q: %v", combined.String(), err)
			}
			if result["command"] != original {
				t.Fatalf("Hermes command=%q want=%q", result["command"], original)
			}
		})
	}
}

func TestNativeProtocolParallelChoicesToolsAndCounts(t *testing.T) {
	config, err := engineConfigFromControl(map[string]any{"builtin_rules": map[string]any{"IP_PUBLIC": true}})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := newNativeEngine(Config{Engine: "cpp", EngineConfig: string(config)})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.(CloseableEngine).Close()
	mask, err := engine.(StreamingMaskingEngine).NewMaskingSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	owner := mask.(ContextualMaskingSession)
	defer owner.Release()
	// Public example-domain fixtures: IP_PUBLIC excludes documentation ranges and common DNS.
	values := []string{"93.184.216.34", "93.184.215.14", "93.184.215.15"}
	raw, _ := json.Marshal(map[string]any{"hosts": values})
	a, err := mask.Push(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	b, err := mask.Finish(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var request struct {
		Hosts []string `json:"hosts"`
	}
	if err := json.Unmarshal(append(a, b...), &request); err != nil {
		t.Fatal(err)
	}
	for i, value := range request.Hosts {
		if value == values[i] {
			t.Fatalf("builtin IP_PUBLIC missed %s", value)
		}
	}
	// Every event contains just one array element. Index is the protocol slot,
	// not that array position; choices and tool slots are deliberately crossed.
	session := newProtocolRestoreSession("sse", owner.NewRestoreSession)
	defer session.Abort()
	var wire bytes.Buffer
	for j := 0; j < len(request.Hosts[0]); j++ {
		for i, token := range request.Hosts {
			choice, tool := i/2, i%2
			obj := map[string]any{"id": "preserve-id", "model": "preserve-model", "choices": []any{map[string]any{"index": choice, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": tool, "function": map[string]any{"arguments": token[j : j+1]}}}}}}}
			payload, _ := json.Marshal(obj)
			wire.WriteString("id: event-1\r\ndata: ")
			wire.Write(payload)
			wire.WriteString("\r\n\r\n")
		}
	}
	for choice := 0; choice < 2; choice++ {
		fmt.Fprintf(&wire, "data: {\"choices\":[{\"index\":%d,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n", choice)
	}
	wire.WriteString("data: [DONE]\n\n")
	var output []byte
	// Fragment transport boundaries too, including CRLF and UTF-8 bytes.
	for data := wire.Bytes(); len(data) > 0; {
		n := 7
		if n > len(data) {
			n = len(data)
		}
		out, err := session.Push(context.Background(), data[:n])
		if err != nil {
			t.Fatal(err)
		}
		output = append(output, out...)
		data = data[n:]
	}
	tail, err := session.Finish(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	output = append(output, tail...)
	got := map[string]string{}
	scan := bufio.NewScanner(bytes.NewReader(output))
	for scan.Scan() {
		line := strings.TrimSpace(scan.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			continue
		}
		obj, err := decodeProtocolJSON([]byte(payload))
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range obj["choices"].([]any) {
			choice := c.(map[string]any)
			d := choice["delta"].(map[string]any)
			if tools, ok := d["tool_calls"].([]any); ok {
				for _, toolValue := range tools {
					tool := toolValue.(map[string]any)
					key := protocolIndex(choice["index"], 0) + ":" + protocolIndex(tool["index"], 0)
					got[key] += tool["function"].(map[string]any)["arguments"].(string)
				}
			}
		}
	}
	for i, value := range values {
		key := fmt.Sprintf("%d:%d", i/2, i%2)
		if got[key] != value {
			t.Fatalf("channel %s=%q want=%q", key, got[key], value)
		}
	}
	counts := session.RestoreCounts()
	if counts.Total != 3 || counts.Restored != 3 || counts.Unresolved != 0 {
		t.Fatalf("live counts=%+v", counts)
	}
	if maskedTokenCount(wire.Bytes()) != 0 {
		t.Fatal("fixture must split every placeholder across events")
	}
	// Counters remain authoritative when response capture is truncated. Verify
	// the request ledger, operational event and dashboard all agree.
	t.Setenv("SILENCER_STORAGE_MODE", "sqlite")
	t.Setenv("SILENCER_SQLITE_PATH", filepath.Join(t.TempDir(), "protocol.db"))
	store, err := storage.OpenFromEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	item := storage.Request{ID: "protocol-counts", Payload: []byte("POST /v1/chat/completions"), Masked: []byte(request.Hosts[0]), Status: "running", StartedAt: now, CreatedAt: now}
	if err := store.AppendRequest(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	record := &requestRecord{store: store, item: item, maxBytes: 8, protocolCounts: &counts}
	record.finish(http.StatusOK, nil)
	stored, err := store.GetRequest(context.Background(), item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if inferredRestoreStatus(stored, true) != "restored" {
		t.Fatalf("ledger lost channel counts: %s", stored.Meta)
	}
	if value, _ := numericMetaInt(requestMeta(stored)["restore_restored"]); value != 3 {
		t.Fatalf("ledger counts=%s", stored.Meta)
	}
	server, err := NewServer(Config{}, engine)
	if err != nil {
		t.Fatal(err)
	}
	server.SetStorage(store)
	defer server.Shutdown(context.Background())
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/api/stats/today", nil))
	var stats struct {
		Restored int            `json:"restored"`
		ByStatus map[string]int `json:"restore_by_status"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &stats); err != nil {
		t.Fatal(err)
	}
	if stats.Restored != 1 || stats.ByStatus["restored"] != 1 {
		t.Fatalf("dashboard stats=%s", rr.Body.Bytes())
	}
}

func TestNativeIPResponseWithoutContentType(t *testing.T) {
	for _, responseKind := range []string{"json", "text", "sse"} {
		t.Run(responseKind, func(t *testing.T) {
			engine, err := newNativeEngine(Config{Engine: "cpp", EngineConfig: `{"rules":[{"needle":"203.0.113.42","label":"IP_PUBLIC"}]}`})
			if err != nil {
				t.Fatal(err)
			}
			defer engine.(CloseableEngine).Close()
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				token := string(maskedTokenPattern.Find(b))
				w.Header()["Content-Type"] = nil
				switch responseKind {
				case "json":
					fmt.Fprintf(w, `{"ip":"%s"}`, token)
				case "text":
					fmt.Fprintf(w, "host=%s", token)
				case "sse":
					fmt.Fprintf(w, "data: {\"text\":\"%s\"}\n\n", token)
				}
			}))
			defer upstream.Close()
			server, err := NewServer(Config{Engine: "cpp", UpstreamURL: upstream.URL, MaxBodyBytes: 4096}, engine)
			if err != nil {
				t.Fatal(err)
			}
			gw := httptest.NewServer(server.Handler())
			defer gw.Close()
			resp, err := gw.Client().Post(gw.URL+"/api/requests/test", "application/json", strings.NewReader(`{"ip":"203.0.113.42"}`))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(body, []byte("203.0.113.42")) || bytes.Contains(body, []byte("{{IP_PUBLIC_")) {
				t.Fatalf("unrestored response %q", body)
			}
		})
	}
}

func TestNativeHistoricalTokenCannotAliasNewIP(t *testing.T) {
	engine, err := newNativeEngine(Config{Engine: "cpp", EngineConfig: `{"rules":[{"needle":"203.0.113.42","label":"IP_PUBLIC"},{"needle":"198.51.100.7","label":"IP_PUBLIC"}]}`})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.(CloseableEngine).Close()
	mask := func(input string) ([]byte, ContextualMaskingSession) {
		s, err := engine.(StreamingMaskingEngine).NewMaskingSession(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		a, err := s.Push(context.Background(), []byte(input))
		if err != nil {
			t.Fatal(err)
		}
		b, err := s.Finish(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return append(a, b...), s.(ContextualMaskingSession)
	}
	old, first := mask("203.0.113.42")
	first.Release()
	forms := []string{string(old), strings.Trim(string(old), "{}"), "{" + strings.Trim(string(old), "{}") + "}", "{{ " + strings.Trim(string(old), "{}") + " }}", strings.ReplaceAll(strings.ReplaceAll(string(old), "{", `\u007b`), "}", `\u007d`)}
	for _, form := range forms {
		t.Run(form, func(t *testing.T) {
			input := form + " 198.51.100.7"
			if strings.Contains(form, `\u`) {
				input = `{"history":"` + form + `","new":"198.51.100.7"}`
			}
			_, second := mask(input)
			defer second.Release()
			s, err := second.NewRestoreSession(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			a, err := s.Push(context.Background(), old)
			if err != nil {
				t.Fatal(err)
			}
			b, err := s.Finish(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if got := string(append(a, b...)); got != string(old) {
				t.Fatalf("old token silently restored to wrong IP: %q", got)
			}
		})
	}
	// Tolerant restoration must also work for real underscore labels.
	_, last := mask("198.51.100.7")
	defer last.Release()
	items, err := last.(MaskedItemsProvider).MaskedItems()
	if err != nil {
		t.Fatal(err)
	}
	canonical := items[0]["preview"].(string)
	form := "{{CHANGED_" + canonical[strings.LastIndex(canonical, "_")+1:]
	s, err := last.NewRestoreSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.Push(context.Background(), []byte(form))
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Finish(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if string(append(a, b...)) != "198.51.100.7" {
		t.Fatalf("underscore label suffix fallback failed: %q", append(a, b...))
	}
}

func TestNativeProtocolSnapshotsMetadataAndTerminalOrdering(t *testing.T) {
	engine, err := newNativeEngine(Config{Engine: "cpp", EngineConfig: `{"rules":[{"needle":"198.51.100.7","label":"IP_PUBLIC"}]}`})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.(CloseableEngine).Close()
	token, err := engine.(MaskingEngine).Mask(context.Background(), []byte("198.51.100.7"))
	if err != nil {
		t.Fatal(err)
	}
	factory := engine.(StreamingEngine).NewRestoreSession
	t.Run("normalized-placeholder-transport-splits", func(t *testing.T) {
		bare := strings.Trim(string(token), "{}")
		for _, form := range []string{string(token), bare, "{" + bare + "}", "{{ " + strings.ToLower(bare) + " }}", `\{\{` + bare + `\}\}`, "{" + bare + "}}", "{{" + bare + "}}}", "{{" + strings.Repeat(" ", 64) + bare + strings.Repeat(" ", 64) + "}}"} {
			for cut := 1; cut < len(form); cut++ {
				s, err := factory(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				a, err := s.Push(context.Background(), []byte(form[:cut]))
				if err != nil {
					t.Fatal(err)
				}
				b, err := s.Push(context.Background(), []byte(form[cut:]))
				if err != nil {
					t.Fatal(err)
				}
				c, err := s.Finish(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if got := string(append(append(a, b...), c...)); got != "198.51.100.7" {
					t.Fatalf("form=%q cut=%d result=%q", form, cut, got)
				}
			}
			s, err := factory(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			var got []byte
			for i := range form {
				out, err := s.Push(context.Background(), []byte(form[i:i+1]))
				if err != nil {
					t.Fatal(err)
				}
				got = append(got, out...)
			}
			tail, err := s.Finish(context.Background())
			if err != nil || string(append(got, tail...)) != "198.51.100.7" {
				t.Fatalf("per-byte form %q: %q / %v", form, append(got, tail...), err)
			}
		}
		s, err := factory(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		a, _ := s.Push(context.Background(), []byte(bare))
		b, _ := s.Push(context.Background(), []byte("x"))
		c, err := s.Finish(context.Background())
		if err != nil || string(append(append(a, b...), c...)) != bare+"x" {
			t.Fatalf("extended bare word was incorrectly restored: %q %v", append(append(a, b...), c...), err)
		}
		for _, input := range []string{"x" + bare + " ", `{"command":"x` + bare + ` "}`, bare + " ", bare + " next word"} {
			for _, jsonEscape := range []bool{false, true} {
				s, err := factory(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if err := s.(ConfigurableRestoreSession).SetJSONEscape(context.Background(), jsonEscape); err != nil {
					t.Fatal(err)
				}
				var got []byte
				for i := range input {
					out, err := s.Push(context.Background(), []byte(input[i:i+1]))
					if err != nil {
						t.Fatal(err)
					}
					got = append(got, out...)
				}
				tail, err := s.Finish(context.Background())
				want := input
				if input == bare+" " {
					want = "198.51.100.7 "
				} else if input == bare+" next word" {
					want = "198.51.100.7 next word"
				}
				if err != nil || string(append(got, tail...)) != want {
					t.Fatalf("bare token word boundary/whitespace: %q -> %q want %q / %v", input, append(got, tail...), want, err)
				}
			}
		}
	})
	t.Run("metadata-and-array", func(t *testing.T) {
		obj := map[string]any{"id": string(token), "model": string(token), "type": "message", "choices": []any{map[string]any{"message": map[string]any{"content": string(token), "tool_calls": []any{map[string]any{"id": string(token), "function": map[string]any{"name": string(token), "arguments": `{"host":"` + string(token) + `"}`}}}}}}}
		wire, _ := json.Marshal([]any{obj})
		s := newProtocolRestoreSession("json", factory)
		defer s.Abort()
		head, err := s.Push(context.Background(), wire)
		if err != nil {
			t.Fatal(err)
		}
		tail, err := s.Finish(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		var result []map[string]any
		if err := json.Unmarshal(append(head, tail...), &result); err != nil {
			t.Fatal(err)
		}
		out := result[0]
		if out["id"] != string(token) || out["model"] != string(token) {
			t.Fatalf("provider metadata changed: %+v", out)
		}
		message := out["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
		if message["content"] != "198.51.100.7" {
			t.Fatalf("content=%v", message)
		}
		tool := message["tool_calls"].([]any)[0].(map[string]any)
		function := tool["function"].(map[string]any)
		if tool["id"] != string(token) || function["name"] != string(token) || !strings.Contains(function["arguments"].(string), "198.51.100.7") {
			t.Fatalf("tool=%v", tool)
		}
	})
	t.Run("responses-summary-isolation", func(t *testing.T) {
		s := newProtocolRestoreSession("sse", factory)
		defer s.Abort()
		var output []byte
		parts := []struct {
			index int
			text  string
		}{{0, string(token[:12])}, {1, "other{"}, {0, string(token[12:])}}
		for _, p := range parts {
			obj := map[string]any{"type": "response.reasoning_summary_text.delta", "output_index": 0, "summary_index": p.index, "delta": p.text}
			wire, _ := json.Marshal(obj)
			out, err := s.Push(context.Background(), []byte("data: "+string(wire)+"\n\n"))
			if err != nil {
				t.Fatal(err)
			}
			output = append(output, out...)
		}
		// A full snapshot must not drop the literal brace withheld in deltas.
		out, err := s.Push(context.Background(), []byte("data: {\"type\":\"response.reasoning_summary_text.done\",\"output_index\":0,\"summary_index\":1,\"text\":\"other{\"}\n\ndata: [DONE]\n\n"))
		if err != nil {
			t.Fatal(err)
		}
		output = append(output, out...)
		tail, err := s.Finish(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		output = append(output, tail...)
		got := map[string]string{}
		scan := bufio.NewScanner(bytes.NewReader(output))
		doneSeen := false
		summaryDoneSeen := false
		summaryDoneCount := 0
		for scan.Scan() {
			line := strings.TrimPrefix(scan.Text(), "data: ")
			if line == "[DONE]" {
				doneSeen = true
				continue
			}
			if !strings.HasPrefix(line, "{") {
				continue
			}
			obj, err := decodeProtocolJSON([]byte(line))
			if err != nil {
				t.Fatal(err)
			}
			if delta, ok := obj["delta"].(string); ok {
				if doneSeen {
					t.Fatal("delta after DONE")
				}
				if summaryDoneSeen && protocolIndex(obj["summary_index"], 0) == "1" {
					t.Fatal("summary delta after its done event")
				}
				got[protocolIndex(obj["summary_index"], 0)] += delta
			}
			if obj["type"] == "response.reasoning_summary_text.done" {
				summaryDoneSeen = true
				summaryDoneCount++
				if got["1"] != "other{" || obj["text"] != "other{" {
					t.Fatalf("done overtook pending delta: %v / %v", got, obj)
				}
			}
		}
		if got["0"] != "198.51.100.7" || got["1"] != "other{" || summaryDoneCount != 1 {
			t.Fatalf("summary streams mixed or lost: %v", got)
		}
	})
	t.Run("unknown-tool-placeholder-fails-closed", func(t *testing.T) {
		for _, form := range []string{"{{IP_PUBLIC_ffffffffffffffff}}", "{IP_PUBLIC_ffffffffffffffff}", "{{ IP_PUBLIC_ffffffffffffffff }}", `\{\{IP_PUBLIC_ffffffffffffffff\}\}`, "IP_PUBLIC_ffffffffffffffff"} {
			s := newProtocolRestoreSession("sse", factory)
			args, _ := json.Marshal(map[string]string{"command": "ssh " + form})
			obj := map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"arguments": string(args)}}}}}}}
			wire, _ := json.Marshal(obj)
			out, err := s.Push(context.Background(), []byte("data: "+string(wire)+"\n\n"))
			s.Abort()
			if err == nil || len(out) > 0 {
				t.Fatalf("unknown command passed through %q: output=%q err=%v", form, out, err)
			}
		}
	})
}

func TestNativeUnicodeToolArgumentDeltas(t *testing.T) {
	engine, err := newNativeEngine(Config{Engine: "cpp", EngineConfig: `{"rules":[{"needle":"198.51.100.7","label":"IP_PUBLIC"}]}`})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.(CloseableEngine).Close()
	token, err := engine.(MaskingEngine).Mask(context.Background(), []byte("198.51.100.7"))
	if err != nil {
		t.Fatal(err)
	}
	escape := func(text string) string {
		var b strings.Builder
		for _, c := range text {
			fmt.Fprintf(&b, `\u%04x`, c)
		}
		return b.String()
	}
	for _, protocol := range []string{"openai", "anthropic", "responses", "snapshot"} {
		for _, unknown := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/unknown=%v", protocol, unknown), func(t *testing.T) {
				value := string(token)
				if unknown {
					value = "{{IP_PUBLIC_ffffffffffffffff}}"
				}
				const literal = `\u007b "quote" C:\tmp`
				args, _ := json.Marshal(map[string]string{"command": "ssh " + value, "literal": literal})
				encoded := strings.ReplaceAll(string(args), value, escape(value))
				for cut := 1; cut < len(encoded); cut++ {
					s := newProtocolRestoreSession("sse", engine.(StreamingEngine).NewRestoreSession)
					var wire []byte
					var failure error
					parts := []string{encoded[:cut], encoded[cut:]}
					if protocol == "snapshot" {
						parts = []string{encoded}
					}
					for _, part := range parts {
						var obj map[string]any
						switch protocol {
						case "openai", "snapshot":
							container := "delta"
							if protocol == "snapshot" {
								container = "message"
							}
							obj = map[string]any{"choices": []any{map[string]any{"index": 0, container: map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"arguments": part}}}}}}}
						case "anthropic":
							obj = map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": part}}
						case "responses":
							obj = map[string]any{"type": "response.function_call_arguments.delta", "output_index": 0, "delta": part}
						}
						out, err := s.Push(context.Background(), s.encode(obj, ""))
						wire = append(wire, out...)
						if err != nil {
							failure = err
							break
						}
					}
					if failure == nil {
						out, err := s.Push(context.Background(), []byte("data: [DONE]\n\n"))
						wire = append(wire, out...)
						failure = err
					}
					s.Abort()
					if unknown {
						if failure == nil || bytes.Contains(wire, []byte("[DONE]")) {
							t.Fatalf("Unicode placeholder escaped tool guard at cut %d: %q / %v", cut, wire, failure)
						}
						continue
					}
					if failure != nil {
						t.Fatalf("cut %d: %v", cut, failure)
					}
					var combined strings.Builder
					for _, line := range strings.Split(string(wire), "\n") {
						if !strings.HasPrefix(line, "data: {") {
							continue
						}
						obj, err := decodeProtocolJSON([]byte(strings.TrimPrefix(line, "data: ")))
						if err != nil {
							t.Fatal(err)
						}
						for _, slot := range protocolSlots(obj) {
							if slot.arguments {
								combined.WriteString(protocolGet(obj, slot.path).(string))
							}
						}
					}
					var got map[string]string
					if err := json.Unmarshal([]byte(combined.String()), &got); err != nil || got["command"] != "ssh 198.51.100.7" || got["literal"] != literal {
						t.Fatalf("cut %d corrupted tool input %q / %v", cut, combined.String(), err)
					}
					if protocol == "snapshot" {
						break
					}
				}
			})
		}
	}
}

func TestNativeEscapedWhitespaceToolGuard(t *testing.T) {
	engine, err := newNativeEngine(Config{Engine: "cpp", EngineConfig: `{"rules":[]}`})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.(CloseableEngine).Close()
	for _, space := range []string{`\u0009`, `\t`, `\n`, `\r`, `\u0020`} {
		input := `{"command":"ssh {{IP_PUBLIC_` + space + `ffffffffffffffff}}","tail":"` + strings.Repeat("x", 1024) + `"}`
		for _, snapshot := range []bool{false, true} {
			for cut := 1; cut < 100; cut++ {
				s := newProtocolRestoreSession("sse", engine.(StreamingEngine).NewRestoreSession)
				var failure error
				parts := []string{input[:cut], input[cut:]}
				if snapshot {
					parts = []string{input}
				}
				for _, part := range parts {
					container := "delta"
					if snapshot {
						container = "message"
					}
					obj := map[string]any{"choices": []any{map[string]any{"index": 0, container: map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"arguments": part}}}}}}}
					_, failure = s.Push(context.Background(), s.encode(obj, ""))
					if failure != nil {
						break
					}
				}
				if failure == nil {
					_, failure = s.Push(context.Background(), []byte("data: [DONE]\n\n"))
				}
				s.Abort()
				if failure == nil {
					t.Fatalf("escaped whitespace %q bypassed guard at cut %d, snapshot=%v", space, cut, snapshot)
				}
				if snapshot {
					break
				}
			}
		}
	}
}

func FuzzNativeToolArgumentRestoration(f *testing.F) {
	f.Add(uint8(0), []byte{1})
	f.Add(uint8(1), []byte{1, 5, 7, 31})
	f.Add(uint8(2), []byte{0, 0, 0})
	engine, err := newNativeEngine(Config{Engine: "cpp", EngineConfig: `{"rules":[{"needle":"198.51.100.7","label":"IP_PUBLIC"}]}`})
	if err != nil {
		f.Fatal(err)
	}
	defer engine.(CloseableEngine).Close()
	token, err := engine.(MaskingEngine).Mask(context.Background(), []byte("198.51.100.7"))
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, encoding uint8, layout []byte) {
		if len(layout) == 0 || len(layout) > 512 {
			t.Skip()
		}
		var placeholder strings.Builder
		for i, c := range string(token) {
			if (i+int(encoding))%3 == 0 {
				fmt.Fprintf(&placeholder, `\u%04x`, c)
			} else {
				placeholder.WriteRune(c)
			}
		}
		const suffix = " \"quoted\"\nC:\\tmp"
		args, _ := json.Marshal(map[string]string{"command": "ssh " + string(token) + suffix})
		input := strings.ReplaceAll(string(args), string(token), placeholder.String())
		s := newProtocolRestoreSession("sse", engine.(StreamingEngine).NewRestoreSession)
		defer s.Abort()
		var output []byte
		for offset, index := 0, 0; offset < len(input); index++ {
			size := 1 + int(layout[index%len(layout)])%32
			if offset+size > len(input) {
				size = len(input) - offset
			}
			obj := map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"arguments": input[offset : offset+size]}}}}}}}
			wire := s.encode(obj, "")
			// Split both the inner JSON delta and the HTTP transport bytes.
			cut := 1 + int(layout[index%len(layout)])%(len(wire)-1)
			for _, chunk := range [][]byte{wire[:cut], wire[cut:]} {
				out, err := s.Push(context.Background(), chunk)
				if err != nil {
					t.Fatal(err)
				}
				output = append(output, out...)
			}
			offset += size
		}
		tail, err := s.Push(context.Background(), []byte("data: [DONE]\n\n"))
		if err != nil {
			t.Fatal(err)
		}
		output = append(output, tail...)
		var combined strings.Builder
		for _, line := range strings.Split(string(output), "\n") {
			if !strings.HasPrefix(line, "data: {") {
				continue
			}
			obj, err := decodeProtocolJSON([]byte(strings.TrimPrefix(line, "data: ")))
			if err != nil {
				t.Fatal(err)
			}
			for _, slot := range protocolSlots(obj) {
				if slot.arguments {
					combined.WriteString(protocolGet(obj, slot.path).(string))
				}
			}
		}
		var got map[string]string
		if err := json.Unmarshal([]byte(combined.String()), &got); err != nil || got["command"] != "ssh 198.51.100.7"+suffix {
			t.Fatalf("corrupted tool arguments: %q / %v", combined.String(), err)
		}
	})
}
