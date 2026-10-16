package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/silencer/gateway/internal/storage"
)

func TestLogNoiseAllowlistValidationAndFailClosed(t *testing.T) {
	valid := map[string]any{"method": "GET", "path": "/r/sub2api/api/tags", "status": 404}
	for _, value := range []any{nil, "*", map[string]any{}, []any{nil}, []any{"GET /x"}} {
		if _, err := parseLogNoiseAllowlist(value); err == nil {
			t.Fatalf("accepted malformed list: %#v", value)
		}
	}
	for key, values := range map[string][]any{
		"method":  {"get", "*", "GET ", 1},
		"path":    {"", "x", "//host/x", "/api/*", "/x?q=1", "/x#part", "/x\ny", "/x\x00", "/x\\y"},
		"status":  {200, 403, 499, 500, 404.5, "404", nil},
		"unknown": {true},
	} {
		for _, value := range values {
			item := cloneMap(valid)
			item[key] = value
			if _, err := parseLogNoiseAllowlist([]any{item}); err == nil {
				t.Fatalf("accepted %s=%#v", key, value)
			}
		}
	}
	if _, err := parseLogNoiseAllowlist([]any{valid, valid}); err == nil {
		t.Fatal("accepted duplicate rules")
	}
	s, _ := newLogTestServer(t)
	s.configMu.Lock()
	s.control["log_noise_allowlist"] = []any{valid, "broken"}
	s.configMu.Unlock()
	e := storage.Event{Type: "MASK", Status: "404", Meta: `{"method":"GET","path":"/r/sub2api/api/tags","ingress":"proxy"}`}
	if displayLogEventType(s.projectLogNoise(e), nil) != "ERR" {
		t.Fatal("malformed persisted rules suppressed an error")
	}
}

func TestLogNoiseListDetailFiltersStatsAndLegacyOutcome(t *testing.T) {
	s, store := newLogTestServer(t)
	for i, tc := range []struct {
		name, method, path, code, failure, ingress string
		allowed                                    bool
	}{
		{"tags", "GET", "/r/sub2api/api/tags", "404", "", "proxy", true},
		{"show", "POST", "/r/sub2api/api/show", "404", "", "proxy", true},
		{"wrong-method", "POST", "/r/sub2api/api/tags", "404", "", "proxy", false},
		{"wrong-path", "GET", "/r/sub2api/api/tags/extra", "404", "", "proxy", false},
		{"server-error", "GET", "/r/sub2api/api/tags", "500", "", "proxy", false},
		{"forbidden", "GET", "/r/sub2api/api/tags", "403", "", "proxy", false},
		{"cancel", "GET", "/r/sub2api/api/tags", "499", "canceled", "proxy", false},
		{"write-error", "GET", "/r/sub2api/api/tags", "404", "response_error", "proxy", false},
		{"extension", "GET", "/r/sub2api/api/tags", "404", "", "ext", false},
	} {
		at := time.Now().UTC().Add(-time.Duration(i+1) * time.Second)
		meta, _ := json.Marshal(map[string]any{"method": tc.method, "path": tc.path, "ingress": tc.ingress, "count": 1, "mask_token_count": 1, "items_complete": true, "model": "fixture-model"})
		item := storage.Request{ID: tc.name, Status: tc.code, Error: tc.failure, Meta: string(meta), StartedAt: at, EndedAt: at.Add(time.Millisecond), CreatedAt: at}
		if err := store.AppendRequest(context.Background(), item); err != nil {
			t.Fatal(err)
		}
		appendLogFixture(t, store, storage.Event{ID: tc.name + "-mask", RequestID: tc.name, Type: "MASK", Status: tc.code, Error: tc.failure, Meta: string(meta), CreatedAt: at})
		// Legacy RESTORE status and metadata did not contain the HTTP outcome.
		appendLogFixture(t, store, storage.Event{ID: tc.name + "-restore", RequestID: tc.name, Type: "RESTORE", Status: "no_placeholder_in_response", Meta: `{"restored":0}`, CreatedAt: at.Add(time.Millisecond)})
		detail := readLogJSON(t, s, "/api/logs/detail?id="+tc.name+"-restore")["event"].(map[string]any)
		if (detail["noise_whitelisted"] == true) != tc.allowed || detail["status"] != tc.code || (detail["display_type"] == "SKIP") != tc.allowed {
			t.Fatalf("wrong projection for %s: %#v", tc.name, detail)
		}
		if s.failedStatsRequest(item) == tc.allowed {
			t.Fatalf("wrong statistics for %s", tc.name)
		}
		if view := s.requestItem(item); (view["noise_whitelisted"] == true) != tc.allowed || view["status"] != tc.code {
			t.Fatalf("ledger disagrees: %v", view)
		}
	}
	for path, want := range map[string]int{"/api/logs": 18, "/api/logs?hide_noise=1": 14, "/api/logs?type=SKIP": 4, "/api/logs?type=PASS_COMBINED": 4, "/api/logs?type=ERR": 12, "/api/logs?type=CANCEL": 2} {
		if got := len(readLogJSON(t, s, path)["events"].([]any)); got != want {
			t.Fatalf("%s: got %d, want %d", path, got, want)
		}
	}
	stats := readLogJSON(t, s, "/api/stats/today")
	if stats["alerts"] != float64(7) || stats["requests"] != float64(9) {
		t.Fatalf("wrong totals: %#v", stats)
	}
	models := readLogJSON(t, s, "/api/stats/models")["models"].([]any)
	if len(models) != 1 || models[0].(map[string]any)["errors"] != float64(7) {
		t.Fatalf("wrong model errors: %#v", models)
	}
	s.configMu.Lock()
	s.control["log_noise_allowlist"] = []any{}
	s.configMu.Unlock()
	if got := len(readLogJSON(t, s, "/api/logs?hide_noise=1")["events"].([]any)); got != 18 {
		t.Fatalf("removing rules failed to reclassify history: %d", got)
	}
}

func TestLogNoiseConfigPersistenceEmptyAndRejectedPatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.json")
	if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SILENCER_CONTROL_CONFIG_FILE", path)
	s, _ := newLogTestServer(t)
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Contains(data, []byte("log_noise_allowlist")) {
		t.Fatal("upgrade defaults not persisted", err)
	}
	post := func(url, body string) int {
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, url, strings.NewReader(body)))
		return rr.Code
	}
	for _, tc := range []struct{ url, body string }{
		{"/api/config", `{"log_noise_allowlist":[{"method":"GET","path":"/*","status":404}]}`},
		{"/api/config/patch", `{"key":"log_noise_allowlist","op":"list_add","value":[{"method":"GET","path":"/x","status":500}]}`},
	} {
		if post(tc.url, tc.body) == 200 {
			t.Fatal("invalid config accepted")
		}
		after, _ := os.ReadFile(path)
		if !bytes.Equal(data, after) || len(s.logNoiseRules()) != 2 {
			t.Fatal("rejected config changed active state or disk")
		}
	}
	// Exercise the actual Settings card operations before a first restart.
	if post("/api/config/patch", `{"key":"log_noise_allowlist","op":"list_add","value":[{"method":"GET","path":"/r/sub2api/api/tags","status":404}]}`) != 200 || len(s.logNoiseRules()) != 2 {
		t.Fatal("adding an existing default should be idempotent")
	}
	if post("/api/config/patch", `{"key":"log_noise_allowlist","op":"list_remove","value":[{"method":"GET","path":"/r/sub2api/api/tags","status":404}]}`) != 200 || len(s.logNoiseRules()) != 1 {
		t.Fatal("default rule cannot be removed before restart")
	}
	if post("/api/config", `{"log_noise_allowlist":[]}`) != 200 {
		t.Fatal("empty allowlist rejected")
	}
	s2, err := NewServer(Config{Engine: "go-fallback"}, testEngine{})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Shutdown(context.Background())
	if len(s2.logNoiseRules()) != 0 {
		t.Fatal("deleted defaults resurrected on restart")
	}
}

func TestLogNoiseCannotHideUnresolvedPlaceholders(t *testing.T) {
	s, _ := newLogTestServer(t)
	for _, key := range []string{"unresolved", "restore_unresolved"} {
		meta := map[string]any{"method": "GET", "path": "/r/sub2api/api/tags", "ingress": "proxy", key: 1}
		if matchesLogNoise(s.logNoiseRules(), "404", "", meta) {
			t.Fatalf("%s suppressed", key)
		}
	}
}

func TestAllowedMaskCannotEraseRestoreFailure(t *testing.T) {
	s, store := newLogTestServer(t)
	at := time.Now().UTC()
	appendLogFixture(t, store, storage.Event{ID: "allowed-mask", RequestID: "phase-failure", Type: "MASK", Status: "404", Meta: `{"method":"GET","path":"/r/sub2api/api/tags","ingress":"proxy"}`, CreatedAt: at})
	appendLogFixture(t, store, storage.Event{ID: "failed-restore", RequestID: "phase-failure", Type: "RESTORE", Status: "error", Error: "response_error", Meta: `{}`, CreatedAt: at.Add(time.Millisecond)})
	row := readLogJSON(t, s, "/api/logs/detail?id=failed-restore")["event"].(map[string]any)
	if row["error"] != "response_error" || row["display_type"] != "ERR" || row["noise_whitelisted"] == true {
		t.Fatalf("restore failure erased: %#v", row)
	}
	if rows := readLogJSON(t, s, "/api/logs?hide_noise=1")["events"].([]any); len(rows) != 1 {
		t.Fatalf("restore failure hidden: %#v", rows)
	}
}

func TestAllowlisted404StillMasksForwardsAndReturns404(t *testing.T) {
	var received string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received = string(body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(404)
		_, _ = io.WriteString(w, `{"error":"expected probe"}`)
	}))
	defer upstream.Close()
	s, _ := newLogTestServer(t)
	s.configMu.Lock()
	s.cfg.UpstreamURL = upstream.URL
	s.running = true
	s.control["upstreams"] = []any{map[string]any{"name": "sub2api", "target": upstream.URL, "base_path": "/"}}
	s.configMu.Unlock()
	s.replaceEngine(bodyMaskEngine{})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/r/sub2api/api/show", strings.NewReader(`{"name":"secret"}`)))
	if rr.Code != 404 || received != `{"name":"masked"}` || !strings.Contains(rr.Body.String(), "expected probe") {
		t.Fatalf("forwarding changed: status=%d sent=%q body=%q", rr.Code, received, rr.Body.String())
	}
	rows := readLogJSON(t, s, "/api/logs?type=SKIP")["events"].([]any)
	if len(rows) != 2 {
		t.Fatalf("request not retained as expected noise: %#v", rows)
	}
	if len(readLogJSON(t, s, "/api/requests")["items"].([]any)) != 1 {
		t.Fatal("ledger entry was dropped")
	}
}
