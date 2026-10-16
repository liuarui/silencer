//go:build silencer_engine && cgo

package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestBuiltinRuleUpdatesPreserveInventory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.json")
	t.Setenv("SILENCER_CONTROL_CONFIG_FILE", path)
	newServer := func() *Server {
		t.Helper()
		s, err := NewServer(Config{Engine: "cpp"}, testEngine{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
		return s
	}
	s := newServer()
	want := defaultBuiltinRules()
	check := func(control map[string]any) {
		t.Helper()
		if !reflect.DeepEqual(control["builtin_rules"], want) {
			t.Fatalf("rule inventory = %#v, want %#v", control["builtin_rules"], want)
		}
	}
	getConfig := func(server *Server) map[string]any {
		t.Helper()
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/config", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET config: status=%d body=%s", rec.Code, rec.Body.String())
		}
		var control map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &control); err != nil {
			t.Fatal(err)
		}
		return control
	}
	for _, changes := range []map[string]any{
		{"EMAIL": false},
		{"PHONE": false},
		{"EMAIL": true},
		{"COOKIE_SESSION": true},
		{"BASIC_AUTH": true, "URL_QUERY_SECRET": true},
		{"COOKIE_SESSION": false, "BASIC_AUTH": false, "URL_QUERY_SECRET": false},
	} {
		body, err := json.Marshal(changes)
		if err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/config/builtin_rules", strings.NewReader(string(body))))
		if rec.Code != http.StatusOK {
			t.Fatalf("update %s: status=%d body=%s", body, rec.Code, rec.Body.String())
		}
		var response struct {
			OK     bool           `json:"ok"`
			Config map[string]any `json:"config"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if !response.OK {
			t.Fatalf("update failed: %s", rec.Body.String())
		}
		for name, value := range changes {
			want[name] = value
		}
		check(response.Config)
		check(getConfig(s))
		persisted, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var control map[string]any
		if err := json.Unmarshal(persisted, &control); err != nil {
			t.Fatal(err)
		}
		check(control)
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	check(getConfig(newServer()))
}

func TestSparseBuiltinRuleConfigRestoresInventoryWithoutEnablingRules(t *testing.T) {
	for _, fixture := range []string{
		`{"builtin_rules":{"EMAIL":false}}`,
		`{"builtin_rules":{"EMAIL":true}}`,
		`{"builtin_rules":{}}`,
	} {
		t.Run(fixture, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "control.json")
			t.Setenv("SILENCER_CONTROL_CONFIG_FILE", path)
			var partial map[string]any
			if err := json.Unmarshal([]byte(fixture), &partial); err != nil {
				t.Fatal(err)
			}
			// Keep every other field complete so only the sparse rule inventory
			// can trigger the startup migration and its persistence.
			initial := defaultControlConfig()
			initial["ext_token"] = "existing-extension-test-token"
			initial["builtin_rules"] = partial["builtin_rules"]
			body, err := json.Marshal(initial)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, body, 0600); err != nil {
				t.Fatal(err)
			}
			s, err := NewServer(Config{Engine: "cpp"}, testEngine{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/config", nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("GET config: status=%d body=%s", rec.Code, rec.Body.String())
			}
			persisted, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, body := range [][]byte{rec.Body.Bytes(), persisted} {
				var control struct {
					Rules map[string]bool `json:"builtin_rules"`
				}
				if err := json.Unmarshal(body, &control); err != nil {
					t.Fatal(err)
				}
				want := defaultBuiltinRules()
				if len(control.Rules) != len(want) {
					t.Fatalf("rule count=%d, want %d", len(control.Rules), len(want))
				}
				for name := range want {
					enabled, present := control.Rules[name]
					wantEnabled := name == "EMAIL" && strings.Contains(fixture, "true")
					if !present || enabled != wantEnabled {
						t.Fatalf("rule %s=%v (present=%v), want %v", name, enabled, present, wantEnabled)
					}
				}
			}
		})
	}
}
