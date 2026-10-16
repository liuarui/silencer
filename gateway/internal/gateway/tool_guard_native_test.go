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

func TestNativeToolGuardAuditsAfterRestoration(t *testing.T) {
	for _, mode := range []string{"observe", "rewrite", "block"} {
		t.Run(mode, func(t *testing.T) {
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
				if !strings.HasPrefix(req["secret"], "{{JSON_") {
					t.Error("upstream received unmasked command")
					return
				}
				args, _ := json.Marshal(map[string]string{"command": req["secret"]})
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, guardWire("json", chatTool(string(args))))
			}))
			defer upstream.Close()
			s, err := NewServer(Config{UpstreamURL: upstream.URL, Engine: "cpp", MaxConcurrency: 1}, engine)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Shutdown(context.Background())
			s.control["audit"].(map[string]any)["tool_guard"] = map[string]any{"enabled": true, "mode": mode}
			ts := httptest.NewServer(s.Handler())
			defer ts.Close()
			resp, err := ts.Client().Post(ts.URL+"/api/requests/v1/chat/completions", "application/json", strings.NewReader(`{"secret":"rm -rf /srv/data"}`))
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(body), "{{JSON_") {
				t.Fatal("placeholder escaped", string(body))
			}
			if mode == "observe" {
				if !strings.Contains(string(body), `"tool_calls"`) || !strings.Contains(string(body), "rm -rf /srv/data") {
					t.Fatal("observe altered restored call", string(body))
				}
			} else if strings.Contains(string(body), `"tool_calls"`) || !strings.Contains(string(body), "请再次确认") {
				t.Fatal("dangerous restored command not intercepted", string(body))
			}
		})
	}
}
