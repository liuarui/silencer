//go:build silencer_engine && cgo

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNativeHermesIPRoundTrip(t *testing.T) {
	engine, err := newNativeEngine(Config{Engine: "cpp", EngineConfig: `{"rules":[{"needle":"203.0.113.42","label":"IP_PUBLIC"},{"needle":"198.51.100.7","label":"IP_PUBLIC"}]}`})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.(CloseableEngine).Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		var req struct {
			Messages []struct{ Role, Content string }
		}
		if json.Unmarshal(body, &req) != nil || len(req.Messages) != 2 || req.Messages[0].Content != "Hermes instructions" || req.Messages[0].Role != "system" {
			t.Errorf("Hermes instructions changed: %s", body)
			return
		}
		if bytes.Contains(body, []byte("203.0.113.42")) || bytes.Contains(body, []byte("198.51.100.7")) {
			t.Error("original IP forwarded upstream")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		// Copy each opaque reference exactly, split across separate events.
		for _, char := range req.Messages[1].Content {
			frame, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]string{"content": string(char)}}}})
			_, _ = fmt.Fprintf(w, "data: %s\n\n", frame)
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()
	server, err := NewServer(Config{Engine: "cpp", UpstreamURL: upstream.URL, MaxBodyBytes: 1 << 20}, engine)
	if err != nil {
		t.Fatal(err)
	}
	gateway := httptest.NewServer(server.Handler())
	defer gateway.Close()
	const text = "203.0.113.42 and 198.51.100.7; again 203.0.113.42"
	body, _ := json.Marshal(map[string]any{"messages": []any{map[string]string{"role": "system", "content": "Hermes instructions"}, map[string]string{"role": "user", "content": text}}})
	resp, err := gateway.Client().Post(gateway.URL+"/api/requests/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	wire, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("request failed: status=%d err=%v", resp.StatusCode, err)
	}
	var restored strings.Builder
	for _, line := range bytes.Split(wire, []byte("\n")) {
		if !bytes.HasPrefix(line, []byte("data: {")) {
			continue
		}
		obj, err := decodeProtocolJSON(bytes.TrimPrefix(line, []byte("data: ")))
		if err != nil {
			t.Fatal(err)
		}
		value, _ := protocolGet(obj, []any{"choices", 0, "delta", "content"}).(string)
		restored.WriteString(value)
	}
	if restored.String() != text {
		t.Fatalf("Hermes received %q, want %q", restored.String(), text)
	}
}

func TestNativeHermesAdditionalDeltaChannels(t *testing.T) {
	engine, err := newNativeEngine(Config{Engine: "cpp", EngineConfig: `{"rules":[{"needle":"203.0.113.42","label":"IP_PUBLIC"}]}`})
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
	a, err := mask.Push(context.Background(), []byte("203.0.113.42"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := mask.Finish(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	token := string(append(a, b...))
	for _, field := range []string{"refusal", "content", "reasoning_details", "responses-header"} {
		t.Run(field, func(t *testing.T) {
			s := newProtocolRestoreSession("sse", owner.NewRestoreSession)
			defer s.Abort()
			var output []byte
			for _, part := range []string{token[:12], token[12:] + "{"} {
				header := ""
				var obj map[string]any
				if field == "responses-header" {
					header = "event: response.output_text.delta\n"
					obj = map[string]any{"output_index": 0, "delta": part}
				} else {
					var value any = part
					if field != "refusal" {
						value = []any{map[string]any{"index": 4, "type": "text", "text": part}}
					}
					obj = map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{field: value}}}}
				}
				frame, _ := json.Marshal(obj)
				out, err := s.Push(context.Background(), []byte(header+"data: "+string(frame)+"\n\n"))
				if err != nil {
					t.Fatal(err)
				}
				output = append(output, out...)
			}
			terminal := "data: [DONE]\n\n"
			if field == "responses-header" {
				terminal = "event: response.completed\ndata: {}\n\n"
			}
			out, err := s.Push(context.Background(), []byte(terminal))
			if err != nil {
				t.Fatal(err)
			}
			output = append(output, out...)
			if len(s.channels) != 0 {
				t.Fatal("completion marker did not flush pending response text")
			}
			out, err = s.Finish(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			output = append(output, out...)
			var result strings.Builder
			for _, line := range bytes.Split(output, []byte("\n")) {
				if !bytes.HasPrefix(line, []byte("data: {")) {
					continue
				}
				obj, err := decodeProtocolJSON(bytes.TrimPrefix(line, []byte("data: ")))
				if err != nil {
					t.Fatal(err)
				}
				path := []any{"choices", 0, "delta", field}
				if field == "responses-header" {
					path = []any{"delta"}
					if _, added := obj["type"]; added {
						t.Fatal("event type was added to provider payload")
					}
				} else if field != "refusal" {
					path = append(path, 0, "text")
				}
				value, _ := protocolGet(obj, path).(string)
				result.WriteString(value)
			}
			if result.String() != "203.0.113.42{" || s.RestoreCounts().Unresolved != 0 {
				t.Fatalf("split %s was not restored: %q", field, result.String())
			}
		})
	}
}
