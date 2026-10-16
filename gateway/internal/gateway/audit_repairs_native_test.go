//go:build silencer_engine && cgo

package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNativeDecodedJSONAndTemplateMasking(t *testing.T) {
	for _, paths := range []string{"", `,"field_paths":["/unused"]`} {
		e, err := newNativeEngine(Config{Engine: "cpp", EngineConfig: `{"rules":[{"needle":"secret","label":"SECRET"},{"needle":"alice@example.com","label":"EMAIL","regex":false}]` + paths + `}`})
		if err != nil {
			t.Fatal(err)
		}
		for _, input := range []string{`{"content":"{{secret}}"}`, `{"content":"s\u0065cret"}`, `{"content":"{{alice@example.com}}"}`, `{"content":"alice\u0040example.com"}`, `"s\u0065cret"`} {
			masked, err := e.(MaskingEngine).Mask(context.Background(), []byte(input))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(masked), "secret") || strings.Contains(string(masked), "alice") || !json.Valid(masked) || !strings.Contains(string(masked), "{{") {
				t.Fatalf("JSON bypass remains: %s", masked)
			}
			session, err := e.(StreamingMaskingEngine).NewMaskingSession(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			var stream strings.Builder
			for _, b := range []byte(input) {
				out, err := session.Push(context.Background(), []byte{b})
				if err != nil {
					t.Fatal(err)
				}
				stream.Write(out)
			}
			out, err := session.Finish(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			stream.Write(out)
			if strings.Contains(stream.String(), "secret") || strings.Contains(stream.String(), "alice") || !json.Valid([]byte(stream.String())) || !strings.Contains(stream.String(), "{{") {
				t.Fatal("streaming JSON bypass remains")
			}
			if owner, ok := session.(ContextualMaskingSession); ok {
				owner.Release()
			}
		}
		// Unmatched serialization and keys remain byte-for-byte cache compatible.
		const unchanged = `{ "s\u0065cret" : "keep\u0020unchanged", "count": 3 }`
		masked, err := e.(MaskingEngine).Mask(context.Background(), []byte(unchanged))
		if err != nil || string(masked) != unchanged {
			t.Fatal("untouched JSON bytes changed", err)
		}
		e.(CloseableEngine).Close()
	}
}
func TestNativeJSONPointerSegments(t *testing.T) {
	e, err := newNativeEngine(Config{Engine: "cpp", EngineConfig: `{"rules":[],"field_paths":["/a.b","/a~1b","/a~0b","/"]}`})
	if err != nil {
		t.Fatal(err)
	}
	defer e.(CloseableEngine).Close()
	masked, err := e.(MaskingEngine).Mask(context.Background(), []byte(`{"a.b":"one","a/b":"two","a~b":"three","":"four","a":{"b":"leave"}}`))
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err = json.Unmarshal(masked, &v); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"a.b", "a/b", "a~b", ""} {
		if !strings.HasPrefix(v[key].(string), "{{") {
			t.Fatal("pointer missed key", key)
		}
	}
	if v["a"].(map[string]any)["b"] != "leave" {
		t.Fatal("pointer masked dot hierarchy")
	}
}
func TestNativeEarlyUpstreamErrorsKeepStatusAndBody(t *testing.T) {
	for _, status := range []int{401, 413, 429} {
		e, err := newNativeEngine(Config{Engine: "cpp", EngineConfig: `{"rules":[{"needle":"secret","label":"SECRET"}]}`})
		if err != nil {
			t.Fatal(err)
		}
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Connection", "close")
			w.WriteHeader(status)
			io.WriteString(w, `{"error":"rejected"}`)
			w.(http.Flusher).Flush()
		}))
		s, err := NewServer(Config{UpstreamURL: up.URL, MaxBodyBytes: 1 << 20}, e)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest("POST", "/api/requests/v1/chat/completions", strings.NewReader(`{"content":"secret"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Expect", "100-continue")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, req)
		if w.Code != status || !strings.Contains(w.Body.String(), "rejected") {
			t.Fatalf("upstream %d became %d: %s", status, w.Code, w.Body)
		}
		s.Shutdown(context.Background())
		up.Close()
	}
}
func TestNativeSealedMappingCanRestoreAndRelease(t *testing.T) {
	e, err := newNativeEngine(Config{Engine: "cpp", EngineConfig: `{"rules":[{"needle":"secret","label":"SECRET"}]}`})
	if err != nil {
		t.Fatal(err)
	}
	defer e.(CloseableEngine).Close()
	session, err := e.(ReusableMaskingEngine).NewReusableMaskingSession(context.Background(), "seal-test")
	if err != nil {
		t.Fatal(err)
	}
	out, err := session.Push(context.Background(), []byte("secret "+strings.Repeat("x ", 100)))
	if err != nil {
		t.Fatal(err)
	}
	owner := session.(ContextualMaskingSession)
	if err = session.(interface{ Seal() error }).Seal(); err != nil {
		t.Fatal(err)
	}
	restored, err := owner.NewRestoreSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	owner.Release()
	a, err := restored.Push(context.Background(), out)
	if err != nil {
		t.Fatal(err)
	}
	b, err := restored.Finish(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(append(a, b...)), "secret") {
		t.Fatal("sealed mapping lost")
	}
}

func TestNativeNumericJSONValuesAndLegalPlaceholders(t *testing.T) {
	e, err := newNativeEngine(Config{Engine: "cpp", EngineConfig: `{"rules":[{"needle":"13800138000","label":"PHONE"},{"needle":"SECRET","label":"TERM"}]}`})
	if err != nil {
		t.Fatal(err)
	}
	defer e.(CloseableEngine).Close()
	input := `{"phone":13800138000,"token":"{{SECRET_bcdfgh}}","keep":42}`
	check := func(out []byte) {
		t.Helper()
		var v map[string]any
		if err := json.Unmarshal(out, &v); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(v["phone"].(string), "{{PHONE_") || v["token"] != "{{SECRET_bcdfgh}}" || v["keep"] != float64(42) {
			t.Fatal("number leaked or legal placeholder changed", v)
		}
	}
	out, err := e.(MaskingEngine).Mask(context.Background(), []byte(input))
	if err != nil {
		t.Fatal(err)
	}
	check(out)
	session, err := e.(StreamingMaskingEngine).NewMaskingSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer session.(ContextualMaskingSession).Release()
	var wire []byte
	for _, b := range []byte(input) {
		out, err := session.Push(context.Background(), []byte{b})
		if err != nil {
			t.Fatal(err)
		}
		wire = append(wire, out...)
	}
	out, err = session.Finish(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	check(append(wire, out...))
}
func TestNativeCustomToolInputRestoresFreeTextAndGuardsUnknown(t *testing.T) {
	e, err := newNativeEngine(Config{Engine: "cpp", EngineConfig: `{"rules":[{"needle":"a\"b\\c\nline","label":"SECRET"}]}`})
	if err != nil {
		t.Fatal(err)
	}
	defer e.(CloseableEngine).Close()
	ownerSession, err := e.(ReusableMaskingEngine).NewReusableMaskingSession(context.Background(), "custom-tool-test")
	if err != nil {
		t.Fatal(err)
	}
	owner := ownerSession.(ContextualMaskingSession)
	defer owner.Release()
	raw := "a\"b\\c\nline"
	a, err := ownerSession.Push(context.Background(), []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	b, err := ownerSession.Finish(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	token := string(append(a, b...))
	for _, mode := range []string{"delta", "done"} {
		s := newProtocolRestoreSession("sse", owner.NewRestoreSession)
		field := "delta"
		if mode == "done" {
			field = "input"
		}
		var got strings.Builder
		parts := []string{token}
		if mode == "delta" {
			parts = []string{token[:8], token[8:]}
		}
		for _, part := range parts {
			data, _ := json.Marshal(map[string]any{"type": "response.custom_tool_call_input." + mode, "output_index": 0, field: part})
			out, err := s.Push(context.Background(), append(append([]byte("data: "), data...), []byte("\n\n")...))
			if err != nil {
				t.Fatal(err)
			}
			for _, line := range strings.Split(string(out), "\n") {
				if strings.HasPrefix(line, "data: {") {
					obj, err := decodeProtocolJSON([]byte(strings.TrimPrefix(line, "data: ")))
					if err != nil {
						t.Fatal(err)
					}
					if value, ok := obj[field].(string); ok {
						got.WriteString(value)
					}
				}
			}
		}
		tail, err := s.Finish(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(tail), "\n") {
			if strings.HasPrefix(line, "data: {") {
				obj, _ := decodeProtocolJSON([]byte(strings.TrimPrefix(line, "data: ")))
				if v, ok := obj[field].(string); ok {
					got.WriteString(v)
				}
			}
		}
		if got.String() != raw {
			t.Fatalf("free text escaped incorrectly: %q", got.String())
		}
	}
	for _, terminal := range []string{"completed", "eof"} {
		s := newProtocolRestoreSession("sse", owner.NewRestoreSession)
		_, err := s.Push(context.Background(), []byte("data: {\"type\":\"response.custom_tool_call_input.delta\",\"output_index\":0,\"delta\":\"{{SECRET_zzzzzz}}\"}\n\n"))
		if err == nil {
			if terminal == "eof" {
				_, err = s.Finish(context.Background())
			} else {
				_, err = s.Push(context.Background(), []byte("data: {\"type\":\"response.completed\"}\n\n"))
			}
		}
		if err == nil {
			t.Fatal("unknown custom tool placeholder escaped guard", terminal)
		}
		s.Abort()
	}
}
