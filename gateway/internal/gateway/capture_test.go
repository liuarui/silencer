package gateway

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCaptureTLSProbeUsesActualCA(t *testing.T) {
	root := t.TempDir()
	if err := ensureCaptureCA(root); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "mitmproxy-ca-cert.pem"))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(data)
	if err := captureTLSProbe(context.Background(), root, roots); err != nil {
		t.Fatal(err)
	}
	if err := captureTLSProbe(context.Background(), root, x509.NewCertPool()); err == nil {
		t.Fatal("untrusted CA passed TLS handshake")
	}
}

func TestCaptureLocalReadinessIdentifiesInstalledBundle(t *testing.T) {
	t.Setenv("SILENCER_INSTALLATION_ID", strings.Repeat("a", 64))
	s := newCaptureTestServer(t, true, testEngine{})
	r := httptest.NewRequest("GET", "/local-readyz", nil)
	r.Host = "127.0.0.1:8080"
	r.RemoteAddr = "127.0.0.1:1234"
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 || w.Body.String() != strings.Repeat("a", 64) {
		t.Fatal(w.Code, w.Body.String())
	}
	r.Header.Set("Origin", "https://untrusted.test")
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("local readiness bypassed origin validation")
	}
	remote := newCaptureTestServer(t, false, testEngine{})
	w = httptest.NewRecorder()
	remote.ServeHTTP(w, httptest.NewRequest("GET", "/local-readyz", nil))
	if w.Code != 404 {
		t.Fatal("server deployment exposed local readiness")
	}
}

func TestCaptureActivationWaitsForAppliedTargets(t *testing.T) {
	s := newCaptureTestServer(t, true, testEngine{})
	p := &captureProcess{done: make(chan struct{}), cancel: func() {}}
	s.capture.mu.Lock()
	s.capture.process = p
	s.capture.mu.Unlock()
	defer func() { close(p.done) }()
	result := make(chan error, 1)
	go func() { result <- s.capture.activateCapture(p) }()
	heartbeat := func(targets []string) {
		body, _ := json.Marshal(map[string]any{"protected": true, "targets": targets})
		r := httptest.NewRequest("POST", "/heartbeat", strings.NewReader(string(body)))
		r.Header.Set("X-Silencer-Capture-Bridge", s.capture.bridgeToken)
		s.handleCaptureBridge(httptest.NewRecorder(), r)
	}
	heartbeat([]string{"wrong.example:443"})
	select {
	case err := <-result:
		t.Fatalf("unapplied targets accepted: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	heartbeat(s.capture.snapshot().Targets)
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("applied targets did not activate")
	}
	if !s.capture.snapshot().SetupComplete {
		t.Fatal("setup completion was not persisted")
	}
}

func newCaptureTestServer(t *testing.T, local bool, engine Engine) *Server {
	t.Helper()
	t.Setenv("SILENCER_CONTROL_CONFIG_FILE", filepath.Join(t.TempDir(), "control.json"))
	cfg := DefaultConfig()
	cfg.LocalMode = local
	cfg.Engine = "go-fallback"
	cfg.AuthToken = "management-secret"
	s, err := NewServer(cfg, engine)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	return s
}
func captureControl(t *testing.T, s *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, "http://127.0.0.1:8080"+path, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:34567"
	r.Header.Set("X-Silencer-Token", "management-secret")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}
func TestCaptureCanonicalTargets(t *testing.T) {
	s := newCaptureTestServer(t, true, testEngine{})
	w := captureControl(t, s, "POST", "/api/local/capture/config", `{"targets":["API.OPENAI.COM.","api.openai.com:443","example.test:8443"]}`)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	cfg := s.capture.snapshot()
	if strings.Join(cfg.Targets, ",") != "api.openai.com:443,example.test:8443" {
		t.Fatal(cfg.Targets)
	}
	for _, target := range []string{"evil.test", "api.openai.com@evil.test", "https://api.openai.com/path", "api.openai.com:bad"} {
		if _, err := captureAuthority(target); err == nil && strings.ContainsAny(target, "@/") {
			t.Fatalf("accepted %s", target)
		}
	}
	if _, err := captureTarget("https://api.openai.com.evil.test/v1", cfg.Targets); err == nil {
		t.Fatal("suffix domain accepted")
	}
}
func TestCaptureNativeLoopbackBoundary(t *testing.T) {
	for _, target := range []string{"localhost:15721", "LOCALHOST.:443", "127.0.0.1:15721", "127.20.0.1:80", "[::1]:443", "[::ffff:127.0.0.1]:443"} {
		if err := validateCaptureNativeTargets("darwin", []string{target}); err == nil || err.Error() != "capture_loopback_unsupported" {
			t.Fatalf("macOS accepted unsupported loopback target %s: %v", target, err)
		}
		for _, platform := range []string{"linux", "windows"} {
			if err := validateCaptureNativeTargets(platform, []string{target}); err != nil {
				t.Fatalf("macOS limitation applied to %s: %v", platform, err)
			}
		}
	}
	if err := validateCaptureNativeTargets("darwin", []string{"api.example.test:443", "192.168.1.10:8080"}); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "darwin" {
		s := newCaptureTestServer(t, true, testEngine{})
		before := s.capture.snapshot().Targets
		w := captureControl(t, s, "POST", "/api/local/capture/config", `{"targets":["127.0.0.1:15721"]}`)
		if w.Code != 400 || !strings.Contains(w.Body.String(), "capture_loopback_unsupported") || !sameCaptureTargets(before, s.capture.snapshot().Targets) {
			t.Fatal("unsupported configuration was committed", w.Code, w.Body.String())
		}
	}
}

func TestCaptureTrustFailureRequiresManualInteraction(t *testing.T) {
	output := "SecTrustSettingsSetTrustSettings: The authorization was denied since no user interaction was possible. (1)"
	if err := captureTrustFailure("darwin", output); err.Error() != "manual_certificate_trust_required" {
		t.Fatal(err)
	}
	for _, platform := range []string{"darwin", "windows", "linux"} {
		if err := captureTrustFailure(platform, "User canceled. (-128)"); err.Error() != "certificate_trust_denied" {
			t.Fatal(platform, err)
		}
	}
}

func TestCaptureCertificateDownloadPublicOnly(t *testing.T) {
	s := newCaptureTestServer(t, true, testEngine{})
	path := "/api/local/setup/certificate"
	if w := captureControl(t, s, "GET", path, ""); w.Code != 404 {
		t.Fatal("GET must not generate a CA", w.Code)
	}
	if err := ensureCaptureCA(s.capture.root); err != nil {
		t.Fatal(err)
	}
	public, info, err := capturePublicCertificate(s.capture.root)
	if err != nil || len(info["sha256"]) != 64 {
		t.Fatal(info, err)
	}
	private, err := os.ReadFile(filepath.Join(s.capture.root, "mitmproxy-ca.pem"))
	if err != nil {
		t.Fatal(err)
	}
	// Even an accidentally concatenated private block must never be returned.
	if err := os.WriteFile(filepath.Join(s.capture.root, "mitmproxy-ca-cert.pem"), append(public, private...), 0600); err != nil {
		t.Fatal(err)
	}
	w := captureControl(t, s, "GET", path, "")
	if w.Code != 200 || w.Body.String() != string(public) || strings.Contains(w.Body.String(), "PRIVATE KEY") || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("public certificate export failed", w.Code)
	}
	r := httptest.NewRequest("GET", "http://127.0.0.1:8080"+path, nil)
	r.RemoteAddr = "127.0.0.1:34567"
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("certificate export bypassed local authentication", w.Code)
	}
	if w := captureControl(t, s, "POST", "/api/local/setup/actions", `{"action":"open_keychain","path":"/tmp/untrusted"}`); w.Code != 400 {
		t.Fatal("setup action accepted arbitrary parameters", w.Code)
	}
}

func TestCaptureForwardStrictCredentialsAndTarget(t *testing.T) {
	s := newCaptureTestServer(t, false, bodyMaskEngine{})
	s.cfg.CaptureForwardEnabled = true
	s.cfg.CaptureAllowedTargets = "api.example.test:443"
	for _, headers := range []http.Header{{}, {"Authorization": {"Bearer management-secret"}}, {"Cookie": {"silencer_session=management-secret"}}} {
		r := httptest.NewRequest("POST", "/api/capture/forward", strings.NewReader(`{"text":"secret"}`))
		r.Header = headers
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	w := captureControl(t, s, "POST", "/api/capture/forward", `{}`)
	if w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
	r := httptest.NewRequest("POST", "/api/capture/forward", strings.NewReader(`{}`))
	r.Header.Set("X-Silencer-Token", "management-secret")
	r.Header.Set("X-Silencer-Capture-Protocol", "1")
	r.Header.Set(captureTargetHeader, "https://evil.test/v1")
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal(w.Code, w.Body.String())
	}
	s.cfg.AuthToken = ""
	r.Header.Set("X-Silencer-Token", "")
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("empty token allowed", w.Code)
	}
}
func TestCaptureForwardPreservesTargetAndRestores(t *testing.T) {
	seen := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.RequestURI() != "/original/v1?z=2&a=1" {
			t.Errorf("target path/query changed: %s", r.URL.RequestURI())
		}
		if r.Header.Get("Authorization") != "Bearer provider-key" {
			t.Error("provider key changed")
		}
		if r.Header.Get("X-Silencer-Token") != "" || r.Header.Get(captureTargetHeader) != "" {
			t.Error("internal credentials leaked")
		}
		seen <- string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer upstream.Close()
	s := newCaptureTestServer(t, false, contextualTestEngine{})
	s.cfg.CaptureForwardEnabled = true
	u, _ := url.Parse(upstream.URL)
	s.cfg.CaptureAllowedTargets = u.Host
	r := httptest.NewRequest("POST", "/api/capture/forward", strings.NewReader(`{"text":"alice@example.com"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer provider-key")
	r.Header.Set("X-Silencer-Token", "management-secret")
	r.Header.Set("X-Silencer-Capture-Protocol", "1")
	r.Header.Set(captureTargetHeader, upstream.URL+"/original/v1?z=2&a=1")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "alice@example.com") {
		t.Fatal(w.Code, w.Body.String())
	}
	if body := <-seen; strings.Contains(body, "alice@example.com") || !strings.Contains(body, "EMAIL_") {
		t.Fatalf("unmasked body: %s", body)
	}
}
func TestCaptureScopeUsesOriginalIdentity(t *testing.T) {
	r := httptest.NewRequest("POST", "http://127.0.0.1/internal", nil)
	r.Header.Set("Authorization", "Bearer provider")
	scope := func(origin string) string {
		return requestMappingScope(r.WithContext(context.WithValue(r.Context(), captureDispatchKey{}, &captureDispatch{origin: origin})))
	}
	a := scope("https://api.one.test:443")
	r.Host = "different-bridge:18711"
	r.Header.Set("X-Silencer-Token", "rotated-management-secret")
	if a != scope("https://api.one.test:443") {
		t.Fatal("bridge or management identity changed mapping scope")
	}
	if a == scope("https://api.two.test:443") {
		t.Fatal("different providers share scope")
	}
	r.Header.Set("Authorization", "Bearer other")
	if a == scope("https://api.one.test:443") {
		t.Fatal("different credentials share scope")
	}
}
func TestCaptureHostAndOriginChecks(t *testing.T) {
	s := newCaptureTestServer(t, true, testEngine{})
	for _, tc := range []struct{ host, origin string }{{"evil.test:8080", ""}, {"127.0.0.1:8080", "https://evil.test"}} {
		r := httptest.NewRequest("GET", "/api/local/capture/config", nil)
		r.Host = tc.host
		r.RemoteAddr = "127.0.0.1:1"
		r.Header.Set("Origin", tc.origin)
		r.Header.Set("X-Silencer-Token", "management-secret")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatal(tc, w.Code)
		}
	}
}
func TestCaptureRemoteCredentialsNeverReturnedAndFailedSwitchPreservesLocal(t *testing.T) {
	s := newCaptureTestServer(t, true, testEngine{})
	remote := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Silencer-Token") != "remote-secret" {
			t.Error("missing remote auth")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"protocol":1,"remote_processing":true,"ok":true}`)
	}))
	defer remote.Close()
	s.capture.transports[""] = remote.Client().Transport.(*http.Transport)
	body, _ := json.Marshal(map[string]string{"mode": "remote", "url": remote.URL, "token": "remote-secret"})
	w := captureControl(t, s, "POST", "/api/local/processing/config", string(body))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "remote-secret") {
		t.Fatal("secret returned")
	}
	old := s.capture.snapshot().ActiveID
	if s.capture.pipelineTarget != old {
		t.Fatal("successful remote probe was discarded")
	}
	w = captureControl(t, s, "POST", "/api/local/processing/config", `{"mode":"remote","url":"http://unsafe.test","token":"secret"}`)
	if w.Code != 400 || s.capture.snapshot().ActiveID != old || s.capture.pipelineTarget != old {
		t.Fatal("failed switch committed")
	}
	data, _ := os.ReadFile(filepath.Join(s.capture.root, "settings.json"))
	if !strings.Contains(string(data), "remote-secret") {
		t.Fatal("credential not persisted")
	}
	info, _ := os.Stat(filepath.Join(s.capture.root, "settings.json"))
	if runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		t.Fatal("credential file is public")
	}
}

func TestCaptureSavingLocalProcessorPreservesVerifiedPipeline(t *testing.T) {
	s := newCaptureTestServer(t, true, contextualTestEngine{})
	s.capture.settings.Enabled = true
	for range []int{0, 1} {
		w := captureControl(t, s, "POST", "/api/local/processing/config", `{"mode":"local"}`)
		if w.Code != 200 || s.capture.pipelineTarget != "local" || !s.capture.snapshot().Enabled {
			t.Fatal("saving verified local processor degraded protection", w.Code, s.capture.pipelineTarget)
		}
	}
	// A failed commit must keep both the active processor and its verification.
	s.capture.root = filepath.Join(t.TempDir(), "missing", "parent")
	w := captureControl(t, s, "POST", "/api/local/processing/config", `{"mode":"local"}`)
	if w.Code != 500 || s.capture.pipelineTarget != "local" || s.capture.snapshot().ActiveID != "local" {
		t.Fatal("failed persistence changed processor verification", w.Code)
	}
}

func TestCaptureFailedProcessorProbeCannotClaimVerifiedPipeline(t *testing.T) {
	s := newCaptureTestServer(t, true, testEngine{})
	s.capture.settings.ActiveID = "remote-previous"
	s.capture.pipelineTarget = "remote-previous"
	w := captureControl(t, s, "POST", "/api/local/processing/config", `{"mode":"local"}`)
	if w.Code != 503 || s.capture.snapshot().ActiveID != "remote-previous" || s.capture.pipelineTarget != "remote-previous" {
		t.Fatal("failed probe replaced previous verified processor", w.Code)
	}
}
func TestCaptureRemoteControlAuthErrorDoesNotLogoutLocal(t *testing.T) {
	s := newCaptureTestServer(t, true, testEngine{})
	remote := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || strings.Contains(r.Header.Get("Cookie"), "local") {
			t.Error("local credentials forwarded")
		}
		w.Header().Set("Set-Cookie", "silencer_session=remote")
		w.WriteHeader(403)
		_, _ = io.WriteString(w, `{"error":"invalid_token"}`)
	}))
	defer remote.Close()
	s.capture.transports[""] = remote.Client().Transport.(*http.Transport)
	s.capture.settings.Profiles["remote-test"] = processingProfile{ID: "remote-test", URL: remote.URL, Token: "remote"}
	w := captureControl(t, s, "GET", "/api/processing/remote-test/config", "")
	if w.Code != 502 || !strings.Contains(w.Body.String(), "remote_auth_failed") || w.Header().Get("Set-Cookie") != "" {
		t.Fatal(w.Code, w.Body.String(), w.Header())
	}
}
func TestCaptureStopPersistFailureRetainsProcessOwnership(t *testing.T) {
	s := newCaptureTestServer(t, true, testEngine{})
	p := &captureProcess{done: make(chan struct{})}
	s.capture.settings.Enabled = true
	s.capture.process = p
	s.capture.root = filepath.Join(t.TempDir(), "missing", "parent")
	if err := s.captureAction(context.Background(), "stop"); err == nil {
		t.Fatal("expected persist failure")
	}
	if s.capture.process != p || !s.capture.settings.Enabled {
		t.Fatal("lost live process on failed stop")
	}
	s.capture.process = nil
}
func TestCaptureCAIdempotentAndPrivate(t *testing.T) {
	root := t.TempDir()
	if err := ensureCaptureCA(root); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(root, "mitmproxy-ca.pem"))
	if err := ensureCaptureCA(root); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(root, "mitmproxy-ca.pem"))
	if string(before) != string(after) {
		t.Fatal("CA changed on retry")
	}
	info, _ := os.Stat(filepath.Join(root, "mitmproxy-ca.pem"))
	if runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		t.Fatal("private key public")
	}
}
func TestCaptureSSEStreamsBeforeCompletion(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()
	s := newCaptureTestServer(t, false, testEngine{})
	s.cfg.CaptureForwardEnabled = true
	u, _ := url.Parse(upstream.URL)
	s.cfg.CaptureAllowedTargets = u.Host
	server := httptest.NewServer(s)
	defer server.Close()
	r, _ := http.NewRequest("GET", server.URL+"/api/capture/forward", nil)
	r.Header.Set("X-Silencer-Token", "management-secret")
	r.Header.Set("X-Silencer-Capture-Protocol", "1")
	r.Header.Set(captureTargetHeader, upstream.URL)
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(r)
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buffer := make([]byte, 4096)
	n, err := resp.Body.Read(buffer)
	close(release)
	if err != nil || !strings.Contains(string(buffer[:n]), "hello") {
		t.Fatalf("first SSE frame buffered: %s %v", buffer[:n], err)
	}
}

func TestCaptureNativePipelineSelfTest(t *testing.T) {
	engine, err := NewEngine(DefaultConfig())
	if err != nil {
		t.Skip("native build required")
	}
	s := newCaptureTestServer(t, false, engine)
	if err := s.checkCapturePipeline(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCaptureBridgePreservesExistingHTTPProxy(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host != "api.example.test:80" || r.Header.Get("X-Silencer-Capture-Egress") != "" {
			t.Error("original proxy target or internal headers changed")
		}
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "alice@example.com") {
			t.Error("proxy received original text")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer proxy.Close()
	s := newCaptureTestServer(t, true, contextualTestEngine{})
	s.capture.settings.Targets = []string{"api.example.test:80"}
	s.capture.settings.Enabled = true
	r, _ := http.NewRequest("POST", s.capture.bridgeURL+"/ingress", strings.NewReader(`{"text":"alice@example.com"}`))
	r.Header.Set("X-Silencer-Capture-Bridge", s.capture.bridgeToken)
	r.Header.Set(captureTargetHeader, "http://api.example.test:80/v1")
	r.Header.Set("X-Silencer-Capture-Egress", proxy.URL)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(body), "alice@example.com") {
		t.Fatalf("proxy was bypassed or restoration failed: %d %s", resp.StatusCode, body)
	}
}

func TestCaptureRemoteStreamStaysPinnedAcrossModeSwitch(t *testing.T) {
	release := make(chan struct{})
	released := false
	remote := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Header.Get("X-Silencer-Token") != "remote-only" || r.Header.Get("Authorization") != "Bearer provider-key" || r.Header.Get("X-Silencer-Capture-Bridge") != "" {
			t.Error("forwarded credential ownership changed")
		}
		if !strings.Contains(string(body), "alice@example.com") {
			t.Error("local coordinator masked the remote request")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: remote-start\n\n")
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, "data: remote-end\n\n")
	}))
	defer func() {
		if !released {
			close(release)
		}
		remote.Close()
	}()
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"processor":"local"}`)
	}))
	defer local.Close()
	s := newCaptureTestServer(t, true, contextualTestEngine{})
	u, _ := url.Parse(local.URL)
	s.capture.settings.Targets = []string{u.Host}
	s.capture.settings.Enabled = true
	s.capture.settings.Mode = "remote"
	s.capture.settings.ActiveID = "remote-test"
	s.capture.settings.Profiles["remote-test"] = processingProfile{ID: "remote-test", URL: remote.URL, Token: "remote-only"}
	s.capture.transports[""] = remote.Client().Transport.(*http.Transport)
	request := func() *http.Response {
		r, _ := http.NewRequest("POST", s.capture.bridgeURL+"/ingress", strings.NewReader(`{"text":"alice@example.com"}`))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer provider-key")
		r.Header.Set("X-Silencer-Capture-Bridge", s.capture.bridgeToken)
		r.Header.Set(captureTargetHeader, local.URL+"/original?z=2&a=1")
		resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(r)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	first := request()
	defer first.Body.Close()
	chunk := make([]byte, 256)
	n, err := first.Body.Read(chunk)
	if err != nil || !strings.Contains(string(chunk[:n]), "remote-start") {
		t.Fatal("remote first frame was buffered", err)
	}
	switched := captureControl(t, s, "POST", "/api/local/processing/config", `{"mode":"local"}`)
	if switched.Code != 200 {
		t.Fatal(switched.Code, switched.Body.String())
	}
	if s.capture.pipelineTarget != "local" {
		t.Fatal("successful hot switch discarded local pipeline verification")
	}
	second := request()
	defer second.Body.Close()
	body, _ := io.ReadAll(second.Body)
	if !strings.Contains(string(body), `"processor":"local"`) {
		t.Fatalf("new request used old processor: %s", body)
	}
	close(release)
	released = true
	body, err = io.ReadAll(first.Body)
	if err != nil || !strings.Contains(string(body), "remote-end") {
		t.Fatalf("existing stream changed processor: %s %v", body, err)
	}
}
