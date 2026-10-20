package gateway

// Capture configuration is deliberately separate from the processor control
// document: importing rules or changing processors must never move host secrets.
import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/silencer/gateway/internal/platformfs"
)

const captureProtocol = 1
const captureRuntimeVersion = "12.2.3"
const captureTargetHeader = "X-Silencer-Capture-Target"

var builtinCaptureTargets = []string{"api.openai.com:443", "api.anthropic.com:443", "generativelanguage.googleapis.com:443", "api.deepseek.com:443", "openrouter.ai:443", "api.moonshot.cn:443", "api.moonshot.ai:443", "api.siliconflow.cn:443"}

type processingProfile struct {
	ID    string `json:"id"`
	URL   string `json:"url"`
	Token string `json:"token"`
}
type captureSettings struct {
	SetupComplete bool                         `json:"setup_complete"`
	Enabled       bool                         `json:"enabled"`
	Mode          string                       `json:"mode"`
	ActiveID      string                       `json:"active_id"`
	Targets       []string                     `json:"targets"`
	RemoteProxy   string                       `json:"remote_proxy"`
	ModelProxy    string                       `json:"model_proxy"`
	AutoStart     bool                         `json:"auto_start"`
	Profiles      map[string]processingProfile `json:"profiles"`
}
type captureController struct {
	pipelineTarget   string
	actionMu         sync.Mutex
	mu               sync.Mutex
	settings         captureSettings
	root             string
	bridge           *http.Server
	bridgeURL        string
	bridgeToken      string
	transports       map[string]*http.Transport
	process          *captureProcess
	lastError        string
	observedAt       time.Time
	observedRequests uint64
	closed           bool
}
type captureDispatchKey struct{}
type captureDispatch struct {
	proxy  *httputil.ReverseProxy
	origin string
}

func (s *Server) initCapture() error {
	root := ""
	if s.configPath != "" {
		root = s.configPath + ".capture"
	}
	c := &captureController{root: root, transports: map[string]*http.Transport{}, settings: captureSettings{Mode: "local", ActiveID: "local", AutoStart: true, Targets: append([]string(nil), builtinCaptureTargets...), Profiles: map[string]processingProfile{}}}
	if s.Config().LocalMode {
		host, _, err := net.SplitHostPort(s.cfg.ListenAddr)
		if err != nil || !isLoopbackHost(host) {
			return errors.New("local mode must listen on loopback")
		}
		if root == "" {
			return errors.New("local mode requires a persistent control configuration")
		}
		if err := platformfs.MkdirPrivate(root); err != nil {
			return err
		}
		if data, err := os.ReadFile(filepath.Join(root, "settings.json")); err == nil {
			if json.Unmarshal(data, &c.settings) != nil {
				return errors.New("invalid capture settings")
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		c.settings.Targets = canonicalCaptureTargets(c.settings.Targets)
		if err := validateCaptureSettings(c.settings); err != nil {
			return err
		}
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return err
		}
		c.bridgeToken = newRequestID() + newRequestID()
		c.bridgeURL = "http://" + listener.Addr().String()
		c.bridge = &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.handleCaptureBridge(w, r) })}
		s.capture = c
		go func() { _ = c.bridge.Serve(listener) }()
	} else {
		s.capture = c
	}
	return nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
func canonicalCaptureTargets(targets []string) []string {
	result := []string{}
	seen := map[string]bool{}
	for _, value := range targets {
		canonical, err := captureAuthority(strings.TrimSpace(value))
		if err != nil {
			result = append(result, value)
			continue
		}
		if !seen[canonical] {
			seen[canonical] = true
			result = append(result, canonical)
		}
	}
	return result
}
func validLocalCaptureOrigin(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
	}
	if !isLoopbackHost(host) {
		return false
	}
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || !isLoopbackHost(peer) {
		return false
	}
	if raw := r.Header.Get("Origin"); raw != "" {
		u, err := url.Parse(raw)
		if err != nil || u.Host != r.Host || (u.Scheme != "http" && u.Scheme != "https") {
			return false
		}
	}
	return true
}
func captureAuthority(raw string) (string, error) {
	if raw == "" || strings.ContainsAny(raw, "/\\@?#\r\n\t ") {
		return "", errors.New("invalid target authority")
	}
	u, err := url.Parse("https://" + raw)
	if err != nil || u.Hostname() == "" {
		return "", errors.New("invalid target authority")
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	if _, err := net.LookupPort("tcp", port); err != nil {
		return "", errors.New("invalid target port")
	}
	return net.JoinHostPort(strings.ToLower(strings.TrimSuffix(u.Hostname(), ".")), port), nil
}
func captureTarget(raw string, targets []string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Fragment != "" || u.Opaque != "" || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" {
		return nil, errors.New("invalid capture target")
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "http" {
			port = "80"
		} else {
			port = "443"
		}
	}
	authority := net.JoinHostPort(strings.ToLower(strings.TrimSuffix(u.Hostname(), ".")), port)
	for _, allowed := range targets {
		canonical, err := captureAuthority(strings.TrimSpace(allowed))
		if err == nil && canonical == authority {
			return u, nil
		}
	}
	return nil, errors.New("capture target not allowed")
}
func validateCaptureSettings(cfg captureSettings) error {
	if cfg.Mode != "local" && cfg.Mode != "remote" {
		return errors.New("invalid processing mode")
	}
	if len(cfg.Targets) > 256 {
		return errors.New("too many capture targets")
	}
	for _, target := range cfg.Targets {
		if _, err := captureAuthority(target); err != nil {
			return err
		}
	}
	for _, raw := range []string{cfg.RemoteProxy, cfg.ModelProxy} {
		if err := validateCaptureProxy(raw); err != nil {
			return err
		}
	}
	if cfg.Profiles == nil {
		return errors.New("missing processing profiles")
	}
	if cfg.Mode == "remote" {
		if _, ok := cfg.Profiles[cfg.ActiveID]; !ok {
			return errors.New("unknown processing target")
		}
	}
	return nil
}

// The pinned macOS redirector excludes loopback at the NetworkExtension rule
// layer. Accepting these targets would imply protection the addon cannot apply.
func validateCaptureNativeTargets(platform string, targets []string) error {
	if platform != "darwin" {
		return nil
	}
	for _, target := range targets {
		authority, err := captureAuthority(target)
		if err != nil {
			return err
		}
		host, _, _ := net.SplitHostPort(authority)
		if isLoopbackHost(host) {
			return errors.New("capture_loopback_unsupported")
		}
	}
	return nil
}
func validateCaptureProxy(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h") || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("invalid egress proxy")
	}
	return nil
}
func (c *captureController) saveLocked() error {
	if c.root == "" {
		return errors.New("capture storage unavailable")
	}
	data, err := json.Marshal(c.settings)
	if err != nil {
		return err
	}
	return writeConfigBytes(filepath.Join(c.root, "settings.json"), data)
}
func (c *captureController) snapshot() captureSettings {
	c.mu.Lock()
	defer c.mu.Unlock()
	copy := c.settings
	copy.Targets = append([]string(nil), copy.Targets...)
	copy.Profiles = map[string]processingProfile{}
	for k, v := range c.settings.Profiles {
		copy.Profiles[k] = v
	}
	return copy
}
func (c *captureController) publicSettings() map[string]any {
	cfg := c.snapshot()
	profiles := []map[string]any{}
	for _, p := range cfg.Profiles {
		profiles = append(profiles, map[string]any{"id": p.ID, "url": p.URL, "credential_configured": p.Token != ""})
	}
	return map[string]any{"enabled": cfg.Enabled, "mode": cfg.Mode, "active_id": cfg.ActiveID, "targets": cfg.Targets, "remote_proxy": redactProxyURL(cfg.RemoteProxy), "model_proxy": redactProxyURL(cfg.ModelProxy), "auto_start": cfg.AutoStart, "setup_complete": cfg.SetupComplete, "profiles": profiles}
}
func redactProxyURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if u.User != nil {
		u.User = nil
	}
	return u.String()
}
func (c *captureController) transport(cfg Config, proxyURL string) *http.Transport {
	c.mu.Lock()
	defer c.mu.Unlock()
	if tr := c.transports[proxyURL]; tr != nil {
		return tr
	}
	tr := newProxyTransport(cfg).(*http.Transport)
	tr.Proxy = nil
	if proxyURL != "" {
		u, _ := url.Parse(proxyURL)
		tr.Proxy = http.ProxyURL(u)
	}
	c.transports[proxyURL] = tr
	return tr
}
func (c *captureController) close() {
	c.actionMu.Lock()
	defer c.actionMu.Unlock()
	c.mu.Lock()
	c.closed = true
	bridge := c.bridge
	process := c.process
	for _, tr := range c.transports {
		tr.CloseIdleConnections()
	}
	c.mu.Unlock()
	if process != nil {
		process.stop()
	}
	if bridge != nil {
		_ = bridge.Close()
	}
}
func (s *Server) captureError(w http.ResponseWriter, status int, code string) {
	s.writeJSON(w, status, map[string]any{"ok": false, "error": code})
}

// Local host operations have stricter authentication than the legacy console.
func (s *Server) captureLocalAuthorized(r *http.Request) bool {
	if !s.Config().LocalMode || s.Config().AuthToken == "" || !s.managementAuthorized(r) {
		return false
	}
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
	}
	if !isLoopbackHost(host) {
		return false
	}
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || !isLoopbackHost(peer) {
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || u.Host != r.Host || (u.Scheme != "http" && u.Scheme != "https") {
			return false
		}
	}
	return true
}
func captureReleaseBase() string {
	if value := strings.TrimSpace(os.Getenv("SILENCER_CAPTURE_RELEASE_BASE")); value != "" {
		return value
	}
	return "https://github.com/liuarui/silencer/releases/latest/download"
}

func (s *Server) handleCaptureRoute(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path == "/local-readyz" {
		identity := os.Getenv("SILENCER_INSTALLATION_ID")
		if !s.Config().LocalMode || len(identity) != 64 || r.Method != http.MethodGet {
			s.captureError(w, 404, "endpoint_not_found")
			return true
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(w, identity)
		return true
	}
	if strings.HasPrefix(r.URL.Path, "/capture-assets/") {
		s.serveCaptureAsset(w, r)
		return true
	}
	if r.URL.Path == "/api/capture/forward" || r.URL.Path == "/api/capture/probe" {
		s.handleCaptureForward(w, r)
		return true
	}
	if r.URL.Path == "/api/capabilities" {
		if !s.authorized(r) {
			s.captureError(w, 401, "unauthorized")
			return true
		}
		s.writeJSON(w, 200, map[string]any{"protocol": captureProtocol, "local": s.Config().LocalMode, "remote_processing": s.Config().CaptureForwardEnabled && s.Config().AuthToken != "", "runtime_version": captureRuntimeVersion, "platform": runtime.GOOS, "arch": runtime.GOARCH, "release_base": captureReleaseBase()})
		return true
	}
	if !strings.HasPrefix(r.URL.Path, "/api/local/") && !strings.HasPrefix(r.URL.Path, "/api/processing/") {
		return false
	}
	if !s.Config().LocalMode {
		s.captureError(w, 409, "local_component_required")
		return true
	}
	if !s.captureLocalAuthorized(r) {
		s.captureError(w, 403, "local_authorization_required")
		return true
	}
	if strings.HasPrefix(r.URL.Path, "/api/processing/") {
		s.handleProcessingControl(w, r)
		return true
	}
	s.handleLocalCaptureControl(w, r)
	return true
}

func (s *Server) handleCaptureForward(w http.ResponseWriter, r *http.Request) {
	if !s.Config().CaptureForwardEnabled {
		s.captureError(w, 404, "capture_forward_disabled")
		return
	}
	token := r.Header.Get("X-Silencer-Token")
	if s.Config().AuthToken == "" || len(token) != len(s.Config().AuthToken) || subtle.ConstantTimeCompare([]byte(token), []byte(s.Config().AuthToken)) != 1 {
		s.captureError(w, 401, "remote_auth_failed")
		return
	}
	if r.Header.Get("X-Silencer-Capture-Protocol") != "1" {
		s.captureError(w, 409, "capture_protocol_mismatch")
		return
	}
	if r.URL.Path == "/api/capture/probe" {
		if r.Method != http.MethodPost {
			s.captureError(w, 405, "method_not_allowed")
			return
		}
		if err := s.checkCapturePipeline(r.Context()); err != nil {
			s.captureError(w, 503, err.Error())
			return
		}
		s.writeJSON(w, 200, map[string]any{"ok": true, "protocol": captureProtocol})
		return
	}
	target, err := captureTarget(r.Header.Get(captureTargetHeader), strings.Split(s.Config().CaptureAllowedTargets, ","))
	if err != nil {
		s.captureError(w, 403, "capture_target_not_allowed")
		return
	}
	s.configMu.RLock()
	egressSettings, _ := s.control["egress_proxy"].(map[string]any)
	egress := ""
	if egressSettings["enabled"] == true {
		egress, _ = egressSettings["url"].(string)
	}
	s.configMu.RUnlock()
	if validateCaptureProxy(egress) != nil {
		s.captureError(w, 503, "processor_egress_invalid")
		return
	}
	s.serveCapturedRequest(w, r, target, egress)
}
func (s *Server) serveCapturedRequest(w http.ResponseWriter, r *http.Request, target *url.URL, egress string) {
	if r.Method == http.MethodConnect || r.Header.Get("Upgrade") != "" {
		s.captureError(w, 400, "unsupported_capture_protocol")
		return
	}
	limiter, ok := s.acquire(w)
	if !ok {
		return
	}
	defer s.release(limiter)
	proxy := newReverseProxy(target, s)
	proxy.Transport = s.capture.transport(s.Config(), egress)
	proxy.FlushInterval = -1
	proxy.Director = func(req *http.Request) {
		stripGatewayCredentials(req)
		stripCaptureHeaders(req)
		u := *target
		req.URL = &u
		req.Host = u.Host
		req.Header.Del("Forwarded")
		req.Header["X-Forwarded-For"] = nil
		req.Header.Del("X-Forwarded-Host")
	}
	port := target.Port()
	if port == "" {
		if target.Scheme == "http" {
			port = "80"
		} else {
			port = "443"
		}
	}
	origin := target.Scheme + "://" + net.JoinHostPort(strings.ToLower(strings.TrimSuffix(target.Hostname(), ".")), port)
	u := *target
	r.URL = &u
	r.Host = u.Host
	r = r.WithContext(context.WithValue(r.Context(), captureDispatchKey{}, &captureDispatch{proxy: proxy, origin: origin}))
	s.handleProxy(w, r)
}
func stripCaptureHeaders(r *http.Request) {
	for name := range r.Header {
		if strings.HasPrefix(strings.ToLower(name), "x-silencer-capture-") {
			r.Header.Del(name)
		}
	}
}
func requestMappingScope(r *http.Request) string {
	if dispatch, ok := r.Context().Value(captureDispatchKey{}).(*captureDispatch); ok {
		h := sha256.New()
		for _, v := range []string{"capture-v1", dispatch.origin, r.Header.Get("Authorization"), r.Header.Get("X-Api-Key"), r.Header.Get("Api-Key")} {
			h.Write([]byte(v))
			h.Write([]byte{0})
		}
		return hex.EncodeToString(h.Sum(nil))
	}
	return proxyMappingScope(r)
}

func (s *Server) handleCaptureBridge(w http.ResponseWriter, r *http.Request) {
	c := s.capture
	if c == nil || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Silencer-Capture-Bridge")), []byte(c.bridgeToken)) != 1 {
		s.captureError(w, 403, "bridge_auth_failed")
		return
	}
	if r.URL.Path == "/heartbeat" {
		if r.Method != http.MethodPost {
			s.captureError(w, 405, "method_not_allowed")
			return
		}
		var heartbeat struct {
			Protected bool     `json:"protected"`
			Targets   []string `json:"targets"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<10)).Decode(&heartbeat) != nil {
			s.captureError(w, 400, "invalid_capture_heartbeat")
			return
		}
		c.mu.Lock()
		if c.process != nil {
			c.process.heartbeat = time.Now()
			c.process.protected = heartbeat.Protected && sameCaptureTargets(heartbeat.Targets, c.settings.Targets)
		}
		targets := []string{}
		if c.settings.Enabled {
			targets = append(targets, c.settings.Targets...)
		}
		proxies := []string{c.settings.ModelProxy, c.settings.RemoteProxy}
		c.mu.Unlock()
		s.writeJSON(w, 200, map[string]any{"ok": true, "targets": targets, "proxies": proxies})
		return
	}
	if r.URL.Path != "/ingress" {
		s.captureError(w, 404, "endpoint_not_found")
		return
	}
	s.lifecycleMu.Lock()
	if s.shuttingDown {
		s.lifecycleMu.Unlock()
		s.captureError(w, 503, "gateway_shutting_down")
		return
	}
	s.requests.begin()
	s.lifecycleMu.Unlock()
	defer s.requests.end()
	cfg := c.snapshot()
	if !cfg.Enabled {
		s.captureError(w, 503, "capture_stopped")
		return
	}
	target, err := captureTarget(r.Header.Get(captureTargetHeader), cfg.Targets)
	if err != nil {
		s.captureError(w, 403, "capture_target_not_allowed")
		return
	}
	if cfg.Mode == "local" {
		egress := cfg.ModelProxy
		if egress == "" {
			egress = r.Header.Get("X-Silencer-Capture-Egress")
		}
		if validateCaptureProxy(egress) != nil {
			s.captureError(w, 400, "invalid_capture_egress")
			return
		}
		s.serveCapturedRequest(w, r, target, egress)
	} else {
		profile, ok := cfg.Profiles[cfg.ActiveID]
		if !ok {
			s.captureError(w, 503, "processing_target_unavailable")
			return
		}
		u, _ := url.Parse(profile.URL + "/api/capture/forward")
		proxy := &httputil.ReverseProxy{Transport: c.transport(s.Config(), cfg.RemoteProxy), FlushInterval: -1}
		proxy.Director = func(req *http.Request) {
			stripGatewayCredentials(req)
			stripCaptureHeaders(req)
			copy := *u
			req.URL = &copy
			req.Host = u.Host
			req.Header.Set("X-Silencer-Token", profile.Token)
			req.Header.Set(captureTargetHeader, target.String())
			req.Header.Set("X-Silencer-Capture-Protocol", "1")
		}
		proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
			s.captureError(w, 502, "remote_processor_unavailable")
		}
		proxy.ServeHTTP(w, r)
	}
	c.mu.Lock()
	c.observedRequests++
	c.observedAt = time.Now()
	c.mu.Unlock()
}

func sameCaptureTargets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[string]bool, len(a))
	for _, target := range a {
		set[target] = true
	}
	for _, target := range b {
		if !set[target] {
			return false
		}
	}
	return true
}

func (s *Server) handleLocalCaptureControl(w http.ResponseWriter, r *http.Request) {
	c := s.capture
	switch r.URL.Path {
	case "/api/local/setup/uninstall-script":
		s.serveCaptureUninstallScript(w, r)
	case "/api/local/setup/certificate":
		if r.Method != http.MethodGet {
			s.captureError(w, 405, "method_not_allowed")
			return
		}
		data, _, err := capturePublicCertificate(c.root)
		if err != nil {
			s.captureError(w, 404, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/x-x509-ca-cert")
		w.Header().Set("Content-Disposition", `attachment; filename="silencer-local-ca.crt"`)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write(data)
	case "/api/local/capture/config":
		if r.Method == http.MethodGet {
			s.writeJSON(w, 200, c.publicSettings())
			return
		}
		if r.Method != http.MethodPost {
			s.captureError(w, 405, "method_not_allowed")
			return
		}
		c.actionMu.Lock()
		defer c.actionMu.Unlock()
		if captureMaintenanceInProgress(c.root) {
			s.captureError(w, 409, "maintenance_in_progress")
			return
		}
		var patch struct {
			Targets     *[]string `json:"targets"`
			RemoteProxy *string   `json:"remote_proxy"`
			ModelProxy  *string   `json:"model_proxy"`
		}
		if err := decodeCaptureJSON(w, r, &patch); err != nil {
			s.captureError(w, 400, "invalid_capture_config")
			return
		}
		c.mu.Lock()
		before := c.settings
		next := before
		if patch.Targets != nil {
			next.Targets = *patch.Targets
		}
		if patch.RemoteProxy != nil && *patch.RemoteProxy != redactProxyURL(before.RemoteProxy) {
			next.RemoteProxy = *patch.RemoteProxy
		}
		if patch.ModelProxy != nil && *patch.ModelProxy != redactProxyURL(before.ModelProxy) {
			next.ModelProxy = *patch.ModelProxy
		}
		next.Targets = canonicalCaptureTargets(next.Targets)
		if err := validateCaptureSettings(next); err != nil {
			c.mu.Unlock()
			s.captureError(w, 400, "invalid_capture_config")
			return
		}
		if err := validateCaptureNativeTargets(runtime.GOOS, next.Targets); err != nil {
			c.mu.Unlock()
			s.captureError(w, 400, err.Error())
			return
		}
		if c.settings.Enabled {
			c.mu.Unlock()
			s.captureError(w, 409, "stop_capture_before_editing_targets")
			return
		}
		c.settings = next
		err := c.saveLocked()
		if err != nil {
			c.settings = before
		}
		c.mu.Unlock()
		if err != nil {
			s.captureError(w, 500, "capture_persist_failed")
			return
		}
		s.writeJSON(w, 200, c.publicSettings())
	case "/api/local/capture/status", "/api/local/setup/status":
		if r.Method != http.MethodGet {
			s.captureError(w, 405, "method_not_allowed")
			return
		}
		s.writeJSON(w, 200, s.captureStatus(r.Context()))
	case "/api/local/processing/config":
		if r.Method == http.MethodGet {
			s.writeJSON(w, 200, c.publicSettings())
			return
		}
		if r.Method != http.MethodPost {
			s.captureError(w, 405, "method_not_allowed")
			return
		}
		s.configureProcessing(w, r)
	case "/api/local/setup/actions", "/api/local/capture/start", "/api/local/capture/stop":
		if r.Method != http.MethodPost {
			s.captureError(w, 405, "method_not_allowed")
			return
		}
		var action struct {
			Action string `json:"action"`
		}
		if strings.HasSuffix(r.URL.Path, "/start") {
			action.Action = "start"
		} else if strings.HasSuffix(r.URL.Path, "/stop") {
			action.Action = "stop"
		} else if decodeCaptureJSON(w, r, &action) != nil {
			s.captureError(w, 400, "invalid_setup_action")
			return
		}
		if err := s.captureAction(r.Context(), action.Action); err != nil {
			s.captureError(w, 409, err.Error())
			return
		}
		s.writeJSON(w, 200, s.captureStatus(r.Context()))
	default:
		s.captureError(w, 404, "endpoint_not_found")
	}
}
func decodeCaptureJSON(w http.ResponseWriter, r *http.Request, out any) error {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("trailing data")
	}
	return nil
}

func (s *Server) configureProcessing(w http.ResponseWriter, r *http.Request) {
	s.capture.actionMu.Lock()
	defer s.capture.actionMu.Unlock()
	if captureMaintenanceInProgress(s.capture.root) {
		s.captureError(w, 409, "maintenance_in_progress")
		return
	}
	var input struct {
		Mode  string `json:"mode"`
		URL   string `json:"url"`
		Token string `json:"token"`
	}
	if decodeCaptureJSON(w, r, &input) != nil {
		s.captureError(w, 400, "invalid_processing_config")
		return
	}
	c := s.capture
	before := c.snapshot()
	next := before
	if input.Mode == "local" {
		if err := s.checkCapturePipeline(r.Context()); err != nil {
			s.captureError(w, 503, err.Error())
			return
		}
		next.Mode = "local"
		next.ActiveID = "local"
	} else if input.Mode == "remote" {
		u, err := url.Parse(strings.TrimRight(input.URL, "/"))
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || input.Token == "" {
			s.captureError(w, 400, "remote_requires_https_and_token")
			return
		}
		profile := processingProfile{URL: strings.TrimRight(u.String(), "/"), Token: input.Token}
		digest := sha256.Sum256([]byte(profile.URL + "\x00" + profile.Token))
		profile.ID = "remote-" + hex.EncodeToString(digest[:8])
		if err := s.probeRemoteCapture(r.Context(), profile, before.RemoteProxy); err != nil {
			s.captureError(w, 502, err.Error())
			return
		}
		next.Profiles[profile.ID] = profile
		next.ActiveID = profile.ID
		next.Mode = "remote"
	} else {
		s.captureError(w, 400, "invalid_processing_mode")
		return
	}
	c.mu.Lock()
	if c.settings.ActiveID != before.ActiveID || c.settings.RemoteProxy != before.RemoteProxy {
		c.mu.Unlock()
		s.captureError(w, 409, "processing_config_changed")
		return
	}
	next.Targets = c.settings.Targets
	next.ModelProxy = c.settings.ModelProxy
	next.Enabled = c.settings.Enabled
	c.settings = next
	err := c.saveLocked()
	if err != nil {
		c.settings = before
	} else {
		// Both branches verified this exact processor before committing. Keep the
		// result, including when saving the already-active processor again.
		c.pipelineTarget = next.ActiveID
	}
	c.mu.Unlock()
	if err != nil {
		s.captureError(w, 500, "capture_persist_failed")
		return
	}
	s.writeJSON(w, 200, c.publicSettings())
}
func (s *Server) checkRemoteProcessor(ctx context.Context, p processingProfile, egress string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", p.URL+"/api/capabilities", nil)
	if err != nil {
		return errors.New("remote_url_invalid")
	}
	req.Header.Set("X-Silencer-Token", p.Token)
	client := &http.Client{Transport: s.capture.transport(s.Config(), egress), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("remote_connection_failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return errors.New("remote_auth_failed")
	}
	if resp.StatusCode != 200 {
		return errors.New("remote_capability_unavailable")
	}
	var cap struct {
		Protocol int  `json:"protocol"`
		Enabled  bool `json:"remote_processing"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&cap) != nil || cap.Protocol != captureProtocol || !cap.Enabled {
		return errors.New("remote_protocol_unsupported")
	}
	return nil
}

func processorControlAllowed(path string) bool {
	for _, prefix := range []string{"/api/logs", "/api/requests", "/api/stats", "/api/audit", "/api/config", "/api/test/", "/api/demo", "/api/upstream/test"} {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return path == "/api/status" || path == "/api/health"
}

var processingConfigFields = map[string]bool{"sensitive": true, "sensitive_disabled": true, "sensitive_word_disabled": true, "sensitive_word_whole": true, "builtin_rules": true, "secret_prefixes": true, "audit": true, "log_noise_allowlist": true, "record_detail_data": true, "record_plaintext_words": true, "log_retention_days": true, "upstreams": true, "egress_proxy": true, "filter_enabled": true, "response_scan": true, "stream_response": true}

func (s *Server) handleProcessingControl(w http.ResponseWriter, r *http.Request) {
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/api/processing/"), "/", 2)
	if len(parts) != 2 {
		s.captureError(w, 404, "processing_target_not_found")
		return
	}
	id, path := parts[0], "/api/"+parts[1]
	if !processorControlAllowed(path) || (strings.HasPrefix(path, "/api/requests/") && !isRequestIDPath(path)) {
		s.captureError(w, 403, "processing_operation_not_allowed")
		return
	}
	if id == "local" {
		copy := r.Clone(r.Context())
		copy.URL.Path = path
		copy.URL.RawPath = ""
		s.ServeHTTP(w, copy)
		return
	}
	if r.Method != http.MethodGet && strings.HasPrefix(path, "/api/config") {
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			s.captureError(w, 400, "invalid_config")
			return
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(data, &fields) != nil {
			s.captureError(w, 400, "invalid_config")
			return
		}
		if path == "/api/config" {
			for key := range fields {
				if !processingConfigFields[key] {
					s.captureError(w, 403, "local_setting_not_allowed")
					return
				}
			}
		} else if path == "/api/config/patch" {
			var key string
			_ = json.Unmarshal(fields["key"], &key)
			if !processingConfigFields[key] {
				s.captureError(w, 403, "local_setting_not_allowed")
				return
			}
		} else if path != "/api/config/builtin_rules" {
			s.captureError(w, 403, "processing_operation_not_allowed")
			return
		}
		r.Body = io.NopCloser(strings.NewReader(string(data)))
		r.ContentLength = int64(len(data))
	}

	cfg := s.capture.snapshot()
	profile, ok := cfg.Profiles[id]
	if !ok {
		s.captureError(w, 404, "processing_target_not_found")
		return
	}
	u, _ := url.Parse(profile.URL)
	proxy := &httputil.ReverseProxy{Transport: s.capture.transport(s.Config(), cfg.RemoteProxy), FlushInterval: -1, Director: func(req *http.Request) {
		stripGatewayCredentials(req)
		req.Header.Del("Authorization")
		copy := *u
		copy.Path = path
		copy.RawQuery = req.URL.RawQuery
		req.URL = &copy
		req.Host = u.Host
		req.Header.Set("X-Silencer-Token", profile.Token)
		req.Header.Del("Cookie")
	}, ModifyResponse: func(resp *http.Response) error {
		resp.Header.Del("Set-Cookie")
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			resp.Body.Close()
			resp.StatusCode = 502
			resp.Body = io.NopCloser(strings.NewReader(`{"error":"remote_auth_failed"}`))
			resp.ContentLength = -1
			resp.Header.Del("Content-Length")
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 && strings.HasPrefix(path, "/api/config") {
			data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20+1))
			resp.Body.Close()
			if err != nil || len(data) > 4<<20 {
				return errors.New("processor configuration too large")
			}
			var document map[string]any
			if json.Unmarshal(data, &document) != nil {
				return errors.New("invalid processor configuration")
			}
			s.configMu.RLock()
			merged := cloneMap(s.control)
			s.configMu.RUnlock()
			if fields, ok := document["config"].(map[string]any); ok {
				for key, value := range fields {
					if processingConfigFields[key] {
						merged[key] = value
					}
				}
				document["config"] = merged
			} else if path == "/api/config" && r.Method == http.MethodGet {
				for key, value := range document {
					if processingConfigFields[key] {
						merged[key] = value
					}
				}
				document = merged
			}
			data, _ = json.Marshal(document)
			resp.Body = io.NopCloser(bytes.NewReader(data))
			resp.ContentLength = int64(len(data))
			resp.Header.Del("Content-Encoding")
			resp.Header.Del("Content-Length")
		}
		return nil
	}, ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
		s.captureError(w, 502, "remote_processor_unavailable")
	}}
	proxy.ServeHTTP(w, r)
}
