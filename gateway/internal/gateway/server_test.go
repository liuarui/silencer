package gateway

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/silencer/gateway/internal/storage"
)

func TestHealthReadyAndConfig(t *testing.T) {
	s, err := NewServer(Config{ListenAddr: ":0", MaxConcurrency: 2, Engine: "go-fallback"}, testEngine{})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	for _, path := range []string{"/healthz", "/readyz", "/api/config"} {
		resp, err := ts.Client().Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d", path, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

func TestUpstreamTestUsesConfiguredTargetAndChatPayload(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	defer upstream.Close()

	s, err := NewServer(Config{MaxConcurrency: 1, Engine: "go-fallback"}, testEngine{})
	if err != nil {
		t.Fatal(err)
	}
	s.configMu.Lock()
	s.control["upstreams"] = []any{map[string]any{"name": "demo", "target": upstream.URL, "base_path": "/v1"}}
	s.configMu.Unlock()
	tsrv := httptest.NewServer(s.Handler())
	defer tsrv.Close()

	req, err := http.NewRequest(http.MethodPost, tsrv.URL+"/api/upstream/test", strings.NewReader(`{"name":"demo","mode":"chat","api_key":"test-key","model":"demo-model","content":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := tsrv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if gotPath != "/v1/chat/completions" || gotAuth != "Bearer test-key" {
		t.Fatalf("path=%q auth=%q", gotPath, gotAuth)
	}
	if !strings.Contains(gotBody, `"model":"demo-model"`) || !strings.Contains(gotBody, `"content":"hello"`) {
		t.Fatalf("body=%q", gotBody)
	}
}

func TestProxyDoesNotLeakGatewayCredentialsToUpstream(t *testing.T) {
	type seenHeaders struct {
		silencer string
		auth     string
		cookie   string
		apiKey   string
	}
	seen := make(chan seenHeaders, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- seenHeaders{silencer: r.Header.Get("X-Silencer-Token"), auth: r.Header.Get("Authorization"), cookie: r.Header.Get("Cookie"), apiKey: r.Header.Get("X-Api-Key")}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	s, err := NewServer(Config{AuthToken: "gateway-secret", UpstreamURL: upstream.URL, Engine: "go-fallback"}, testEngine{})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	request, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/requests/v1/models", nil)
	request.Header.Set("X-Silencer-Token", "gateway-secret")
	request.Header.Set("Authorization", "Bearer upstream-key")
	request.Header.Set("Cookie", "provider_session=keep; silencer_session=gateway-secret; preference=dark")
	request.Header.Set("X-Api-Key", "provider-key")
	response, err := ts.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("upstream-key request status=%d", response.StatusCode)
	}

	bearerRequest, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/requests/v1/models", nil)
	bearerRequest.Header.Set("Authorization", "Bearer gateway-secret")
	bearerRequest.Header.Set("X-Silencer-Token", "gateway-secret")
	bearerRequest.Header.Set("Cookie", "silencer_session=gateway-secret")
	bearerResponse, err := ts.Client().Do(bearerRequest)
	if err != nil {
		t.Fatal(err)
	}
	bearerResponse.Body.Close()
	if bearerResponse.StatusCode != http.StatusOK {
		t.Fatalf("gateway bearer request status=%d", bearerResponse.StatusCode)
	}

	first := <-seen
	second := <-seen
	if first.silencer != "" || first.auth != "Bearer upstream-key" || first.cookie != "provider_session=keep; preference=dark" || first.apiKey != "provider-key" {
		t.Fatalf("gateway header leaked or upstream auth lost: %#v", first)
	}
	if second.silencer != "" || second.auth != "Bearer gateway-secret" || second.cookie != "" {
		t.Fatalf("provider authorization changed or management credentials leaked: %#v", second)
	}
}

func TestManagementTokenProtectsConsoleWithoutBlockingProviderCredentials(t *testing.T) {
	var forwarded atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer provider-key" {
			t.Errorf("provider credentials changed: %q", r.Header.Get("Authorization"))
		}
		forwarded.Add(1)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()
	t.Setenv("SILENCER_STORAGE_MODE", "sqlite")
	t.Setenv("SILENCER_SQLITE_PATH", filepath.Join(t.TempDir(), "auth.db"))
	store, err := storage.OpenFromEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s, err := NewServer(Config{AuthToken: "management-secret", UpstreamURL: upstream.URL, Engine: "go-fallback"}, testEngine{})
	if err != nil {
		t.Fatal(err)
	}
	s.control["upstreams"] = []any{map[string]any{"name": "sub2api", "target": upstream.URL, "base_path": "/v1"}}
	s.running = true
	s.SetStorage(store)
	defer s.Shutdown(context.Background())
	for _, path := range []string{"/r/sub2api/v1/models", "/api/requests/v1/models", "/api/requests"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"message":"hello"}`))
		req.Header.Set("Authorization", "Bearer provider-key")
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("proxy %s status=%d body=%s", path, rr.Code, rr.Body.String())
		}
	}
	for _, item := range []struct{ method, path string }{
		{http.MethodGet, "/api/config"},
		{http.MethodPost, "/api/config/patch"},
		{http.MethodGet, "/api/requests"},
		{http.MethodGet, "/api/requests/0123456789abcdef0123456789abcdef"},
		{http.MethodGet, "/api/logs"},
	} {
		req := httptest.NewRequest(item.method, item.path, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer provider-key")
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("management %s %s status=%d body=%s", item.method, item.path, rr.Code, rr.Body.String())
		}
	}
	if forwarded.Load() != 3 {
		t.Fatalf("upstream requests=%d", forwarded.Load())
	}
	for _, credential := range []string{"header", "cookie"} {
		req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
		if credential == "header" {
			req.Header.Set("X-Silencer-Token", "management-secret")
		} else {
			req.AddCookie(&http.Cookie{Name: "silencer_session", Value: "management-secret"})
		}
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("management %s status=%d", credential, rr.Code)
		}
	}
}

func TestManagementLoginAndLogout(t *testing.T) {
	s, err := NewServer(Config{AuthUsername: "admin", AuthToken: "management-secret"}, testEngine{})
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"username":"wrong","password":"management-secret"}`, `{"username":"admin","password":"wrong"}`} {
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(body)))
		if rr.Code != http.StatusUnauthorized || len(rr.Result().Cookies()) != 0 {
			t.Fatalf("invalid login status=%d", rr.Code)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"username":"admin","password":"management-secret"}`))
	req.Header.Set("X-Forwarded-Proto", "https")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	cookies := rr.Result().Cookies()
	if rr.Code != http.StatusOK || len(cookies) != 1 || !cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteLaxMode {
		t.Fatalf("login status=%d cookies=%v", rr.Code, cookies)
	}
	statusReq := httptest.NewRequest(http.MethodGet, "/api/auth/status", nil)
	statusReq.AddCookie(cookies[0])
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, statusReq)
	if !strings.Contains(rr.Body.String(), `"authenticated":true`) {
		t.Fatalf("login cookie not accepted: %s", rr.Body.String())
	}
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil))
	logoutCookies := rr.Result().Cookies()
	if rr.Code != http.StatusOK || len(logoutCookies) != 1 || logoutCookies[0].MaxAge >= 0 || logoutCookies[0].Value != "" {
		t.Fatalf("logout did not expire cookie: %d %v", rr.Code, logoutCookies)
	}
}

func TestNamedRouteUsesConfiguredUpstreamWithoutGlobalURL(t *testing.T) {
	var gotPath, gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()

	s, err := NewServer(Config{MaxConcurrency: 1, Engine: "go-fallback"}, &streamingBodyEngine{pushes: new(int)})
	if err != nil {
		t.Fatal(err)
	}
	s.configMu.Lock()
	s.control["upstreams"] = []any{map[string]any{"name": "demo", "target": upstream.URL, "base_path": "/v1"}}
	s.running = true
	s.configMu.Unlock()
	tsrv := httptest.NewServer(s.Handler())
	defer tsrv.Close()

	resp, err := tsrv.Client().Post(tsrv.URL+"/r/demo/chat", "text/plain", strings.NewReader("secret"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || gotPath != "/v1/chat" || gotBody != "masked" {
		t.Fatalf("status=%d path=%q body=%q", resp.StatusCode, gotPath, gotBody)
	}

	missing, err := tsrv.Client().Get(tsrv.URL + "/r/missing/chat")
	if err != nil {
		t.Fatal(err)
	}
	defer missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("missing route status=%d", missing.StatusCode)
	}
}

func TestExtensionRestoreTTLIsConfigurable(t *testing.T) {
	s, err := NewServer(Config{MaxConcurrency: 1, Engine: "go-fallback"}, testEngine{})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.extensionRestoreTTL(); got != extensionRestoreIdleTTL {
		t.Fatalf("default ttl=%s", got)
	}
	s.configMu.Lock()
	s.control["session_ttl"] = 45
	s.configMu.Unlock()
	if got := s.extensionRestoreTTL(); got != 45*time.Second {
		t.Fatalf("configured ttl=%s", got)
	}
	s.configMu.Lock()
	s.control["session_ttl"] = 1
	s.configMu.Unlock()
	if got := s.extensionRestoreTTL(); got != extensionRestoreIdleTTL {
		t.Fatalf("invalid ttl=%s", got)
	}
}

func TestEngineConfigFromControlBuildsEnabledSensitiveRules(t *testing.T) {
	raw, err := engineConfigFromControl(map[string]any{
		"sensitive": map[string]any{
			"CUSTOM":   []any{"alpha", "beta"},
			"DISABLED": []any{"hidden"},
		},
		"sensitive_disabled":      []any{"DISABLED"},
		"sensitive_word_disabled": map[string]any{"CUSTOM": []any{"beta"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, `"needle":"alpha"`) || strings.Contains(text, "beta") || strings.Contains(text, "hidden") {
		t.Fatalf("unexpected engine config: %s", text)
	}
	if !strings.Contains(text, `"label":"EMAIL"`) || !strings.Contains(text, `"label":"BEARER"`) {
		t.Fatalf("safe defaults missing: %s", text)
	}
}

func TestEngineConfigFromControlCarriesLegacySemanticRuleOptions(t *testing.T) {
	raw, err := engineConfigFromControl(map[string]any{
		"sensitive":     map[string]any{},
		"builtin_rules": map[string]any{"CONNSTR": true, "SECRET": true, "CARD": true, "IDCARD": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Rules []map[string]any `json:"rules"`
	}
	if err := json.Unmarshal([]byte(raw), &config); err != nil {
		t.Fatal(err)
	}
	var sawConn, sawSecret, sawCard, sawID bool
	for _, rule := range config.Rules {
		label, _ := rule["label"].(string)
		if label == "CONNSTR" {
			if int(rule["capture_group"].(float64)) != 1 {
				t.Fatalf("CONNSTR must replace only its password: %#v", rule)
			}
			sawConn = true
		}
		if label == "SECRET" {
			if int(rule["capture_group"].(float64)) != 3 {
				t.Fatalf("SECRET must replace only its value: %#v", rule)
			}
			sawSecret = true
		}
		if label == "CARD" {
			if rule["semantic"] != true {
				t.Fatalf("CARD semantic validation missing: %#v", rule)
			}
			sawCard = true
		}
		if label == "IDCARD" {
			if rule["semantic"] != true {
				t.Fatalf("IDCARD semantic validation missing: %#v", rule)
			}
			sawID = true
		}
	}
	if !sawConn || !sawSecret || !sawCard || !sawID {
		t.Fatalf("legacy semantic options missing from engine config: %s", raw)
	}
}

func TestEngineConfigFromControlTreatsBuiltinRuleMapAsAuthoritative(t *testing.T) {
	t.Run("all disabled stays explicitly empty", func(t *testing.T) {
		raw, err := engineConfigFromControl(map[string]any{
			"sensitive":     map[string]any{},
			"builtin_rules": map[string]any{"EMAIL": false, "TOKEN": false},
		})
		if err != nil {
			t.Fatal(err)
		}
		if raw == "" {
			t.Fatal("explicit disable-all must not fall back to C++ default rules")
		}
		var config struct {
			Rules []any `json:"rules"`
		}
		if err := json.Unmarshal([]byte(raw), &config); err != nil {
			t.Fatal(err)
		}
		if config.Rules == nil || len(config.Rules) != 0 {
			t.Fatalf("explicit disable-all emitted rules: %s", raw)
		}
	})

	t.Run("custom words do not re-enable disabled defaults", func(t *testing.T) {
		raw, err := engineConfigFromControl(map[string]any{
			"sensitive":     map[string]any{"CUSTOM": []any{"alpha"}},
			"builtin_rules": map[string]any{"EMAIL": false, "TOKEN": false},
		})
		if err != nil {
			t.Fatal(err)
		}
		text := string(raw)
		if !strings.Contains(text, `"needle":"alpha"`) || strings.Contains(text, `"label":"EMAIL"`) || strings.Contains(text, `"label":"BEARER"`) {
			t.Fatalf("explicit builtin switches were not respected: %s", text)
		}
	})

	t.Run("legacy custom-word config keeps safe defaults", func(t *testing.T) {
		raw, err := engineConfigFromControl(map[string]any{"sensitive": map[string]any{}})
		if err != nil {
			t.Fatal(err)
		}
		text := string(raw)
		if !strings.Contains(text, `"label":"EMAIL"`) || !strings.Contains(text, `"label":"BEARER"`) {
			t.Fatalf("legacy config lost default protections: %s", text)
		}
	})
}

func TestEngineConfigFromControlPreservesCustomRegexWords(t *testing.T) {
	raw, err := engineConfigFromControl(map[string]any{
		"sensitive": map[string]any{
			"CUSTOM": []any{"re:EMP-[0-9]{6}", "re:EMP-[0-9"},
		},
		"builtin_rules": map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(raw, `"pattern":"EMP-[0-9]{6}"`) || strings.Contains(raw, `EMP-[0-9"`) {
		t.Fatalf("custom regex conversion is incorrect: %s", raw)
	}
}

func TestDefaultControlConfigIncludesBuiltinRuleInventory(t *testing.T) {
	control := defaultControlConfig()
	rules, ok := control["builtin_rules"].(map[string]any)
	if !ok || len(rules) < 20 {
		t.Fatalf("builtin rule inventory missing: %#v", control["builtin_rules"])
	}
	for _, label := range []string{"API_KEY", "CARD", "CONNSTR", "EMAIL", "IDCARD", "LANDLINE", "PHONE"} {
		if rules[label] != true {
			t.Fatalf("default rule %s=%v, want enabled", label, rules[label])
		}
	}
	meta, ok := control["_meta"].(map[string]any)
	if !ok || len(meta) == 0 {
		t.Fatalf("builtin rule metadata missing: %#v", control["_meta"])
	}
}

func TestTokenReuseTrackerOnlyCountsPriorRequests(t *testing.T) {
	tracker := newTokenReuseTracker()
	if tracker.observe([]byte(`prefix {{EMAIL_abcdef}}`)) {
		t.Fatal("first issuance must not be reported as reuse")
	}
	if !tracker.observe([]byte(`prefix {{EMAIL_abcdef}}`)) {
		t.Fatal("a token seen in a prior request must be reported as reuse")
	}
	if tracker.observe([]byte(`{{PHONE_bcdefa}} {{PHONE_bcdefa}}`)) {
		t.Fatal("duplicate tokens within one request must not count as prior-request reuse")
	}
}

func TestReadyCanRequireConfiguredUpstream(t *testing.T) {
	s, err := NewServer(Config{MaxConcurrency: 1, Engine: "go-fallback", RequireUpstream: true}, testEngine{})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"require_upstream":true`) || !strings.Contains(rec.Body.String(), `"upstream_configured":false`) {
		t.Fatalf("readiness body=%s", rec.Body.String())
	}
}

func TestProxyEnforcesUpstreamHeaderTimeout(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Accept the connection but never send response headers. The gateway's
		// transport must release the request instead of holding a concurrency slot
		// forever when an upstream is alive but wedged.
		time.Sleep(2 * time.Second)
	}))
	defer upstream.Close()
	s, err := NewServer(Config{
		UpstreamURL: upstream.URL, MaxConcurrency: 1, Engine: "go-fallback",
		UpstreamHeaderTimeoutSeconds: 1,
	}, testEngine{})
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := s.proxy.Transport.(*http.Transport)
	if !ok || transport.ResponseHeaderTimeout != time.Second {
		if ok {
			t.Fatalf("transport header timeout not configured: %v", transport.ResponseHeaderTimeout)
		}
		t.Fatal("proxy transport is not *http.Transport")
	}
	if transport.MaxIdleConnsPerHost != 2 || transport.MaxIdleConns < 2 {
		t.Fatalf("transport idle pool not aligned with concurrency: per_host=%d total=%d", transport.MaxIdleConnsPerHost, transport.MaxIdleConns)
	}
	probeStarted := time.Now()
	probeReq, _ := http.NewRequest(http.MethodGet, upstream.URL, nil)
	probeResp, probeErr := transport.RoundTrip(probeReq)
	if probeResp != nil {
		probeResp.Body.Close()
	}
	if probeErr == nil || time.Since(probeStarted) > 1500*time.Millisecond {
		t.Fatalf("direct transport timeout err=%v elapsed=%s", probeErr, time.Since(probeStarted))
	}
}

func TestHealthReadyAndMetricsExposeStorageState(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SILENCER_STORAGE_MODE", "sqlite")
	t.Setenv("SILENCER_SQLITE_PATH", filepath.Join(dir, "health.db"))
	store, err := storage.OpenFromEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s, err := NewServer(Config{MaxConcurrency: 2, Engine: "go-fallback"}, testEngine{})
	if err != nil {
		t.Fatal(err)
	}
	s.SetStorage(store)
	defer s.Shutdown(context.Background())
	for _, path := range []string{"/readyz", "/api/health", "/metrics"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", path, rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if !strings.Contains(body, "queue_depth") && path != "/metrics" {
			t.Fatalf("%s omitted storage queue state: %s", path, body)
		}
		if path == "/api/health" && (!strings.Contains(body, `"queue_capacity"`) || !strings.Contains(body, `"last_write_error"`)) {
			t.Fatalf("%s omitted storage alert state: %s", path, body)
		}
		if path == "/metrics" && !strings.Contains(body, "silencer_storage_queue_capacity") {
			t.Fatalf("metrics omitted storage queue capacity: %s", body)
		}
	}
}

func TestReadyReportsStorageFailureWithoutDriverDetails(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SILENCER_STORAGE_MODE", "sqlite")
	t.Setenv("SILENCER_SQLITE_PATH", filepath.Join(dir, "closed.db"))
	store, err := storage.OpenFromEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := NewServer(Config{MaxConcurrency: 1, Engine: "go-fallback"}, testEngine{})
	if err != nil {
		t.Fatal(err)
	}
	s.SetStorage(store)
	defer s.Shutdown(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), `"storage_error":"unavailable"`) {
		t.Fatalf("readiness failure status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(strings.ToLower(rec.Body.String()), "database is closed") {
		t.Fatalf("readiness leaked driver detail: %s", rec.Body.String())
	}
}

func TestStorageFailurePreventsUpstreamForwarding(t *testing.T) {
	var forwarded atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	t.Setenv("SILENCER_STORAGE_MODE", "sqlite")
	t.Setenv("SILENCER_SQLITE_PATH", filepath.Join(t.TempDir(), "closed-ledger.db"))
	store, err := storage.OpenFromEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := NewServer(Config{UpstreamURL: upstream.URL, MaxConcurrency: 1, Engine: "go-fallback"}, testEngine{})
	if err != nil {
		t.Fatal(err)
	}
	s.SetStorage(store)
	defer s.Shutdown(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/api/requests/v1/chat/completions", strings.NewReader(`{"email":"alice@example.com"}`))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), `"error":"storage_unavailable"`) || forwarded.Load() != 0 {
		t.Fatalf("status=%d upstream_requests=%d body=%s", rec.Code, forwarded.Load(), rec.Body.String())
	}
	if strings.Contains(strings.ToLower(rec.Body.String()), "storage closed") {
		t.Fatalf("response exposed storage implementation detail: %s", rec.Body.String())
	}
}

func TestProxyRequestLedgerCapturesBodiesAndResponse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"echo": string(body)})
	}))
	defer upstream.Close()
	dir := t.TempDir()
	oldMode, oldPath := os.Getenv("SILENCER_STORAGE_MODE"), os.Getenv("SILENCER_SQLITE_PATH")
	defer os.Setenv("SILENCER_STORAGE_MODE", oldMode)
	defer os.Setenv("SILENCER_SQLITE_PATH", oldPath)
	os.Setenv("SILENCER_STORAGE_MODE", "sqlite")
	os.Setenv("SILENCER_SQLITE_PATH", filepath.Join(dir, "ledger.db"))
	store, err := storage.OpenFromEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s, err := NewServer(Config{UpstreamURL: upstream.URL, MaxConcurrency: 2, Engine: "go-fallback", StoreRawBody: true}, testEngine{})
	if err != nil {
		t.Fatal(err)
	}
	s.SetStorage(store)
	defer s.Shutdown(context.Background())
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/requests/v1/chat", strings.NewReader(`{"email":"a@example.com"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	requestID := resp.Header.Get("X-Silencer-Request-ID")
	resp.Body.Close()
	if requestID == "" {
		t.Fatal("missing request id")
	}
	for i := 0; i < 50; i++ {
		items, listErr := store.ListRequests(context.Background(), 10)
		if listErr == nil && len(items) == 1 && items[0].EndedAt.After(items[0].StartedAt) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	items, err := store.ListRequests(context.Background(), 10)
	if err != nil || len(items) != 1 {
		t.Fatalf("items=%+v err=%v", items, err)
	}
	if string(items[0].Raw) == "" || string(items[0].Masked) == "" || string(items[0].Restored) == "" || items[0].Status != "201" {
		t.Fatalf("ledger=%+v", items[0])
	}
	listResp, err := ts.Client().Get(ts.URL + "/api/requests?limit=10")
	if err != nil {
		t.Fatal(err)
	}
	defer listResp.Body.Close()
	listBody, _ := io.ReadAll(listResp.Body)
	if listResp.StatusCode != http.StatusOK || !bytes.Contains(listBody, []byte(requestID)) {
		t.Fatalf("request list status=%d body=%s", listResp.StatusCode, listBody)
	}
	detailResp, err := ts.Client().Get(ts.URL + "/api/requests/" + requestID)
	if err != nil {
		t.Fatal(err)
	}
	detailBody, _ := io.ReadAll(detailResp.Body)
	_ = detailResp.Body.Close()
	if detailResp.StatusCode != http.StatusOK || !bytes.Contains(detailBody, []byte("a@example.com")) || !bytes.Contains(detailBody, []byte(`"raw"`)) {
		t.Fatalf("default detail capture did not expose the requested body: status=%d body=%s", detailResp.StatusCode, detailBody)
	}
}

func TestRequestRecordOmitsRawByDefault(t *testing.T) {
	t.Setenv("SILENCER_STORAGE_MODE", "sqlite")
	t.Setenv("SILENCER_SQLITE_PATH", filepath.Join(t.TempDir(), "raw-default.db"))
	store, err := storage.OpenFromEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	started := time.Now().UTC()
	item := storage.Request{ID: "raw-default", Payload: []byte("POST /v1/chat"), Status: "running", StartedAt: started, CreatedAt: started}
	if err := store.AppendRequest(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	record := &requestRecord{store: store, maxBytes: 1024, item: item}
	record.appendRequest(false, []byte(`{"secret":"do-not-persist"}`))
	record.appendRequest(true, []byte(`{"secret":"{{TERM_token}}"}`))
	record.finish(http.StatusOK, nil)
	items, err := store.ListRequests(context.Background(), 10)
	if err != nil || len(items) != 1 {
		t.Fatalf("items=%+v err=%v", items, err)
	}
	if len(items[0].Raw) != 0 {
		t.Fatalf("raw body persisted by default: %q", items[0].Raw)
	}
	if string(items[0].Masked) == "" {
		t.Fatal("masked body was not persisted")
	}
}

func TestDetailRecordingExposesRequestAndEventBodies(t *testing.T) {
	t.Setenv("SILENCER_STORAGE_MODE", "sqlite")
	t.Setenv("SILENCER_SQLITE_PATH", filepath.Join(t.TempDir(), "detail-recording.db"))
	store, err := storage.OpenFromEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	request := storage.Request{
		ID:       "0123456789abcdef0123456789abcdef",
		Raw:      []byte(`{"email":"alice@example.com"}`),
		Masked:   []byte(`{"email":"{{EMAIL_abc123}}"}`),
		Upstream: []byte(`{"reply":"{{EMAIL_abc123}}"}`),
		Restored: []byte(`{"reply":"alice@example.com"}`),
		Meta:     `{"items":[{"label":"EMAIL","original":"alice@example.com","preview":"{{EMAIL_abc123}}","tok":"{{EMAIL_abc123}}","display":"plaintext"}]}`,
		Status:   "200", StartedAt: now, EndedAt: now, CreatedAt: now,
	}
	if err := store.AppendRequest(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendEvent(context.Background(), storage.Event{
		ID: "detail-event", RequestID: request.ID, Type: "MASK", Masked: request.Masked,
		Meta:      `{"sid":"0123456789abcdef0123456789abcdef","count":1,"items":[{"label":"EMAIL","original":"alice@example.com","preview":"{{EMAIL_abc123}}","tok":"{{EMAIL_abc123}}"}]}`,
		CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	s, err := NewServer(Config{MaxConcurrency: 1, Engine: "go-fallback"}, testEngine{})
	if err != nil {
		t.Fatal(err)
	}
	s.SetStorage(store)
	defer s.Shutdown(context.Background())
	s.configMu.Lock()
	s.control["record_detail_data"] = true
	s.configMu.Unlock()
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	for _, path := range []string{"/api/requests/" + request.ID, "/api/logs/detail?id=detail-event"} {
		resp, getErr := ts.Client().Get(ts.URL + path)
		if getErr != nil {
			t.Fatal(getErr)
		}
		var body map[string]any
		decodeErr := json.NewDecoder(resp.Body).Decode(&body)
		_ = resp.Body.Close()
		if decodeErr != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("path=%s status=%d decode=%v body=%v", path, resp.StatusCode, decodeErr, body)
		}
		encoded, _ := json.Marshal(body)
		for _, value := range []string{"alice@example.com", "{{EMAIL_abc123}}", `"restored_response"`} {
			if !bytes.Contains(encoded, []byte(value)) {
				t.Fatalf("path=%s omitted %q: %s", path, value, encoded)
			}
		}
		if !bytes.Contains(encoded, []byte(`"detail_recorded":true`)) {
			t.Fatalf("path=%s did not mark detail recording: %s", path, encoded)
		}
	}
}

func TestProxyUsageIsRecordedAndAggregated(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"gpt-4o","usage":{"prompt_tokens":100,"prompt_tokens_details":{"cached_tokens":60},"completion_tokens":7}}`)
	}))
	defer upstream.Close()
	t.Setenv("SILENCER_STORAGE_MODE", "sqlite")
	t.Setenv("SILENCER_SQLITE_PATH", filepath.Join(t.TempDir(), "usage.db"))
	store, err := storage.OpenFromEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s, err := NewServer(Config{UpstreamURL: upstream.URL, MaxConcurrency: 1, Engine: "go-fallback"}, testEngine{})
	if err != nil {
		t.Fatal(err)
	}
	s.SetStorage(store)
	defer s.Shutdown(context.Background())
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	resp, err := ts.Client().Post(ts.URL+"/api/requests/v1/chat", "application/json", strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("proxy status=%d", resp.StatusCode)
	}

	var item storage.Request
	for i := 0; i < 50; i++ {
		items, listErr := store.ListRequests(context.Background(), 10)
		if listErr == nil && len(items) == 1 && items[0].EndedAt.After(items[0].StartedAt) {
			item = items[0]
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if item.ID == "" {
		t.Fatal("request ledger did not finish")
	}
	usage := usageFromMeta(item.Meta)
	if !usage.Found || usage.Model != "gpt-4o" || usage.Prompt != 100 || usage.Completion != 7 || !usage.CacheFound || usage.CacheRead != 60 {
		t.Fatalf("metadata=%s usage=%+v", item.Meta, usage)
	}

	getJSON := func(path string, out any) {
		t.Helper()
		response, requestErr := ts.Client().Get(ts.URL + path)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s status=%d", path, response.StatusCode)
		}
		if decodeErr := json.NewDecoder(response.Body).Decode(out); decodeErr != nil {
			t.Fatal(decodeErr)
		}
	}
	var today struct {
		Tokens struct {
			Prompt     int64 `json:"prompt"`
			Completion int64 `json:"completion"`
		} `json:"tokens"`
		Cache struct {
			Read     int64    `json:"read_tokens"`
			Observed int64    `json:"observed_requests"`
			HitRate  *float64 `json:"hit_rate"`
		} `json:"cache"`
	}
	getJSON("/api/stats/today", &today)
	if today.Tokens.Prompt != 100 || today.Tokens.Completion != 7 {
		t.Fatalf("today tokens=%+v", today.Tokens)
	}
	if today.Cache.Read != 60 || today.Cache.Observed != 1 || today.Cache.HitRate == nil || *today.Cache.HitRate != 0.6 {
		t.Fatalf("today cache=%+v", today.Cache)
	}
	var history struct {
		Data []struct {
			CacheRead     int64 `json:"cache_read_tokens"`
			CacheWrite    int64 `json:"cache_write_tokens"`
			CacheObserved int64 `json:"cache_observed_requests"`
		} `json:"data"`
	}
	getJSON("/api/stats/history?days=1&granularity=day", &history)
	if len(history.Data) != 1 || history.Data[0].CacheRead != 60 || history.Data[0].CacheObserved != 1 {
		t.Fatalf("history cache=%+v", history.Data)
	}
	var models struct {
		Models []struct {
			Model         string   `json:"model"`
			Prompt        int64    `json:"prompt"`
			Completion    int64    `json:"completion"`
			CacheRead     int64    `json:"cache_read_tokens"`
			CacheObserved int64    `json:"cache_observed_requests"`
			CacheHitRate  *float64 `json:"cache_hit_rate"`
			Cost          float64  `json:"cost_usd"`
			Priced        bool     `json:"priced"`
		} `json:"models"`
	}
	getJSON("/api/stats/models?days=1", &models)
	if len(models.Models) != 1 || models.Models[0].Model != "gpt-4o" || models.Models[0].Prompt != 100 || models.Models[0].Completion != 7 || models.Models[0].CacheRead != 60 || models.Models[0].CacheObserved != 1 || models.Models[0].CacheHitRate == nil || *models.Models[0].CacheHitRate != 0.6 || !models.Models[0].Priced || models.Models[0].Cost <= 0 {
		t.Fatalf("models=%+v", models.Models)
	}
	var logs struct {
		Events []struct {
			Type string `json:"type"`
		} `json:"events"`
	}
	getJSON("/api/logs?slim=1", &logs)
	seenMask, seenRestore := false, false
	for _, event := range logs.Events {
		seenMask = seenMask || event.Type == "MASK"
		seenRestore = seenRestore || event.Type == "RESTORE"
	}
	if !seenMask || !seenRestore {
		t.Fatalf("operational events missing: %+v", logs.Events)
	}
}

func TestProxyCacheUsageProviderFormatsReachRequestLedgerAndStats(t *testing.T) {
	responses := map[string]string{
		"/v1/responses": `{"response":{"model":"gpt-4.1","usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":50},"output_tokens":7}}}`,
		"/v1beta/models/gemini-2.5-pro:generateContent": `{"modelVersion":"gemini-2.5-pro","usageMetadata":{"promptTokenCount":80,"candidatesTokenCount":4,"cachedContentTokenCount":60}}`,
		"/v1/messages": `{"content":[],"model":"claude-test","usage":{"input_tokens":20,"cache_read_input_tokens":30,"cache_creation_input_tokens":10,"output_tokens":5}}`,
		"/api/chat":    `{"model":"llama3.2","prompt_eval_count":50,"eval_count":4,"done":true}`,
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := responses[r.URL.Path]
		if !ok {
			t.Errorf("unexpected provider path %q", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	defer upstream.Close()

	t.Setenv("SILENCER_STORAGE_MODE", "sqlite")
	t.Setenv("SILENCER_SQLITE_PATH", filepath.Join(t.TempDir(), "provider-cache-usage.db"))
	store, err := storage.OpenFromEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := NewServer(Config{UpstreamURL: upstream.URL, MaxConcurrency: 1, Engine: "go-fallback"}, testEngine{})
	if err != nil {
		t.Fatal(err)
	}
	server.SetStorage(store)
	defer server.Shutdown(context.Background())
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	cases := []struct {
		name, path, body, model                     string
		prompt, completion, read, write, cacheInput int64
	}{
		{"responses", "/api/requests/v1/responses", `{"model":"gpt-4.1","input":"hello"}`, "gpt-4.1", 100, 7, 50, 0, 100},
		{"gemini", "/api/requests/v1beta/models/gemini-2.5-pro:generateContent?key=test-key", `{"contents":[{"parts":[{"text":"hello"}]}]}`, "gemini-2.5-pro", 80, 4, 60, 0, 80},
		{"anthropic", "/api/requests/v1/messages", `{"model":"claude-test","messages":[]}`, "claude-test", 20, 5, 30, 10, 60},
		{"ollama", "/api/requests/api/chat", `{"model":"llama3.2","messages":[],"stream":false}`, "llama3.2", 50, 4, 0, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, requestErr := ts.Client().Post(ts.URL+tc.path, "application/json", strings.NewReader(tc.body))
			if requestErr != nil {
				t.Fatal(requestErr)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("proxy status=%d", resp.StatusCode)
			}
		})
	}

	var items []storage.Request
	for attempt := 0; attempt < 100; attempt++ {
		items, err = store.ListRequests(context.Background(), 10)
		if err == nil && len(items) == len(cases) {
			complete := true
			for _, item := range items {
				complete = complete && item.EndedAt.After(item.StartedAt)
			}
			if complete {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(items) != len(cases) {
		t.Fatalf("request ledger recorded %d requests, want %d", len(items), len(cases))
	}
	got := make(map[string]usageSummary, len(items))
	for _, item := range items {
		usage := usageFromMeta(item.Meta)
		got[usage.Model] = usage
	}
	var totalPrompt, totalCompletion, totalRead, totalWrite, totalCacheInput int64
	for _, tc := range cases {
		usage, ok := got[tc.model]
		if !ok || !usage.Found || usage.Prompt != tc.prompt || usage.Completion != tc.completion || usage.CacheRead != tc.read || usage.CacheWrite != tc.write {
			t.Errorf("%s usage=%+v, want prompt=%d completion=%d cache_read=%d cache_write=%d", tc.name, usage, tc.prompt, tc.completion, tc.read, tc.write)
		}
		if tc.cacheInput > 0 && (!usage.CacheRateFound || usage.CacheTotal != tc.cacheInput) {
			t.Errorf("%s cache denominator: found=%v total=%d, want %d", tc.name, usage.CacheRateFound, usage.CacheTotal, tc.cacheInput)
		}
		totalPrompt += tc.prompt
		totalCompletion += tc.completion
		totalRead += tc.read
		totalWrite += tc.write
		totalCacheInput += tc.cacheInput
	}

	statsResp, err := ts.Client().Get(ts.URL + "/api/stats/today")
	if err != nil {
		t.Fatal(err)
	}
	defer statsResp.Body.Close()
	if statsResp.StatusCode != http.StatusOK {
		t.Fatalf("stats status=%d", statsResp.StatusCode)
	}
	var stats struct {
		Tokens struct {
			Prompt     int64 `json:"prompt"`
			Completion int64 `json:"completion"`
		} `json:"tokens"`
		Cache struct {
			Read        int64    `json:"read_tokens"`
			Write       int64    `json:"write_tokens"`
			Observed    int64    `json:"observed_requests"`
			RateSamples int64    `json:"rate_observed_requests"`
			Input       int64    `json:"input_tokens"`
			HitRate     *float64 `json:"hit_rate"`
			Source      string   `json:"source"`
		} `json:"cache"`
	}
	if err := json.NewDecoder(statsResp.Body).Decode(&stats); err != nil {
		t.Fatal(err)
	}
	wantRate := float64(totalRead) / float64(totalCacheInput)
	if stats.Tokens.Prompt != totalPrompt || stats.Tokens.Completion != totalCompletion ||
		stats.Cache.Read != totalRead || stats.Cache.Write != totalWrite || stats.Cache.Observed != 3 ||
		stats.Cache.RateSamples != 3 || stats.Cache.Input != totalCacheInput || stats.Cache.HitRate == nil ||
		*stats.Cache.HitRate != wantRate || stats.Cache.Source != "upstream_usage" {
		t.Fatalf("provider usage aggregate=%+v tokens=%+v, want read=%d write=%d input=%d rate=%v", stats.Cache, stats.Tokens, totalRead, totalWrite, totalCacheInput, wantRate)
	}
}

func TestStatsCacheRatesIgnoreUncomputableReadCounts(t *testing.T) {
	t.Setenv("SILENCER_STORAGE_MODE", "sqlite")
	t.Setenv("SILENCER_SQLITE_PATH", filepath.Join(t.TempDir(), "cache-rates.db"))
	store, err := storage.OpenFromEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Now().UTC()
	metas := []string{
		`{"usage_found":true,"usage_prompt_tokens":100,"usage_cache_observed":true,"usage_cache_read_tokens":60,"usage_cache_rate_observed":true,"usage_cache_input_tokens":100,"model":"gpt-4o"}`,
		`{"usage_found":true,"usage_prompt_tokens":100,"usage_cache_observed":true,"usage_cache_read_tokens":101,"usage_cache_rate_observed":false,"usage_cache_input_tokens":0,"model":"gpt-4o"}`,
	}
	for i, meta := range metas {
		created := now.Add(time.Duration(i) * time.Second)
		if err := store.AppendRequest(context.Background(), storage.Request{
			ID: fmt.Sprintf("cache-rate-%d", i), Payload: []byte("POST /v1/chat"), Status: "200", Meta: meta,
			StartedAt: created, EndedAt: created, CreatedAt: created,
		}); err != nil {
			t.Fatal(err)
		}
	}
	s, err := NewServer(Config{MaxConcurrency: 1, Engine: "go-fallback"}, testEngine{})
	if err != nil {
		t.Fatal(err)
	}
	s.SetStorage(store)
	defer s.Shutdown(context.Background())
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	getJSON := func(path string, out any) {
		t.Helper()
		resp, err := ts.Client().Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status=%d", path, resp.StatusCode)
		}
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
	var today struct {
		Cache struct {
			Read         int64    `json:"read_tokens"`
			Observed     int64    `json:"observed_requests"`
			RateObserved int64    `json:"rate_observed_requests"`
			Input        int64    `json:"input_tokens"`
			HitRate      *float64 `json:"hit_rate"`
		} `json:"cache"`
	}
	getJSON("/api/stats/today", &today)
	if today.Cache.Read != 161 || today.Cache.Observed != 2 || today.Cache.RateObserved != 1 || today.Cache.Input != 100 || today.Cache.HitRate == nil || *today.Cache.HitRate != 0.6 {
		t.Fatalf("today cache=%+v", today.Cache)
	}
	var models struct {
		Models []struct {
			Read         int64    `json:"cache_read_tokens"`
			Observed     int64    `json:"cache_observed_requests"`
			RateObserved int64    `json:"cache_rate_observed_requests"`
			Input        int64    `json:"cache_input_tokens"`
			HitRate      *float64 `json:"cache_hit_rate"`
		} `json:"models"`
	}
	getJSON("/api/stats/models?days=1", &models)
	if len(models.Models) != 1 || models.Models[0].Read != 161 || models.Models[0].Observed != 2 || models.Models[0].RateObserved != 1 || models.Models[0].Input != 100 || models.Models[0].HitRate == nil || *models.Models[0].HitRate != 0.6 {
		t.Fatalf("models cache=%+v", models.Models)
	}
}

func TestRequestRecordExtractsTrailingSSEUsageAfterCaptureTruncation(t *testing.T) {
	t.Setenv("SILENCER_STORAGE_MODE", "sqlite")
	t.Setenv("SILENCER_SQLITE_PATH", filepath.Join(t.TempDir(), "usage-tail.db"))
	store, err := storage.OpenFromEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	started := time.Now().UTC()
	item := storage.Request{ID: "usage-tail", Payload: []byte("POST /v1/chat"), Status: "running", StartedAt: started, CreatedAt: started}
	if err := store.AppendRequest(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	record := &requestRecord{store: store, maxBytes: 64, item: item}
	body := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"" + strings.Repeat("x", 256) + "\"}}]}\n\n" +
		"data: {\"model\":\"gpt-4o\",\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"prompt_tokens_details\":{\"cached_tokens\":75},\"completion_tokens\":9}}\n\n")
	record.appendResponse(false, body[:64])
	record.appendResponse(false, body[64:len(body)-13])
	record.appendResponse(false, body[len(body)-13:])
	record.finish(http.StatusOK, nil)

	stored, err := store.GetRequest(context.Background(), item.ID)
	if err != nil {
		t.Fatal(err)
	}
	usage := usageFromMeta(stored.Meta)
	var metadata map[string]any
	if err := json.Unmarshal([]byte(stored.Meta), &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata["upstream_truncated"] != true {
		t.Fatalf("response capture was not marked truncated: %s", stored.Meta)
	}
	if !usage.Found || usage.Model != "gpt-4o" || usage.Prompt != 100 || usage.Completion != 9 || !usage.CacheFound || usage.CacheRead != 75 {
		t.Fatalf("trailing SSE usage missing after capture truncation: meta=%s usage=%+v", stored.Meta, usage)
	}
}

func TestStatsAggregateLedgerBodiesAndHourlyBuckets(t *testing.T) {
	t.Setenv("SILENCER_STORAGE_MODE", "sqlite")
	t.Setenv("SILENCER_SQLITE_PATH", filepath.Join(t.TempDir(), "stats.db"))
	store, err := storage.OpenFromEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	for i, created := range []time.Time{now.Add(-30 * time.Minute), now.Add(-2 * time.Hour)} {
		id := fmt.Sprintf("stats-%d", i)
		var restoredBody []byte
		if i == 0 {
			restoredBody = []byte("ok")
		}
		if err := store.AppendRequest(context.Background(), storage.Request{
			ID: id, Payload: []byte("POST /v1/chat"), Masked: []byte("{{EMAIL_bcdfgh}}"),
			Restored: restoredBody, Status: "200", Meta: `{"usage_found":true,"usage_prompt_tokens":3,"usage_completion_tokens":2,"model":"demo"}`,
			StartedAt: created, EndedAt: created, CreatedAt: created,
		}); err != nil {
			t.Fatal(err)
		}
		// Operational MASK/RESTORE events mirror the request ledger. The
		// management aggregates must count them once, not once per table.
		if err := store.AppendEvent(context.Background(), storage.Event{
			ID: fmt.Sprintf("event-mask-%d", i), RequestID: id, Type: "MASK",
			Payload: []byte{}, Masked: []byte("{{EMAIL_bcdfgh}}"), CreatedAt: created,
		}); err != nil {
			t.Fatal(err)
		}
		if err := store.AppendEvent(context.Background(), storage.Event{
			ID: fmt.Sprintf("event-restore-%d", i), RequestID: id, Type: "RESTORE",
			Payload: []byte{}, Status: map[bool]string{true: "restored", false: "no_sensitive_data"}[i == 0], Restored: restoredBody, CreatedAt: created,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.AppendRequest(context.Background(), storage.Request{ID: "stats-clean", Payload: []byte("GET /v1/models"), Status: "200", CreatedAt: now, StartedAt: now, EndedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendEvent(context.Background(), storage.Event{ID: "event-restore-clean", RequestID: "stats-clean", Type: "RESTORE", Status: "no_sensitive_data", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	s, err := NewServer(Config{MaxConcurrency: 1, Engine: "go-fallback"}, testEngine{})
	if err != nil {
		t.Fatal(err)
	}
	s.SetStorage(store)
	defer s.Shutdown(context.Background())
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	var today struct {
		Requests        int            `json:"requests"`
		MaskedItems     int            `json:"masked_items"`
		Restored        int            `json:"restored"`
		RestoreByStatus map[string]int `json:"restore_by_status"`
		ByLabel         map[string]int `json:"by_label"`
		TopWords        []struct {
			Label string `json:"label"`
			Count int    `json:"count"`
		} `json:"top_words"`
	}
	resp, err := ts.Client().Get(ts.URL + "/api/stats/today")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(resp.Body).Decode(&today); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if today.Requests != 3 || today.MaskedItems != 2 || today.Restored != 1 || today.RestoreByStatus["restored"] != 1 || today.RestoreByStatus["no_placeholder_in_response"] != 1 || today.RestoreByStatus["no_sensitive_data"] != 1 || today.ByLabel["EMAIL"] != 2 || len(today.TopWords) != 1 {
		t.Fatalf("today=%+v", today)
	}
	var restoreItems struct {
		Total int `json:"total_events"`
		Items []struct {
			Preview string `json:"preview"`
		} `json:"items"`
	}
	resp, err = ts.Client().Get(ts.URL + "/api/stats/today/restore-items")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(resp.Body).Decode(&restoreItems); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if restoreItems.Total != 1 || len(restoreItems.Items) != 1 || restoreItems.Items[0].Preview != "[redacted]" {
		t.Fatalf("restore items=%+v", restoreItems)
	}
	var history struct {
		Granularity string `json:"granularity"`
		Days        int    `json:"days"`
		Data        []struct {
			Requests int `json:"requests"`
		} `json:"data"`
	}
	resp, err = ts.Client().Get(ts.URL + "/api/stats/history?days=6&granularity=hour")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(resp.Body).Decode(&history); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if history.Granularity != "hour" || history.Days != 6 || len(history.Data) != 6 {
		t.Fatalf("history shape=%+v", history)
	}
	var highlights struct {
		MaskedItems int            `json:"masked_items"`
		Labels      map[string]int `json:"labels"`
	}
	resp, err = ts.Client().Get(ts.URL + "/api/stats/highlights?days=7")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(resp.Body).Decode(&highlights); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if highlights.MaskedItems != 2 || highlights.Labels["EMAIL"] != 2 {
		t.Fatalf("highlights=%+v", highlights)
	}
}

func TestStatsUseFullLedgerAndExactPlaintextItems(t *testing.T) {
	t.Setenv("SILENCER_STORAGE_MODE", "sqlite")
	t.Setenv("SILENCER_SQLITE_PATH", filepath.Join(t.TempDir(), "stats-items.db"))
	store, err := storage.OpenFromEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	largeMasked := append(bytes.Repeat([]byte("x"), 70<<10), []byte(" {{IP_PUBLIC_a1}} {{API_KEY_b2}}")...)
	meta := `{"ingress":"proxy","items_complete":true,"items":[{"label":"IP_PUBLIC","preview":"{{IP_PUBLIC_a1}}","original":"203.0.113.42","display":"plaintext"},{"label":"API_KEY","preview":"{{API_KEY_b2}}","original":"sk-live-example-key","display":"plaintext"}]}`
	for _, item := range []storage.Request{
		{ID: "large-items", Masked: largeMasked, Meta: meta, Status: "200", StartedAt: now, EndedAt: now, CreatedAt: now},
		{ID: "legacy-placeholder", Masked: []byte("{{IP_PUBLIC_bcdfgh}}"), Status: "200", StartedAt: now, EndedAt: now, CreatedAt: now},
	} {
		if err := store.AppendRequest(context.Background(), item); err != nil {
			t.Fatal(err)
		}
	}
	s, err := NewServer(Config{MaxConcurrency: 1, Engine: "go-fallback"}, testEngine{})
	if err != nil {
		t.Fatal(err)
	}
	s.SetStorage(store)
	defer s.Shutdown(context.Background())
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	resp, err := ts.Client().Get(ts.URL + "/api/stats/today")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var today struct {
		MaskedItems int `json:"masked_items"`
		TopWords    []struct {
			Label    string `json:"label"`
			Word     string `json:"word"`
			Original string `json:"original"`
			Count    int    `json:"count"`
		} `json:"top_words"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&today); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || today.MaskedItems != 3 {
		t.Fatalf("status=%d stats=%+v", resp.StatusCode, today)
	}
	words := map[string]int{}
	for _, word := range today.TopWords {
		if word.Original != word.Word {
			t.Fatalf("word original differs: %+v", word)
		}
		words[word.Word] = word.Count
	}
	if words["203.0.113.42"] != 1 || words["sk-live-example-key"] != 1 || words["{{IP_PUBLIC_bcdfgh}}"] != 1 {
		t.Fatalf("stats did not retain exact plaintext and legacy fallback: %+v", today.TopWords)
	}
}

func TestProxyLedgerFullCaptureIgnoresPreviewLimit(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"value":"{{TOKEN}}-response-that-is-larger-than-the-ledger-limit"}`)
	}))
	defer upstream.Close()
	dir := t.TempDir()
	oldMode, oldPath := os.Getenv("SILENCER_STORAGE_MODE"), os.Getenv("SILENCER_SQLITE_PATH")
	defer os.Setenv("SILENCER_STORAGE_MODE", oldMode)
	defer os.Setenv("SILENCER_SQLITE_PATH", oldPath)
	os.Setenv("SILENCER_STORAGE_MODE", "sqlite")
	os.Setenv("SILENCER_SQLITE_PATH", filepath.Join(dir, "ledger.db"))
	store, err := storage.OpenFromEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s, err := NewServer(Config{UpstreamURL: upstream.URL, MaxConcurrency: 1, MaxResponseBytes: 8, Engine: "go-fallback"}, replaceRestoreEngine{})
	if err != nil {
		t.Fatal(err)
	}
	s.SetStorage(store)
	defer s.Shutdown(context.Background())
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	resp, err := ts.Client().Get(ts.URL + "/api/requests/test")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("secret-value")) {
		t.Fatalf("status=%d body=%q err=%v", resp.StatusCode, body, err)
	}
	items, err := store.ListRequests(context.Background(), 10)
	if err != nil || len(items) != 1 {
		t.Fatalf("items=%+v err=%v", items, err)
	}
	if !strings.Contains(items[0].Meta, `"body_store":true`) || strings.Contains(items[0].Meta, `"upstream_truncated":true`) || strings.Contains(items[0].Meta, `"restored_truncated":true`) {
		t.Fatalf("full file capture was marked truncated: %s", items[0].Meta)
	}
	listResp, err := ts.Client().Get(ts.URL + "/api/requests?limit=10")
	if err != nil {
		t.Fatal(err)
	}
	listBody, _ := io.ReadAll(listResp.Body)
	_ = listResp.Body.Close()
	if !strings.Contains(string(listBody), `"truncated":0`) {
		t.Fatalf("full capture exposed truncation: %s", listBody)
	}
}

func TestClientCancellationFinalizesLedgerAsCanceled(t *testing.T) {
	started := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, `data: {"value":"{{TOKEN}}"}`+"\n\n")
		flusher.Flush()
		close(started)
		<-r.Context().Done()
	}))
	defer upstream.Close()
	dir := t.TempDir()
	oldMode, oldPath := os.Getenv("SILENCER_STORAGE_MODE"), os.Getenv("SILENCER_SQLITE_PATH")
	defer os.Setenv("SILENCER_STORAGE_MODE", oldMode)
	defer os.Setenv("SILENCER_SQLITE_PATH", oldPath)
	os.Setenv("SILENCER_STORAGE_MODE", "sqlite")
	os.Setenv("SILENCER_SQLITE_PATH", filepath.Join(dir, "ledger.db"))
	store, err := storage.OpenFromEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s, err := NewServer(Config{UpstreamURL: upstream.URL, MaxConcurrency: 1, Engine: "go-fallback"}, replaceRestoreEngine{})
	if err != nil {
		t.Fatal(err)
	}
	s.SetStorage(store)
	defer s.Shutdown(context.Background())
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/requests/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("upstream did not start streaming")
	}
	buf := make([]byte, 128)
	if n, readErr := resp.Body.Read(buf); readErr != nil || !bytes.Contains(buf[:n], []byte("secret-value")) {
		t.Fatalf("first stream read n=%d err=%v body=%q", n, readErr, buf[:n])
	}
	cancel()
	_ = resp.Body.Close()
	var items []storage.Request
	for i := 0; i < 100; i++ {
		items, err = store.ListRequests(context.Background(), 10)
		if err == nil && len(items) == 1 && items[0].Status == "499" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("cancellation did not finalize ledger as 499: items=%+v err=%v", items, err)
}

func TestProxyTransparentAndPathRewrite(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat" || r.URL.RawQuery != "stream=1" {
			t.Errorf("path/query = %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		if r.Header.Get("X-Test") != "yes" {
			t.Errorf("header not forwarded")
		}
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Upstream", "ok")
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, "echo:%s", body)
	}))
	defer upstream.Close()
	s, err := NewServer(Config{UpstreamURL: upstream.URL + "/v1", MaxConcurrency: 2, Engine: "go-fallback"}, testEngine{})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/requests/chat?stream=1", strings.NewReader("hello"))
	req.Header.Set("X-Test", "yes")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Upstream"); got != "ok" {
		t.Fatalf("response header %q", got)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "echo:hello" {
		t.Fatalf("body %q", body)
	}
}

func TestChunkedBodyIsMaskedAndOversizeIsRejected(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	s, err := NewServer(Config{UpstreamURL: upstream.URL, MaxConcurrency: 2, MaxBodyBytes: 4, Engine: "go-fallback"}, testEngine{})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	request, _ := http.NewRequest(http.MethodPost, ts.URL+"/r/client/v1/chat", strings.NewReader("12345"))
	request.ContentLength = -1
	resp, err := ts.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}

func TestStreamingMaskingSessionTransformsRequestBeforeProxy(t *testing.T) {
	var upstreamBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		upstreamBody = string(body)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	pushes := 0
	engine := &streamingBodyEngine{pushes: &pushes}
	s, err := NewServer(Config{UpstreamURL: upstream.URL, MaxConcurrency: 1, MaxBodyBytes: 64, Engine: "go-fallback"}, engine)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/requests/chat", strings.NewReader("secret"))
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = -1
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || upstreamBody != "masked" {
		t.Fatalf("status=%d upstream_body=%q", resp.StatusCode, upstreamBody)
	}
	if pushes == 0 {
		t.Fatal("gateway did not use the masking session")
	}
}

func TestConcurrencyLimit(t *testing.T) {
	block := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-block; w.WriteHeader(http.StatusOK) }))
	defer upstream.Close()
	s, err := NewServer(Config{UpstreamURL: upstream.URL, MaxConcurrency: 1, Engine: "go-fallback"}, testEngine{})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	result := make(chan *http.Response)
	go func() { resp, _ := ts.Client().Get(ts.URL + "/api/requests/a"); result <- resp }()
	for i := 0; i < 100; i++ {
		if s.metrics.inFlight.Load() == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	resp2, err := ts.Client().Get(ts.URL + "/api/requests/b")
	if err != nil {
		t.Fatal(err)
	}
	if resp2.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status %d", resp2.StatusCode)
	}
	resp2.Body.Close()
	close(block)
	resp1 := <-result
	resp1.Body.Close()
	if got := s.metrics.rejected.Load(); got != 1 {
		t.Fatalf("rejected = %d", got)
	}
}

type failingEngine struct{}

type failingMaskEngine struct{}

type processOnlyEngine struct{}

type trackingReadCloser struct {
	io.Reader
	closed bool
}

func (r *trackingReadCloser) Close() error {
	r.closed = true
	return nil
}

func (failingMaskEngine) Mask(context.Context, []byte) ([]byte, error) {
	return nil, fmt.Errorf("mask failed")
}
func (failingMaskEngine) NewRestoreSession(context.Context) (RestoreSession, error) {
	return testRestore{}, nil
}

type testEngine struct{}

// contextualTestEngine models the native engine contract used by the
// extension endpoint: every mask request owns a private token map which its
// matching restore stream consumes.
type contextualTestEngine struct{}

type contextualTestMaskSession struct {
	masked    []byte
	originals map[string]string
}

type contextualTestRestore struct {
	masked    []byte
	originals map[string]string
}

func (contextualTestEngine) Mask(_ context.Context, body []byte) ([]byte, error) {
	return append([]byte(nil), body...), nil
}

func (contextualTestEngine) NewMaskingSession(context.Context) (MaskingSession, error) {
	return &contextualTestMaskSession{originals: make(map[string]string)}, nil
}

func (contextualTestEngine) NewRestoreSession(context.Context) (RestoreSession, error) {
	return testRestore{}, nil
}

func (s *contextualTestMaskSession) Push(_ context.Context, body []byte) ([]byte, error) {
	text := string(body)
	for _, original := range []string{"alice@example.com", "bob@example.com", "carol@example.com", "dave@example.com"} {
		for {
			pos := strings.Index(text, original)
			if pos < 0 {
				break
			}
			token := fmt.Sprintf("{{EMAIL_%016x}}", len(s.originals)+1)
			s.originals[token] = original
			text = text[:pos] + token + text[pos+len(original):]
		}
	}
	s.masked = []byte(text)
	return append([]byte(nil), s.masked...), nil
}

func (s *contextualTestMaskSession) Finish(context.Context) ([]byte, error) { return nil, nil }
func (s *contextualTestMaskSession) Abort()                                 {}
func (s *contextualTestMaskSession) Release()                               {}

func (s *contextualTestMaskSession) NewRestoreSession(context.Context) (RestoreSession, error) {
	copyMap := make(map[string]string, len(s.originals))
	for token, original := range s.originals {
		copyMap[token] = original
	}
	return &contextualTestRestore{originals: copyMap}, nil
}

func (s *contextualTestRestore) Push(_ context.Context, body []byte) ([]byte, error) {
	s.masked = append(s.masked, body...)
	return nil, nil
}

func (s *contextualTestRestore) Finish(context.Context) ([]byte, error) {
	text := string(s.masked)
	for token, original := range s.originals {
		text = strings.ReplaceAll(text, token, original)
	}
	return []byte(text), nil
}

func (s *contextualTestRestore) Abort() {}

type bodyMaskEngine struct{}

type streamingBodyEngine struct {
	pushes *int
}

func (testEngine) Mask(_ context.Context, body []byte) ([]byte, error) {
	return append([]byte(nil), body...), nil
}
func (testEngine) NewRestoreSession(context.Context) (RestoreSession, error) {
	return testRestore{}, nil
}

func (bodyMaskEngine) Mask(_ context.Context, body []byte) ([]byte, error) {
	return bytes.ReplaceAll(body, []byte("secret"), []byte("masked")), nil
}
func (bodyMaskEngine) NewRestoreSession(context.Context) (RestoreSession, error) {
	return testRestore{}, nil
}

func (e *streamingBodyEngine) Mask(_ context.Context, body []byte) ([]byte, error) {
	return bytes.ReplaceAll(body, []byte("secret"), []byte("masked")), nil
}
func (e *streamingBodyEngine) NewMaskingSession(context.Context) (MaskingSession, error) {
	return &streamingBodySession{pushes: e.pushes}, nil
}

type streamingBodySession struct {
	pushes *int
}

func (s *streamingBodySession) Push(_ context.Context, body []byte) ([]byte, error) {
	*s.pushes = *s.pushes + 1
	return bytes.ReplaceAll(body, []byte("secret"), []byte("masked")), nil
}
func (s *streamingBodySession) Finish(context.Context) ([]byte, error) { return nil, nil }
func (s *streamingBodySession) Abort()                                 {}

type failingStreamingMaskSession struct {
	pushes  int
	aborted bool
}

func (s *failingStreamingMaskSession) Push(_ context.Context, body []byte) ([]byte, error) {
	s.pushes++
	if s.pushes == 2 {
		return nil, errors.New("mask stream failed")
	}
	return bytes.ReplaceAll(body, []byte("secret"), []byte("masked")), nil
}
func (s *failingStreamingMaskSession) Finish(context.Context) ([]byte, error) { return nil, nil }
func (s *failingStreamingMaskSession) Abort()                                 { s.aborted = true }

type fixedChunkReadCloser struct {
	chunks [][]byte
	index  int
}

func (r *fixedChunkReadCloser) Read(dst []byte) (int, error) {
	if r.index >= len(r.chunks) {
		return 0, io.EOF
	}
	n := copy(dst, r.chunks[r.index])
	r.index++
	return n, nil
}
func (r *fixedChunkReadCloser) Close() error { return nil }

func TestMaskingReadCloserNeverReturnsSourceBytesAfterSessionFailure(t *testing.T) {
	session := &failingStreamingMaskSession{}
	body := &maskingReadCloser{
		source:   &fixedChunkReadCloser{chunks: [][]byte{[]byte("secret-first"), []byte("secret-second")}},
		session:  session,
		ctx:      context.Background(),
		maxBytes: 1024,
	}
	first := make([]byte, 64)
	n, err := body.Read(first)
	if err != nil {
		t.Fatalf("first read err=%v", err)
	}
	if got := string(first[:n]); got != "masked-first" || strings.Contains(got, "secret") {
		t.Fatalf("first output=%q", got)
	}
	second := make([]byte, 64)
	n, err = body.Read(second)
	if err == nil || n != 0 {
		t.Fatalf("failed read n=%d err=%v", n, err)
	}
	if !session.aborted {
		t.Fatal("mask session was not aborted")
	}
}

type testRestore struct{}

func (testRestore) Push(_ context.Context, body []byte) ([]byte, error) { return body, nil }
func (testRestore) Finish(context.Context) ([]byte, error)              { return nil, nil }
func (testRestore) Abort()                                              {}

type concurrentRestoreEngine struct {
	entered chan struct{}
	release chan struct{}
}

func (e *concurrentRestoreEngine) Mask(_ context.Context, body []byte) ([]byte, error) {
	return append([]byte(nil), body...), nil
}
func (e *concurrentRestoreEngine) NewRestoreSession(context.Context) (RestoreSession, error) {
	return &concurrentRestoreSession{entered: e.entered, release: e.release}, nil
}

type concurrentRestoreSession struct {
	entered chan struct{}
	release chan struct{}
}

func (s *concurrentRestoreSession) Push(_ context.Context, body []byte) ([]byte, error) {
	if string(body) == "first" {
		select {
		case <-s.entered:
		default:
			close(s.entered)
		}
		<-s.release
	}
	return body, nil
}
func (s *concurrentRestoreSession) Finish(context.Context) ([]byte, error) { return nil, nil }
func (s *concurrentRestoreSession) Abort()                                 {}

func TestEngineRequiredAndCustomEngine(t *testing.T) {
	if _, err := NewServer(Config{MaxConcurrency: 1}, nil); err == nil {
		t.Fatal("server accepted a missing masking engine")
	}
	e := &failingEngine{}
	s2, err := NewServer(Config{MaxConcurrency: 1}, e)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Engine() != e {
		t.Fatal("custom engine not retained")
	}
}

func TestShutdownDefersEngineCloseAfterTimeout(t *testing.T) {
	engine := &shutdownTrackingEngine{closedCh: make(chan struct{})}
	s, err := NewServer(Config{MaxConcurrency: 1, Engine: "go-fallback"}, engine)
	if err != nil {
		t.Fatal(err)
	}
	s.requests.begin() // model an in-flight handler that still owns the engine
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := s.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown error=%v, want deadline", err)
	}
	if atomic.LoadInt32(&engine.closed) != 0 {
		t.Fatal("engine closed while an in-flight request remained")
	}
	s.requests.end()
	select {
	case <-engine.closedCh:
	case <-time.After(time.Second):
		t.Fatal("engine was not closed after requests drained")
	}
}

func TestShutdownRejectsEngineReplacement(t *testing.T) {
	old := &shutdownTrackingEngine{closedCh: make(chan struct{})}
	s, err := NewServer(Config{MaxConcurrency: 1, Engine: "go-fallback"}, old)
	if err != nil {
		t.Fatal(err)
	}
	s.requests.begin()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := s.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown error=%v, want deadline", err)
	}
	newEngine := &shutdownTrackingEngine{closedCh: make(chan struct{})}
	if s.replaceEngine(newEngine) {
		t.Fatal("engine replacement succeeded after shutdown started")
	}
	select {
	case <-newEngine.closedCh:
	case <-time.After(time.Second):
		t.Fatal("rejected replacement was not closed")
	}
	s.requests.end()
	select {
	case <-old.closedCh:
	case <-time.After(time.Second):
		t.Fatal("old engine was not closed after requests drained")
	}
}

type shutdownTrackingEngine struct {
	closed   int32
	closedCh chan struct{}
}

func (e *shutdownTrackingEngine) Close() {
	if atomic.CompareAndSwapInt32(&e.closed, 0, 1) {
		close(e.closedCh)
	}
}

func TestMaskFailureIsFailClosed(t *testing.T) {
	called := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	s, err := NewServer(Config{UpstreamURL: upstream.URL, MaxConcurrency: 1, Engine: "go-fallback"}, failingMaskEngine{})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	resp, err := ts.Client().Post(ts.URL+"/api/requests/chat", "application/json", strings.NewReader(`{"secret":"value"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway || called {
		t.Fatalf("status=%d upstream_called=%v", resp.StatusCode, called)
	}
}

func TestExtensionMaskAndRestore(t *testing.T) {
	s, err := NewServer(Config{MaxConcurrency: 1, AuthToken: "secret", Engine: "go-fallback"}, contextualTestEngine{})
	if err != nil {
		t.Fatal(err)
	}
	maskReq := httptest.NewRequest(http.MethodPost, "/api/ext/mask", strings.NewReader(`{"text":"hello alice@example.com"}`))
	maskReq.Header.Set("X-Silencer-Token", "secret")
	maskReq.Header.Set("Content-Type", "application/json")
	maskResp := httptest.NewRecorder()
	s.Handler().ServeHTTP(maskResp, maskReq)
	var maskBody struct {
		Text string `json:"text"`
		SID  string `json:"sid"`
	}
	if err := json.Unmarshal(maskResp.Body.Bytes(), &maskBody); err != nil {
		t.Fatal(err)
	}
	if maskResp.Code != http.StatusOK || maskBody.SID == "" || !strings.Contains(maskBody.Text, "{{EMAIL_0000000000000001}}") {
		t.Fatalf("mask status=%d body=%s", maskResp.Code, maskResp.Body.String())
	}
	restorePayload, _ := json.Marshal(map[string]any{"sid": maskBody.SID, "stream_id": "s1", "text": maskBody.Text, "final": true})
	restoreReq := httptest.NewRequest(http.MethodPost, "/api/ext/restore", bytes.NewReader(restorePayload))
	restoreReq.Header.Set("X-Silencer-Token", "secret")
	restoreReq.Header.Set("Content-Type", "application/json")
	restoreResp := httptest.NewRecorder()
	s.Handler().ServeHTTP(restoreResp, restoreReq)
	if restoreResp.Code != http.StatusOK || !strings.Contains(restoreResp.Body.String(), `"text":"hello alice@example.com"`) {
		t.Fatalf("restore status=%d body=%s", restoreResp.Code, restoreResp.Body.String())
	}
}

func TestExtensionEventsAppearInIngressStats(t *testing.T) {
	t.Setenv("SILENCER_STORAGE_MODE", "sqlite")
	t.Setenv("SILENCER_SQLITE_PATH", filepath.Join(t.TempDir(), "extension-stats.db"))
	store, err := storage.OpenFromEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s, err := NewServer(Config{MaxConcurrency: 1, AuthToken: "secret", Engine: "go-fallback"}, contextualTestEngine{})
	if err != nil {
		t.Fatal(err)
	}
	s.SetStorage(store)
	defer s.Shutdown(context.Background())

	maskReq := httptest.NewRequest(http.MethodPost, "/api/ext/mask", strings.NewReader(`{"text":"hello alice@example.com"}`))
	maskReq.Header.Set("X-Silencer-Token", "secret")
	maskReq.Header.Set("Content-Type", "application/json")
	maskResp := httptest.NewRecorder()
	s.Handler().ServeHTTP(maskResp, maskReq)
	if maskResp.Code != http.StatusOK {
		t.Fatalf("mask status=%d body=%s", maskResp.Code, maskResp.Body.String())
	}
	var maskBody struct {
		Text string `json:"text"`
		SID  string `json:"sid"`
	}
	if err := json.Unmarshal(maskResp.Body.Bytes(), &maskBody); err != nil {
		t.Fatal(err)
	}
	if maskBody.SID == "" || !strings.Contains(maskBody.Text, "{{EMAIL_0000000000000001}}") {
		t.Fatalf("mask body=%s", maskResp.Body.String())
	}
	restorePayload, _ := json.Marshal(map[string]any{"sid": maskBody.SID, "stream_id": "stats", "text": maskBody.Text, "final": true})
	restoreReq := httptest.NewRequest(http.MethodPost, "/api/ext/restore", bytes.NewReader(restorePayload))
	restoreReq.Header.Set("X-Silencer-Token", "secret")
	restoreReq.Header.Set("Content-Type", "application/json")
	restoreResp := httptest.NewRecorder()
	s.Handler().ServeHTTP(restoreResp, restoreReq)
	if restoreResp.Code != http.StatusOK {
		t.Fatalf("restore status=%d body=%s", restoreResp.Code, restoreResp.Body.String())
	}

	statsReq := httptest.NewRequest(http.MethodGet, "/api/stats/today", nil)
	statsReq.Header.Set("X-Silencer-Token", "secret")
	statsResp := httptest.NewRecorder()
	s.Handler().ServeHTTP(statsResp, statsReq)
	if statsResp.Code != http.StatusOK {
		t.Fatalf("stats status=%d body=%s", statsResp.Code, statsResp.Body.String())
	}
	var stats struct {
		MaskedItems    int `json:"masked_items"`
		WordsByIngress map[string]struct {
			LabelTotal int `json:"label_total"`
		} `json:"words_by_ingress"`
	}
	if err := json.Unmarshal(statsResp.Body.Bytes(), &stats); err != nil {
		t.Fatal(err)
	}
	if stats.MaskedItems != 1 || stats.WordsByIngress["ext"].LabelTotal != 1 {
		t.Fatalf("stats=%s", statsResp.Body.String())
	}
}

func TestExtensionRestoreRecordsUnresolvedPlaceholders(t *testing.T) {
	t.Setenv("SILENCER_STORAGE_MODE", "sqlite")
	t.Setenv("SILENCER_SQLITE_PATH", filepath.Join(t.TempDir(), "extension-restore.db"))
	store, err := storage.OpenFromEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s, err := NewServer(Config{MaxConcurrency: 1, AuthToken: "secret", Engine: "go-fallback"}, contextualTestEngine{})
	if err != nil {
		t.Fatal(err)
	}
	s.SetStorage(store)
	defer s.Shutdown(context.Background())

	maskReq := httptest.NewRequest(http.MethodPost, "/api/ext/mask", strings.NewReader(`{"text":"hello alice@example.com"}`))
	maskReq.Header.Set("X-Silencer-Token", "secret")
	maskReq.Header.Set("Content-Type", "application/json")
	maskResp := httptest.NewRecorder()
	s.Handler().ServeHTTP(maskResp, maskReq)
	var maskBody struct {
		SID string `json:"sid"`
	}
	if err := json.Unmarshal(maskResp.Body.Bytes(), &maskBody); err != nil || maskBody.SID == "" {
		t.Fatalf("mask response=%s err=%v", maskResp.Body.String(), err)
	}

	restorePayload := fmt.Sprintf(`{"sid":%q,"stream_id":"unresolved","text":"{{EMAIL_0000000000000000}}","final":true}`, maskBody.SID)
	restoreReq := httptest.NewRequest(http.MethodPost, "/api/ext/restore", strings.NewReader(restorePayload))
	restoreReq.Header.Set("X-Silencer-Token", "secret")
	restoreReq.Header.Set("Content-Type", "application/json")
	restoreResp := httptest.NewRecorder()
	s.Handler().ServeHTTP(restoreResp, restoreReq)
	if restoreResp.Code != http.StatusOK || !strings.Contains(restoreResp.Body.String(), "0000000000000000") {
		t.Fatalf("restore status=%d body=%s", restoreResp.Code, restoreResp.Body.String())
	}
	events, err := store.ListAllEvents(context.Background(), 20)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type != "RESTORE" {
			continue
		}
		if events[i].Status != "unresolved" {
			t.Fatalf("restore status=%q meta=%s", events[i].Status, events[i].Meta)
		}
		var meta map[string]any
		if err := json.Unmarshal([]byte(events[i].Meta), &meta); err != nil {
			t.Fatal(err)
		}
		if meta["restored"] != float64(0) || meta["unresolved"] != float64(1) {
			t.Fatalf("restore meta=%v", meta)
		}
		return
	}
	t.Fatal("RESTORE event not found")
}

func TestExtensionContextualMaskingKeepsCacheBytesAndOwnsRestoreMap(t *testing.T) {
	s, err := NewServer(Config{MaxConcurrency: 2, AuthToken: "secret", Engine: "go-fallback"}, contextualTestEngine{})
	if err != nil {
		t.Fatal(err)
	}
	mask := func(text string) (string, string) {
		req := httptest.NewRequest(http.MethodPost, "/api/ext/mask", strings.NewReader(fmt.Sprintf(`{"text":%q}`, text)))
		req.Header.Set("X-Silencer-Token", "secret")
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("mask status=%d body=%s", rr.Code, rr.Body.String())
		}
		var body struct {
			Text string `json:"text"`
			SID  string `json:"sid"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body.Text, body.SID
	}
	maskedAlice, sidAlice := mask("contact alice@example.com")
	maskedBob, sidBob := mask("contact bob@example.com")
	if maskedAlice != maskedBob || !strings.Contains(maskedAlice, "{{EMAIL_0000000000000001}}") {
		t.Fatalf("masked bytes are not stable: alice=%q bob=%q", maskedAlice, maskedBob)
	}
	if sidAlice == "" || sidBob == "" || sidAlice == sidBob {
		t.Fatalf("mask sessions are not request-scoped: alice=%q bob=%q", sidAlice, sidBob)
	}
	restore := func(sid, streamID, text string) string {
		req := httptest.NewRequest(http.MethodPost, "/api/ext/restore", strings.NewReader(fmt.Sprintf(`{"sid":%q,"stream_id":%q,"text":%q,"final":true}`, sid, streamID, text)))
		req.Header.Set("X-Silencer-Token", "secret")
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("restore status=%d body=%s", rr.Code, rr.Body.String())
		}
		var body struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body.Text
	}
	if got := restore(sidAlice, "alice-stream", maskedAlice); got != "contact alice@example.com" {
		t.Fatalf("alice restore=%q", got)
	}
	if got := restore(sidBob, "bob-stream", maskedBob); got != "contact bob@example.com" {
		t.Fatalf("bob restore=%q", got)
	}
}

func TestExtensionBridgeGeneratesAndAuthenticatesToken(t *testing.T) {
	s, err := NewServer(Config{MaxConcurrency: 2, AuthToken: "admin", Engine: "go-fallback"}, testEngine{})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	client := ts.Client()

	patchReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/config", strings.NewReader(`{"ext_bridge_enabled":true}`))
	patchReq.Header.Set("X-Silencer-Token", "admin")
	patchReq.Header.Set("Content-Type", "application/json")
	patchResp, err := client.Do(patchReq)
	if err != nil {
		t.Fatal(err)
	}
	var patchBody struct {
		Config map[string]any `json:"config"`
	}
	if err := json.NewDecoder(patchResp.Body).Decode(&patchBody); err != nil {
		t.Fatal(err)
	}
	patchResp.Body.Close()
	token, _ := patchBody.Config["ext_token"].(string)
	if len(token) < 32 {
		t.Fatalf("generated token=%q", token)
	}

	pingReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/ext/ping", nil)
	pingReq.Header.Set("X-Silencer-Token", token)
	pingResp, err := client.Do(pingReq)
	if err != nil {
		t.Fatal(err)
	}
	if pingResp.StatusCode != http.StatusOK || !strings.Contains(readBody(t, pingResp), `"ok":true`) {
		t.Fatalf("ping status=%d", pingResp.StatusCode)
	}

	badReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/ext/mask", strings.NewReader(`{"text":"hello"}`))
	badReq.Header.Set("X-Silencer-Token", "wrong")
	badReq.Header.Set("Content-Type", "application/json")
	badResp, err := client.Do(badReq)
	if err != nil {
		t.Fatal(err)
	}
	if badResp.StatusCode != http.StatusForbidden || !strings.Contains(readBody(t, badResp), "invalid_token") {
		t.Fatalf("bad token status=%d", badResp.StatusCode)
	}

	rotateReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/ext/rotate-token", nil)
	rotateReq.Header.Set("X-Silencer-Token", "admin")
	rotateResp, err := client.Do(rotateReq)
	if err != nil {
		t.Fatal(err)
	}
	var rotateBody struct {
		Token string `json:"ext_token"`
	}
	if err := json.NewDecoder(rotateResp.Body).Decode(&rotateBody); err != nil {
		t.Fatal(err)
	}
	rotateResp.Body.Close()
	if rotateResp.StatusCode != http.StatusOK || rotateBody.Token == "" || rotateBody.Token == token {
		t.Fatalf("rotate status=%d token changed=%v", rotateResp.StatusCode, rotateBody.Token != token)
	}
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAuthStatus(t *testing.T) {
	s, err := NewServer(Config{MaxConcurrency: 1, AuthToken: "secret"}, testEngine{})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/auth/status", nil)
	s.Handler().ServeHTTP(r, req)
	if !strings.Contains(r.Body.String(), `"authenticated":false`) {
		t.Fatalf("body %s", r.Body.String())
	}
	r = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/auth/status", nil)
	req.Header.Set("Authorization", "Bearer secret")
	s.Handler().ServeHTTP(r, req)
	if !strings.Contains(r.Body.String(), `"authenticated":true`) {
		t.Fatalf("body %s", r.Body.String())
	}
}

func TestControlPlaneV2Contracts(t *testing.T) {
	s, err := NewServer(Config{MaxConcurrency: 2, AuthToken: "secret", Engine: "go-fallback"}, testEngine{})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/status"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Silencer-Token", "secret")
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", path, rr.Code, rr.Body.String())
		}
	}
	for _, path := range []string{"/api/clients/discover", "/api/ext/rotate-token"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Silencer-Token", "secret")
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", path, rr.Code, rr.Body.String())
		}
	}
	discoverReq := httptest.NewRequest(http.MethodGet, "/api/clients/discover", nil)
	discoverReq.Header.Set("X-Silencer-Token", "secret")
	discoverResp := httptest.NewRecorder()
	s.Handler().ServeHTTP(discoverResp, discoverReq)
	for _, want := range []string{`"available":false`, `"scope":"gateway"`, `"capability":"host_agent_unavailable"`} {
		if !strings.Contains(discoverResp.Body.String(), want) {
			t.Fatalf("discover missing %q: %s", want, discoverResp.Body.String())
		}
	}
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		req := httptest.NewRequest(method, "/api/clients/apply", nil)
		req.Header.Set("X-Silencer-Token", "secret")
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusMethodNotAllowed {
			t.Fatalf("clients/apply %s status=%d body=%s", method, rr.Code, rr.Body.String())
		}
	}
	applyReq := httptest.NewRequest(http.MethodPost, "/api/clients/apply", strings.NewReader(`{"plan":{}}`))
	applyReq.Header.Set("X-Silencer-Token", "secret")
	applyResp := httptest.NewRecorder()
	s.Handler().ServeHTTP(applyResp, applyReq)
	if applyResp.Code != http.StatusConflict || !strings.Contains(applyResp.Body.String(), `"available":false`) || !strings.Contains(applyResp.Body.String(), `"capability":"host_agent_unavailable"`) {
		t.Fatalf("clients/apply unavailable contract status=%d body=%s", applyResp.Code, applyResp.Body.String())
	}
	for _, path := range []string{"/api/audit/job", "/api/prices/list"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Silencer-Token", "secret")
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", path, rr.Code, rr.Body.String())
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/api/stats/today", nil)
	req.Header.Set("X-Silencer-Token", "secret")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("stats status=%d body=%s", rr.Code, rr.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/api/logs", nil)
	req.Header.Set("X-Silencer-Token", "secret")
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("logs status=%d body=%s", rr.Code, rr.Body.String())
	}
	req = httptest.NewRequest(http.MethodPost, "/api/config/patch", strings.NewReader(`{"key":"audit","op":"set","path":["enabled"],"value":true}`))
	req.Header.Set("X-Silencer-Token", "secret")
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"enabled":true`) {
		t.Fatalf("config patch status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestReadOnlyControlEndpointsRejectWrites(t *testing.T) {
	s, err := NewServer(Config{MaxConcurrency: 4, Engine: "go-fallback"}, testEngine{})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/logs/export", "/api/logs/detail?id=x", "/api/audit/events"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s status=%d body=%s", path, rr.Code, rr.Body.String())
		}
	}
}

func TestLogsExposeCursorFiltersAndSafeDetail(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SILENCER_STORAGE_MODE", "sqlite")
	t.Setenv("SILENCER_SQLITE_PATH", filepath.Join(dir, "logs.db"))
	store, err := storage.OpenFromEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	first := time.Now().UTC().Add(-time.Second)
	if err := store.AppendEvent(context.Background(), storage.Event{
		ID: "mask-event", RequestID: "request-1", Type: "MASK", Payload: []byte("secret payload"),
		Masked: []byte("{{SECRET_abc123}}"), Restored: []byte("plaintext response"),
		Meta:      `{"sid":"request-1","ingress":"proxy","count":1,"path":"/v1/chat","items":[{"label":"SECRET","preview":"{{SECRET_abc123}}","original":"sk-live-super-secret"}]}`,
		CreatedAt: first,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendEvent(context.Background(), storage.Event{
		ID: "skip-event", RequestID: "request-2", Type: "SKIP", Payload: []byte("noise"),
		Meta: `{"ingress":"ext"}`, CreatedAt: first.Add(time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendEvent(context.Background(), storage.Event{
		ID: "restore-event", RequestID: "request-1", Type: "RESTORE", Status: "no_sensitive_data", Payload: []byte("POST /v1/chat"),
		Meta: `{"sid":"request-1","ingress":"proxy","restored":0,"status":"no_sensitive_data"}`, CreatedAt: first.Add(1500 * time.Millisecond),
	}); err != nil {
		t.Fatal(err)
	}
	s, err := NewServer(Config{MaxConcurrency: 2, Engine: "go-fallback"}, testEngine{})
	if err != nil {
		t.Fatal(err)
	}
	s.SetStorage(store)
	defer s.Shutdown(context.Background())
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	resp, err := ts.Client().Get(ts.URL + "/api/logs?type=MASK&q=chat")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Events []map[string]any `json:"events"`
		Next   int64            `json:"next_since"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	listJSON, _ := json.Marshal(body)
	if resp.StatusCode != http.StatusOK || len(body.Events) != 1 || body.Next == 0 {
		t.Fatalf("status=%d body=%+v", resp.StatusCode, body)
	}
	if !strings.Contains(string(listJSON), "sk-live-super-secret") {
		t.Fatalf("log list did not expose original item: %s", listJSON)
	}
	seq, ok := body.Events[0]["seq"].(float64)
	if !ok || int64(seq) != body.Next {
		t.Fatalf("cursor event=%+v next=%d", body.Events[0], body.Next)
	}
	detail, err := ts.Client().Get(ts.URL + "/api/logs/detail?seq=" + strconv.FormatInt(body.Next, 10))
	if err != nil {
		t.Fatal(err)
	}
	defer detail.Body.Close()
	var detailBody map[string]any
	if err := json.NewDecoder(detail.Body).Decode(&detailBody); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(detailBody)
	if detail.StatusCode != http.StatusOK || !strings.Contains(string(encoded), `"req_preview":"{{SECRET_abc123}}"`) || !strings.Contains(string(encoded), "sk-live-super-secret") || strings.Contains(string(encoded), "plaintext response") || strings.Contains(string(encoded), "secret payload") {
		t.Fatalf("unsafe or incomplete detail status=%d body=%s", detail.StatusCode, encoded)
	}
	if err := store.AppendEvent(context.Background(), storage.Event{
		ID: "noop-mask-event", RequestID: "request-noop", Type: "MASK", Payload: []byte("POST /v1/chat"),
		Meta: `{"sid":"request-noop","ingress":"proxy","count":0,"path":"/v1/chat"}`, CreatedAt: first.Add(2 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendEvent(context.Background(), storage.Event{
		ID: "noop-restore-event", RequestID: "request-noop", Type: "RESTORE", Status: "no_sensitive_data", Payload: []byte("POST /v1/chat"),
		Meta: `{"sid":"request-noop","ingress":"proxy","restored":0,"status":"no_sensitive_data"}`, CreatedAt: first.Add(3 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	filtered, err := ts.Client().Get(ts.URL + "/api/logs?sensitive=1")
	if err != nil {
		t.Fatal(err)
	}
	defer filtered.Body.Close()
	var filteredBody struct {
		Events []struct {
			ID     string `json:"detail_id"`
			Status string `json:"status"`
		} `json:"events"`
	}
	if err := json.NewDecoder(filtered.Body).Decode(&filteredBody); err != nil {
		t.Fatal(err)
	}
	for _, event := range filteredBody.Events {
		if event.ID == "noop-mask-event" || event.ID == "noop-restore-event" || event.Status == "no_sensitive_data" {
			t.Fatalf("sensitive filter returned no-op event: %+v", filteredBody.Events)
		}
	}
	foundRestore := false
	for _, event := range filteredBody.Events {
		if event.ID == "restore-event" {
			foundRestore = true
			if event.Status != "no_placeholder_in_response" {
				t.Fatalf("paired restore status was not normalized: %+v", event)
			}
		}
	}
	if !foundRestore {
		t.Fatalf("sensitive filter dropped paired restore: %+v", filteredBody.Events)
	}
}

func TestAuditEventsExposeMetadataAndApplyServerFilters(t *testing.T) {
	dir := t.TempDir()
	oldMode, oldPath := os.Getenv("SILENCER_STORAGE_MODE"), os.Getenv("SILENCER_SQLITE_PATH")
	defer os.Setenv("SILENCER_STORAGE_MODE", oldMode)
	defer os.Setenv("SILENCER_SQLITE_PATH", oldPath)
	os.Setenv("SILENCER_STORAGE_MODE", "sqlite")
	os.Setenv("SILENCER_SQLITE_PATH", filepath.Join(dir, "audit.db"))
	store, err := storage.OpenFromEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	if err := store.AppendEvent(context.Background(), storage.Event{ID: "audit-1", RequestID: "req-1", Type: "AUDIT_error_leak", Payload: []byte{}, Meta: `{"signal_type":"error_leak","severity":"CRITICAL","evidence":"credential redacted","probe_id":"probe_1"}`, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendEvent(context.Background(), storage.Event{ID: "audit-2", RequestID: "req-2", Type: "AUDIT_stream_anomaly", Payload: []byte{}, Meta: `{"signal_type":"stream_anomaly","severity":"LOW","evidence":"diagnostic"}`, CreatedAt: now.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	for _, event := range []storage.Event{
		{ID: "operational-mask", RequestID: "req-3", Type: "MASK", Payload: []byte("POST /v1/chat/completions"), Meta: `{"method":"POST","path":"/v1/chat/completions"}`, CreatedAt: now.Add(2 * time.Second)},
		{ID: "operational-restore", RequestID: "req-3", Type: "RESTORE", Payload: []byte("POST /v1/chat/completions"), Meta: `{"method":"POST","path":"/v1/chat/completions"}`, CreatedAt: now.Add(3 * time.Second)},
		{ID: "legacy-audit", RequestID: "req-4", Type: "AUDIT", Payload: []byte("legacy finding"), Meta: `{"severity":"LOW"}`, CreatedAt: now.Add(4 * time.Second)},
	} {
		if err := store.AppendEvent(context.Background(), event); err != nil {
			t.Fatal(err)
		}
	}
	s, err := NewServer(Config{MaxConcurrency: 1, Engine: "go-fallback"}, testEngine{})
	if err != nil {
		t.Fatal(err)
	}
	s.SetStorage(store)
	defer s.Shutdown(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/api/audit/events?floor=HIGH&signal=error_leak", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"signal_type":"error_leak"`) || !strings.Contains(rr.Body.String(), `"severity":"CRITICAL"`) || strings.Contains(rr.Body.String(), "stream_anomaly") {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	all := httptest.NewRecorder()
	s.Handler().ServeHTTP(all, httptest.NewRequest(http.MethodGet, "/api/audit/events", nil))
	var response struct {
		Events []map[string]any `json:"events"`
	}
	if err := json.Unmarshal(all.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if all.Code != http.StatusOK || len(response.Events) != 3 {
		t.Fatalf("audit endpoint included operational events: status=%d events=%s", all.Code, all.Body.String())
	}
	for _, event := range response.Events {
		if typ, _ := event["type"].(string); typ != "AUDIT" && !strings.HasPrefix(typ, "AUDIT_") {
			t.Fatalf("non-audit event returned from audit endpoint: %s", all.Body.String())
		}
	}
	bad := httptest.NewRecorder()
	s.Handler().ServeHTTP(bad, httptest.NewRequest(http.MethodGet, "/api/audit/events?floor=UNKNOWN", nil))
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("invalid floor status=%d body=%s", bad.Code, bad.Body.String())
	}
}

func TestPassiveErrorLeakAuditStoresRedactedEvidence(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "upstream error: Bearer abcdefghijklmnopqrstuvwxyz123456")
	}))
	defer upstream.Close()
	dir := t.TempDir()
	oldMode, oldPath := os.Getenv("SILENCER_STORAGE_MODE"), os.Getenv("SILENCER_SQLITE_PATH")
	defer os.Setenv("SILENCER_STORAGE_MODE", oldMode)
	defer os.Setenv("SILENCER_SQLITE_PATH", oldPath)
	os.Setenv("SILENCER_STORAGE_MODE", "sqlite")
	os.Setenv("SILENCER_SQLITE_PATH", filepath.Join(dir, "audit.db"))
	store, err := storage.OpenFromEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s, err := NewServer(Config{UpstreamURL: upstream.URL, MaxConcurrency: 1, Engine: "go-fallback"}, replaceRestoreEngine{})
	if err != nil {
		t.Fatal(err)
	}
	s.SetStorage(store)
	defer s.Shutdown(context.Background())
	s.configMu.Lock()
	audit := s.control["audit"].(map[string]any)
	audit["enabled"] = true
	audit["passive"] = true
	audit["signals"] = map[string]any{"error_leak": true}
	s.configMu.Unlock()
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	resp, err := ts.Client().Get(ts.URL + "/api/requests/error")
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil || resp.StatusCode != http.StatusBadGateway || !bytes.Contains(body, []byte("Bearer")) {
		t.Fatalf("status=%d body=%q err=%v", resp.StatusCode, body, readErr)
	}
	var events []storage.Event
	for i := 0; i < 100; i++ {
		events, err = store.ListAuditEvents(context.Background(), 10)
		if err == nil && len(events) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil || len(events) != 1 {
		t.Fatalf("events=%+v err=%v", events, err)
	}
	if strings.Contains(events[0].Meta, "abcdefghijklmnopqrstuvwxyz123456") || !strings.Contains(events[0].Meta, "sha256=") {
		t.Fatalf("audit evidence leaked credential: %s", events[0].Meta)
	}
	clearReq := httptest.NewRequest(http.MethodPost, "/api/audit/clear", nil)
	clearResp := httptest.NewRecorder()
	s.Handler().ServeHTTP(clearResp, clearReq)
	if clearResp.Code != http.StatusOK {
		t.Fatalf("audit clear status=%d body=%s", clearResp.Code, clearResp.Body.String())
	}
	events, err = store.ListAuditEvents(context.Background(), 10)
	if err != nil || len(events) != 0 {
		t.Fatalf("audit events remain after clear: events=%+v err=%v", events, err)
	}
}

type replaceRestoreEngine struct{}

func (replaceRestoreEngine) NewRestoreSession(context.Context) (RestoreSession, error) {
	return &replaceRestoreSession{}, nil
}

type replaceRestoreSession struct {
	pending []byte
}

func (s *replaceRestoreSession) Push(_ context.Context, body []byte) ([]byte, error) {
	s.pending = append(s.pending, body...)
	if !bytes.Contains(s.pending, []byte("{{TOKEN}}")) {
		return nil, nil
	}
	out := bytes.ReplaceAll(s.pending, []byte("{{TOKEN}}"), []byte("secret-value"))
	s.pending = nil
	return out, nil
}
func (s *replaceRestoreSession) Finish(context.Context) ([]byte, error) {
	out := bytes.ReplaceAll(s.pending, []byte("{{TOKEN}}"), []byte("secret-value"))
	s.pending = nil
	return out, nil
}
func (s *replaceRestoreSession) Abort() { s.pending = nil }

func TestGzipJSONResponseIsRestoredAndDecompressed(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body bytes.Buffer
		zw := gzip.NewWriter(&body)
		_, _ = zw.Write([]byte(`{"value":"{{TOKEN}}"}`))
		_ = zw.Close()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body.Bytes())
	}))
	defer upstream.Close()
	s, err := NewServer(Config{UpstreamURL: upstream.URL, MaxConcurrency: 1, Engine: "go-fallback"}, replaceRestoreEngine{})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	resp, err := ts.Client().Get(ts.URL + "/api/requests/test")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != `{"value":"secret-value"}` {
		t.Fatalf("body=%q", got)
	}
	if got := resp.Header.Get("Content-Encoding"); got != "" {
		t.Fatalf("content encoding=%q", got)
	}
}

func TestGzipSSEResponseIsRestoredAndDecompressed(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body bytes.Buffer
		zw := gzip.NewWriter(&body)
		_, _ = zw.Write([]byte("data: {\"text\":\"{{TOKEN}}\"}\n\n"))
		_ = zw.Close()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body.Bytes())
	}))
	defer upstream.Close()
	s, err := NewServer(Config{UpstreamURL: upstream.URL, MaxConcurrency: 1, Engine: "go-fallback"}, replaceRestoreEngine{})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	resp, err := ts.Client().Get(ts.URL + "/api/requests/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	want := "data: {\"text\":\"secret-value\"}\n\n"
	if resp.StatusCode != http.StatusOK || string(data) != want {
		t.Fatalf("status=%d body=%q want=%q", resp.StatusCode, data, want)
	}
	if got := resp.Header.Get("Content-Encoding"); got != "" {
		t.Fatalf("content encoding=%q", got)
	}
}

func TestBrotliJSONResponseIsRestoredAndDecompressed(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body bytes.Buffer
		bw := brotli.NewWriter(&body)
		_, _ = bw.Write([]byte(`{"value":"{{TOKEN}}"}`))
		_ = bw.Close()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "br")
		_, _ = w.Write(body.Bytes())
	}))
	defer upstream.Close()
	s, err := NewServer(Config{UpstreamURL: upstream.URL, MaxConcurrency: 1, Engine: "go-fallback"}, replaceRestoreEngine{})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	resp, err := ts.Client().Get(ts.URL + "/api/requests/test")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || string(body) != `{"value":"secret-value"}` {
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Content-Encoding"); got != "" {
		t.Fatalf("content encoding=%q", got)
	}
}

func TestBrotliSSEResponseIsRestoredAndDecompressed(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body bytes.Buffer
		bw := brotli.NewWriter(&body)
		_, _ = bw.Write([]byte("data: {\"text\":\"{{TOKEN}}\"}\n\n"))
		_ = bw.Close()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "br")
		w.Header().Set("Content-Length", strconv.Itoa(body.Len()))
		_, _ = w.Write(body.Bytes())
	}))
	defer upstream.Close()
	s, err := NewServer(Config{UpstreamURL: upstream.URL, MaxConcurrency: 1, Engine: "go-fallback"}, replaceRestoreEngine{})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	resp, err := ts.Client().Get(ts.URL + "/api/requests/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	want := "data: {\"text\":\"secret-value\"}\n\n"
	if resp.StatusCode != http.StatusOK || string(body) != want {
		t.Fatalf("status=%d body=%q want=%q", resp.StatusCode, body, want)
	}
	if got := resp.Header.Get("Content-Encoding"); got != "" {
		t.Fatalf("content encoding=%q", got)
	}
	if got := resp.Header.Get("Content-Length"); got != "" {
		t.Fatalf("content length=%q", got)
	}
}

func TestBrotliRequestIsDecodedBeforeMasking(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"value":"masked"}` {
			t.Errorf("upstream body=%q", body)
		}
		if got := r.Header.Get("Content-Encoding"); got != "" {
			t.Errorf("content encoding=%q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()
	engine := bodyMaskEngine{}
	s, err := NewServer(Config{UpstreamURL: upstream.URL, MaxConcurrency: 1, Engine: "go-fallback"}, engine)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	var body bytes.Buffer
	bw := brotli.NewWriter(&body)
	_, _ = bw.Write([]byte(`{"value":"secret"}`))
	_ = bw.Close()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/requests/test", &body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "br")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}

func TestMaskedTokenItemsFromPairReturnsPlaintextWords(t *testing.T) {
	raw := []byte(`ip=203.0.113.42 email=alice@example.com secret=sk-live-abcdef`)
	masked := []byte(`ip={{IP_PUBLIC_aaaa1111aaaa1111}} email={{EMAIL_bbbb2222bbbb2222}} secret={{API_KEY_cccc3333cccc3333}}`)
	items := maskedTokenItemsFromPair(raw, masked)
	if len(items) != 3 {
		t.Fatalf("items=%+v", items)
	}
	want := []string{"203.0.113.42", "alice@example.com", "sk-live-abcdef"}
	for i, value := range want {
		if got, _ := items[i]["original"].(string); got != value {
			t.Fatalf("item[%d]=%+v original=%q want=%q", i, items[i], got, value)
		}
		if got, _ := items[i]["display"].(string); got != "plaintext" {
			t.Fatalf("item[%d] display=%q", i, got)
		}
	}
}

func TestDeflateJSONResponseSupportsZlibAndRawStreams(t *testing.T) {
	for _, wrapped := range []bool{true, false} {
		t.Run(map[bool]string{true: "zlib", false: "raw"}[wrapped], func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body bytes.Buffer
				var writer io.WriteCloser
				var err error
				if wrapped {
					writer = zlib.NewWriter(&body)
				} else {
					writer, err = flate.NewWriter(&body, flate.DefaultCompression)
					if err != nil {
						t.Fatalf("flate writer: %v", err)
					}
				}
				_, _ = writer.Write([]byte(`{"value":"{{TOKEN}}"}`))
				_ = writer.Close()
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Content-Encoding", "deflate")
				_, _ = w.Write(body.Bytes())
			}))
			defer upstream.Close()
			s, err := NewServer(Config{UpstreamURL: upstream.URL, MaxConcurrency: 1, Engine: "go-fallback"}, replaceRestoreEngine{})
			if err != nil {
				t.Fatal(err)
			}
			ts := httptest.NewServer(s.Handler())
			defer ts.Close()
			resp, err := ts.Client().Get(ts.URL + "/api/requests/test")
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			data, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusOK || string(data) != `{"value":"secret-value"}` {
				t.Fatalf("status=%d body=%q", resp.StatusCode, data)
			}
			if got := resp.Header.Get("Content-Encoding"); got != "" {
				t.Fatalf("content encoding=%q", got)
			}
		})
	}
}

func TestRestoredResponseDropsMaskedContentLength(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body := []byte(`{"value":"{{TOKEN}}"}`)
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	}))
	defer upstream.Close()
	s, err := NewServer(Config{UpstreamURL: upstream.URL, MaxConcurrency: 1, Engine: "go-fallback"}, replaceRestoreEngine{})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	resp, err := ts.Client().Get(ts.URL + "/api/requests/test")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != `{"value":"secret-value"}` {
		t.Fatalf("restored body=%q", got)
	}
}

func TestBinaryResponseWithoutContentTypeIsNotRestored(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := []byte{0x00, 0x7b, 0x7b, 0x54, 0x4f, 0x4b, 0x45, 0x4e, 0x7d, 0x7d, 0xff}
		w.Header()["Content-Type"] = nil
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	}))
	defer upstream.Close()
	s, err := NewServer(Config{UpstreamURL: upstream.URL, MaxConcurrency: 1, Engine: "go-fallback"}, replaceRestoreEngine{})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	resp, err := ts.Client().Get(ts.URL + "/api/requests/binary")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{0x00, 0x7b, 0x7b, 0x54, 0x4f, 0x4b, 0x45, 0x4e, 0x7d, 0x7d, 0xff}
	if resp.StatusCode != http.StatusOK || !bytes.Equal(got, want) {
		t.Fatalf("status=%d body=%v", resp.StatusCode, got)
	}
}

func TestGzipRequestIsDecodedBeforeMasking(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if got := string(body); got != `{"value":"masked"}` {
			t.Errorf("upstream body=%q", got)
		}
		if got := r.Header.Get("Content-Encoding"); got != "" {
			t.Errorf("content encoding=%q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()
	engine := bodyMaskEngine{}
	s, err := NewServer(Config{UpstreamURL: upstream.URL, MaxConcurrency: 1, Engine: "go-fallback"}, engine)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	var body bytes.Buffer
	zw := gzip.NewWriter(&body)
	_, _ = zw.Write([]byte(`{"value":"secret"}`))
	_ = zw.Close()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/requests/test", &body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}

func TestSSEAndNDJSONRestoreAcrossChunks(t *testing.T) {
	for _, contentType := range []string{"text/event-stream", "application/x-ndjson"} {
		t.Run(contentType, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", contentType)
				flusher, _ := w.(http.Flusher)
				_, _ = io.WriteString(w, `data: {"text":"{{TO`)
				flusher.Flush()
				_, _ = io.WriteString(w, `KEN}}"}`+"\n\n")
				flusher.Flush()
			}))
			defer upstream.Close()
			s, err := NewServer(Config{UpstreamURL: upstream.URL, MaxConcurrency: 1, Engine: "go-fallback"}, replaceRestoreEngine{})
			if err != nil {
				t.Fatal(err)
			}
			ts := httptest.NewServer(s.Handler())
			defer ts.Close()
			resp, err := ts.Client().Get(ts.URL + "/api/requests/stream")
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "secret-value") {
				t.Fatalf("status=%d body=%q", resp.StatusCode, body)
			}
		})
	}
}

func TestRestorePlaceholderCountsUseFinalResponse(t *testing.T) {
	upstream := []byte(`{"text":"{{EMAIL_aaaaaaaaaaaaaaaa}} {{SECRET_bbbbbbbbbbbbbbbb}}"}`)
	final := []byte(`{"text":"alice@example.com {{SECRET_bbbbbbbbbbbbbbbb}}"}`)
	total, restored, unresolved := restorePlaceholderCounts(upstream, final)
	if total != 2 || restored != 1 || unresolved != 1 {
		t.Fatalf("restore counts total=%d restored=%d unresolved=%d", total, restored, unresolved)
	}
	if got := inferredRestoreStatus(storage.Request{Upstream: upstream, Restored: final}, true); got != "unresolved" {
		t.Fatalf("inferred status=%q", got)
	}
}

func TestAggregateRestoreStatsUsesLatestEvent(t *testing.T) {
	now := time.Now().UTC()
	events := []storage.Event{
		{ID: "restore-old", RequestID: "req-1", Type: "RESTORE", Status: "unresolved", CreatedAt: now},
		{ID: "restore-new", RequestID: "req-1", Type: "RESTORE", Status: "restored", CreatedAt: now.Add(time.Second)},
	}
	restored, byStatus := aggregateRestoreStats(nil, events, map[string]struct{}{"req-1": {}})
	if restored != 1 || byStatus["restored"] != 1 || byStatus["unresolved"] != 0 {
		t.Fatalf("restored=%d by_status=%v", restored, byStatus)
	}
}

func TestObserveMaskedTokenCountHandlesSplitFrames(t *testing.T) {
	var tail string
	if got := observeMaskedTokenCount(&tail, []byte("prefix {{EMAIL_")); got != 0 {
		t.Fatalf("partial token counted too early: %d", got)
	}
	if got := observeMaskedTokenCount(&tail, []byte("abcdef0123456789}} suffix")); got != 1 {
		t.Fatalf("split token count=%d", got)
	}
	// Replaying a tail must not count the same complete token twice.
	if got := observeMaskedTokenCount(&tail, []byte("")); got != 0 {
		t.Fatalf("empty frame count=%d", got)
	}
	var outputTail string
	if got := observeMaskedTokenCount(&outputTail, []byte("{{SECRET_")); got != 0 {
		t.Fatalf("partial unresolved token counted too early: %d", got)
	}
	if got := observeMaskedTokenCount(&outputTail, []byte("fedcba9876543210}}")); got != 1 {
		t.Fatalf("split unresolved count=%d", got)
	}
}

func TestProviderStreamFramingSurvivesRestore(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		chunks      []string
		want        string
	}{
		{
			name:        "openai-chat-completions",
			contentType: "text/event-stream",
			chunks:      []string{`data: {"choices":[{"delta":{"content":"{{TO`, `KEN}}"}}]}` + "\n\n", "data: [DONE]\n\n"},
			want:        `data: {"choices":[{"delta":{"content":"secret-value"}}]}` + "\n\ndata: [DONE]\n\n",
		},
		{
			name:        "anthropic",
			contentType: "text/event-stream",
			chunks:      []string{"event: content_block_delta\ndata: {\"delta\":{\"text\":\"{{TO", `KEN}}"}}` + "\n\n", "event: message_stop\ndata: {}\n\n"},
			want:        "event: content_block_delta\ndata: {\"delta\":{\"text\":\"secret-value\"}}\n\nevent: message_stop\ndata: {}\n\n",
		},
		{
			name:        "responses",
			contentType: "text/event-stream",
			chunks:      []string{"event: response.output_text.delta\ndata: {\"delta\":\"{{TO", `KEN}}"}` + "\n\n", "event: response.completed\ndata: {}\n\n"},
			want:        "event: response.output_text.delta\ndata: {\"delta\":\"secret-value\"}\n\nevent: response.completed\ndata: {}\n\n",
		},
		{
			name:        "ollama",
			contentType: "application/x-ndjson",
			chunks:      []string{`{"response":"{{TO`, `KEN}}","done":false}` + "\n", `{"response":"","done":true}` + "\n"},
			want:        "{\"response\":\"secret-value\",\"done\":false}\n{\"response\":\"\",\"done\":true}\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				flusher, _ := w.(http.Flusher)
				for _, chunk := range tc.chunks {
					_, _ = io.WriteString(w, chunk)
					flusher.Flush()
				}
			}))
			defer upstream.Close()
			s, err := NewServer(Config{UpstreamURL: upstream.URL, MaxConcurrency: 1, Engine: "go-fallback"}, replaceRestoreEngine{})
			if err != nil {
				t.Fatal(err)
			}
			ts := httptest.NewServer(s.Handler())
			defer ts.Close()
			resp, err := ts.Client().Get(ts.URL + "/api/requests/stream")
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusOK || string(body) != tc.want {
				t.Fatalf("status=%d body=%q want=%q", resp.StatusCode, body, tc.want)
			}
		})
	}
}

func TestTextResponseFailsClosedWhenRestoreEngineUnavailable(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"value":"{{TOKEN}}"}`)
	}))
	defer upstream.Close()
	s, err := NewServer(Config{UpstreamURL: upstream.URL, MaxConcurrency: 1, Engine: "go-fallback"}, processOnlyEngine{})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	resp, err := ts.Client().Get(ts.URL + "/api/requests/test")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}
	if bytes.Contains(body, []byte("{{TOKEN}}")) {
		t.Fatalf("textual upstream placeholder leaked: %q", body)
	}
}

func TestModifyResponseClosesBodyOnRestoreSetupFailure(t *testing.T) {
	s, err := NewServer(Config{MaxConcurrency: 1, Engine: "go-fallback"}, processOnlyEngine{})
	if err != nil {
		t.Fatal(err)
	}
	proxy := newReverseProxy(&url.URL{Scheme: "http", Host: "upstream.invalid"}, s)
	body := &trackingReadCloser{Reader: strings.NewReader(`{"value":"{{TOKEN}}"}`)}
	resp := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}
	resp.Header.Set("Content-Type", "application/json")
	if err := proxy.ModifyResponse(resp); err == nil {
		t.Fatal("expected restore setup failure")
	}
	if !body.closed {
		t.Fatal("response body was not closed after restore setup failure")
	}
}

func TestModifyResponseHandlesMissingRequest(t *testing.T) {
	s, err := NewServer(Config{MaxConcurrency: 1, Engine: "go-fallback"}, testEngine{})
	if err != nil {
		t.Fatal(err)
	}
	proxy := newReverseProxy(&url.URL{Scheme: "http", Host: "upstream.invalid"}, s)
	resp := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"value":"plain"}`))}
	resp.Header.Set("Content-Type", "application/json")
	if err := proxy.ModifyResponse(resp); err != nil {
		t.Fatalf("missing request should use background context: %v", err)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil || string(data) != `{"value":"plain"}` {
		t.Fatalf("body=%q err=%v", data, err)
	}
}

func TestExtensionRestoreRequiresStreamID(t *testing.T) {
	s, err := NewServer(Config{MaxConcurrency: 1, Engine: "go-fallback"}, testEngine{})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/ext/restore", strings.NewReader(`{"text":"hello","final":true}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "stream_id_required") {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestExtensionRestoreDoesNotSerializeIndependentStreams(t *testing.T) {
	engine := &concurrentRestoreEngine{entered: make(chan struct{}), release: make(chan struct{})}
	s, err := NewServer(Config{MaxConcurrency: 4, Engine: "go-fallback"}, engine)
	if err != nil {
		t.Fatal(err)
	}
	firstSID, secondSID := "ext:first", "ext:second"
	firstSession, _ := engine.NewRestoreSession(context.Background())
	secondSession, _ := engine.NewRestoreSession(context.Background())
	s.extRestore[firstSID] = &extensionRestoreEntry{session: firstSession, lastUsed: time.Now()}
	s.extRestore[secondSID] = &extensionRestoreEntry{session: secondSession, lastUsed: time.Now()}
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/api/ext/restore", strings.NewReader(`{"sid":"ext:first","stream_id":"first","text":"first"}`))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		firstDone <- rr
	}()
	select {
	case <-engine.entered:
	case <-time.After(time.Second):
		t.Fatal("first restore did not enter engine")
	}
	secondDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/api/ext/restore", strings.NewReader(`{"sid":"ext:second","stream_id":"second","text":"second","final":true}`))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		secondDone <- rr
	}()
	select {
	case rr := <-secondDone:
		if rr.Code != http.StatusOK {
			t.Fatalf("second restore status=%d body=%s", rr.Code, rr.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("independent restore stream was blocked by first stream")
	}
	close(engine.release)
	select {
	case rr := <-firstDone:
		if rr.Code != http.StatusOK {
			t.Fatalf("first restore status=%d body=%s", rr.Code, rr.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("first restore did not finish")
	}
}

func TestExtensionRestoreSameStreamConcurrentFramesDoNotDeadlock(t *testing.T) {
	engine := &concurrentRestoreEngine{entered: make(chan struct{}), release: make(chan struct{})}
	s, err := NewServer(Config{MaxConcurrency: 4, Engine: "go-fallback"}, engine)
	if err != nil {
		t.Fatal(err)
	}
	session, _ := engine.NewRestoreSession(context.Background())
	s.extRestore["ext:same"] = &extensionRestoreEntry{session: session, lastUsed: time.Now()}
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/api/ext/restore", strings.NewReader(`{"sid":"ext:same","stream_id":"same","text":"first"}`))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		firstDone <- rr
	}()
	select {
	case <-engine.entered:
	case <-time.After(time.Second):
		t.Fatal("first restore did not enter engine")
	}
	secondDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/api/ext/restore", strings.NewReader(`{"sid":"ext:same","stream_id":"same","text":"second","final":true}`))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		secondDone <- rr
	}()
	// Let the first frame finish. Before the lock-order fix it attempts to
	// reacquire extMu while holding entry.mu, while the second frame holds
	// extMu and waits for entry.mu; both requests then remain stuck.
	close(engine.release)
	for i, done := range []chan *httptest.ResponseRecorder{firstDone, secondDone} {
		select {
		case rr := <-done:
			if rr.Code != http.StatusOK {
				t.Fatalf("frame %d status=%d body=%s", i+1, rr.Code, rr.Body.String())
			}
		case <-time.After(time.Second):
			t.Fatalf("same-stream frame %d deadlocked", i+1)
		}
	}
}
