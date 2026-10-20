package gateway

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"embed"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

//go:embed capture_assets/addon.py capture_assets/install.sh capture_assets/install.ps1 capture_assets/uninstall.sh capture_assets/uninstall.ps1 capture_assets/linux-sudo.sh capture_assets/linux-askpass.sh
var captureAssets embed.FS

type captureProcess struct {
	cmd       *exec.Cmd
	done      chan struct{}
	cancel    context.CancelFunc
	cleanup   func() error
	heartbeat time.Time
	protected bool
}

func (p *captureProcess) confirmStopped() error {
	if p.cleanup != nil {
		return p.cleanup()
	}
	return confirmCaptureChildrenStopped(p.cmd)
}

func (p *captureProcess) stop() error {
	select {
	case <-p.done:
		if p.cancel != nil {
			p.cancel()
		}
		return p.confirmStopped()
	default:
	}
	if p.cmd != nil && p.cmd.Process != nil && signalCaptureProcess(p.cmd, os.Interrupt) == nil {
		select {
		case <-p.done:
			if p.cancel != nil {
				p.cancel()
			}
			return p.confirmStopped()
		case <-time.After(3 * time.Second):
		}
	}
	if p.cancel != nil {
		p.cancel()
	}
	if p.cmd == nil || p.cmd.Process == nil {
		select {
		case <-p.done:
			return nil
		default:
			return errors.New("capture_stop_failed")
		}
	}
	select {
	case <-p.done:
		return p.confirmStopped()
	case <-time.After(5 * time.Second):
		if err := p.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return errors.New("capture_stop_failed")
		}
	}
	select {
	case <-p.done:
		return p.confirmStopped()
	case <-time.After(3 * time.Second):
		return errors.New("capture_stop_failed")
	}
}
func captureExecutable() (string, error) {
	if path := os.Getenv("SILENCER_MITMDUMP_PATH"); path != "" {
		if !filepath.IsAbs(path) {
			return "", errors.New("capture_runtime_path_invalid")
		}
		return path, nil
	}
	return exec.LookPath("mitmdump")
}
func capturePlatformCheck() (bool, string) {
	if runtime.GOOS == "darwin" {
		data, err := exec.Command("/usr/bin/sw_vers", "-productVersion").Output()
		major, _ := strconv.Atoi(strings.Split(strings.TrimSpace(string(data)), ".")[0])
		if err != nil || major < 14 {
			return false, "macos_14_required"
		}
		return true, ""
	}
	if runtime.GOARCH != "amd64" {
		return false, "unsupported_architecture"
	}
	if runtime.GOOS == "windows" {
		return true, ""
	}
	if runtime.GOOS != "linux" {
		return false, "unsupported_platform"
	}
	for _, path := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(path); err == nil {
			return false, "container_capture_unsupported"
		}
	}
	data, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return false, "kernel_check_failed"
	}
	kernel := strings.ToLower(string(data))
	if strings.Contains(kernel, "microsoft") {
		return false, "wsl_capture_unsupported"
	}
	parts := strings.Split(kernel, ".")
	if len(parts) < 2 {
		return false, "kernel_check_failed"
	}
	major, _ := strconv.Atoi(parts[0])
	minor, _ := strconv.Atoi(parts[1])
	if major < 6 || (major == 6 && minor < 8) {
		return false, "linux_kernel_6_8_required"
	}
	return true, ""
}
func captureRuntimeCheck(ctx context.Context) (bool, string) {
	path, err := captureExecutable()
	if err != nil {
		return false, "capture_runtime_missing"
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, path, "--version")
	command.Env = captureRuntimeEnvironment()
	data, err := command.Output()
	if err != nil {
		return false, "capture_runtime_unavailable"
	}
	if !strings.Contains(string(data), "Mitmproxy: "+captureRuntimeVersion) {
		return false, "capture_runtime_version_mismatch"
	}
	return true, ""
}
func captureProcessReadiness(process *captureProcess, now time.Time) (alive, ready, applied bool) {
	if process == nil {
		return
	}
	select {
	case <-process.done:
		return
	default:
	}
	alive = true
	age := now.Sub(process.heartbeat)
	ready = !process.heartbeat.IsZero() && age >= 0 && age < 10*time.Second
	applied = ready && process.protected
	return
}

func captureLifecycleState(supported, installed, trusted, pipeline, enabled, ready, applied bool) string {
	if !supported {
		return "unsupported"
	}
	if enabled {
		if installed && trusted && pipeline && ready && applied {
			return "running"
		}
		return "degraded"
	}
	if installed && trusted && ready {
		return "ready"
	}
	return "needs_setup"
}

func (s *Server) captureStatus(ctx context.Context) map[string]any {
	c := s.capture
	cfg := c.snapshot()
	supported, platformError := capturePlatformCheck()
	installed, componentError := captureRuntimeCheck(ctx)
	trusted := captureCertificateTrusted(c.root)
	c.mu.Lock()
	alive, permissions, applied := captureProcessReadiness(c.process, time.Now())
	lastError, observedAt, observedRequests := c.lastError, c.observedAt, c.observedRequests
	pipelineOK := c.pipelineTarget == cfg.ActiveID
	c.mu.Unlock()
	state := captureLifecycleState(supported, installed, trusted, pipelineOK, cfg.Enabled, permissions, applied)
	_, certificate, _ := capturePublicCertificate(c.root)
	return map[string]any{"state": state, "platform": runtime.GOOS, "arch": runtime.GOARCH, "certificate": certificate, "config": c.publicSettings(), "checks": map[string]any{"pipeline": map[string]any{"ok": pipelineOK, "error": checkError(pipelineOK, "pipeline_check_required")}, "component": map[string]any{"ok": installed, "error": componentError}, "platform": map[string]any{"ok": supported, "error": platformError}, "permissions": map[string]any{"ok": permissions, "error": checkError(permissions, "capture_authorization_required")}, "certificate": map[string]any{"ok": trusted, "error": checkError(trusted, "certificate_trust_required")}}, "last_error": lastError, "observed_requests": observedRequests, "last_observed_at": observedAt, "runtime_version": captureRuntimeVersion, "job": readCaptureJob(c.root), "uninstall": s.captureUninstallStatus(), "runtime": map[string]bool{"process_running": alive, "heartbeat_ready": permissions, "targets_applied": applied}}
}
func checkError(ok bool, code string) string {
	if ok {
		return ""
	}
	return code
}
func (s *Server) captureAction(ctx context.Context, action string) error {
	c := s.capture
	c.actionMu.Lock()
	defer c.actionMu.Unlock()
	switch action {
	case "start", "authorize", "certificate", "trust_certificate", "diagnose", "detect_proxy":
		if captureMaintenanceInProgress(c.root) {
			return errors.New("maintenance_in_progress")
		}
	}
	switch action {
	case "check":
		return nil
	case "open_permission_settings", "open_keychain":
		if runtime.GOOS != "darwin" {
			return errors.New("capture_settings_open_unsupported")
		}
		bundle := "com.apple.systempreferences"
		if action == "open_keychain" {
			bundle = "com.apple.keychainaccess"
		}
		if err := exec.CommandContext(ctx, "/usr/bin/open", "-b", bundle).Run(); err != nil {
			return errors.New("capture_settings_open_failed")
		}
		return nil
	case "diagnose":
		cfg := c.snapshot()
		if err := captureTLSProbe(ctx, c.root, captureSystemCertPool()); err != nil {
			return err
		}
		var err error
		if cfg.Mode == "remote" {
			err = s.probeRemoteCapture(ctx, cfg.Profiles[cfg.ActiveID], cfg.RemoteProxy)
		} else {
			err = s.checkCapturePipeline(ctx)
		}
		if err != nil {
			return err
		}
		c.mu.Lock()
		c.pipelineTarget = cfg.ActiveID
		c.mu.Unlock()
		return nil
	case "detect_proxy":
		proxy, code := detectCaptureSystemProxy(ctx)
		if code != "" {
			return errors.New(code)
		}
		c.mu.Lock()
		before := c.settings
		if proxy != "" {
			if c.settings.RemoteProxy == "" {
				c.settings.RemoteProxy = proxy
			}
			if c.settings.ModelProxy == "" {
				c.settings.ModelProxy = proxy
			}
		}
		err := c.saveLocked()
		if err != nil {
			c.settings = before
		}
		c.mu.Unlock()
		if err != nil {
			return errors.New("capture_persist_failed")
		}
		return nil
	case "repair", "update", "uninstall":
		return s.queueCaptureMaintenance(action)
	case "certificate":
		return ensureCaptureCA(c.root)
	case "trust_certificate":
		if err := ensureCaptureCA(c.root); err != nil {
			return err
		}
		return trustCaptureCA(ctx, c.root)
	case "authorize", "start":
		if action == "start" {
			if err := validateCaptureNativeTargets(runtime.GOOS, c.snapshot().Targets); err != nil {
				return err
			}
		}
		if ok, code := capturePlatformCheck(); !ok {
			return errors.New(code)
		}
		if ok, code := captureRuntimeCheck(ctx); !ok {
			return errors.New(code)
		}
		if err := ensureCaptureCA(c.root); err != nil {
			return err
		}
		if action == "start" && !captureCertificateTrusted(c.root) {
			return errors.New("certificate_trust_required")
		}
		if action == "start" {
			if err := captureTLSProbe(ctx, c.root, captureSystemCertPool()); err != nil {
				return err
			}
		}
		cfg := c.snapshot()
		if action == "start" {
			if len(cfg.Targets) == 0 {
				return errors.New("capture_targets_required")
			}
			var err error
			if cfg.Mode == "remote" {
				err = s.probeRemoteCapture(ctx, cfg.Profiles[cfg.ActiveID], cfg.RemoteProxy)
			} else {
				err = s.checkCapturePipeline(ctx)
			}
			if err != nil {
				return err
			}
			c.mu.Lock()
			c.pipelineTarget = cfg.ActiveID
			c.mu.Unlock()
		}
		if cfg.Mode == "remote" {
			if err := s.checkRemoteProcessor(ctx, cfg.Profiles[cfg.ActiveID], cfg.RemoteProxy); err != nil {
				return err
			}
		}
		c.mu.Lock()
		current := c.process
		alive := false
		if current != nil {
			select {
			case <-current.done:
			default:
				alive = true
			}
		}
		c.mu.Unlock()
		if alive {
			if action == "start" {
				return c.activateCapture(current)
			}
			return nil
		}
		if action == "authorize" && cfg.Enabled {
			return errors.New("stop_capture_before_authorizing")
		}
		return s.startCaptureProcess(action == "start")
	case "stop":
		c.mu.Lock()
		p := c.process
		before := c.settings.Enabled
		c.settings.Enabled = false
		err := c.saveLocked()
		if err != nil {
			c.settings.Enabled = before
		}
		c.mu.Unlock()
		if err != nil {
			return errors.New("capture_persist_failed")
		}
		if p != nil {
			if err := p.stop(); err != nil {
				c.mu.Lock()
				c.lastError = "capture_stop_failed"
				c.mu.Unlock()
				return err
			}
		}
		c.mu.Lock()
		if c.process == p {
			c.process = nil
		}
		c.lastError = ""
		c.mu.Unlock()
		return nil
	default:
		return errors.New("unknown_setup_action")
	}
}
func (s *Server) startCaptureProcess(protected bool) error {
	c := s.capture
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errors.New("capture_shutting_down")
	}
	old := c.process
	c.process = nil
	c.mu.Unlock()
	if old != nil {
		if err := old.stop(); err != nil {
			c.mu.Lock()
			c.process = old
			c.mu.Unlock()
			return err
		}
	}
	path, _ := captureExecutable()
	addon, err := captureAssets.ReadFile("capture_assets/addon.py")
	if err != nil {
		return errors.New("capture_addon_missing")
	}
	if err := writeConfigBytes(filepath.Join(c.root, "addon.py"), addon); err != nil {
		return errors.New("capture_addon_write_failed")
	}
	cfg := c.snapshot()
	// Targets become active only after the adapter has installed all hooks and
	// excluded proxy cores. Until then the native backend passes traffic through.
	bridgeConfig := map[string]any{"bridge_url": c.bridgeURL, "bridge_token": c.bridgeToken, "targets": []string{}, "proxies": []string{cfg.ModelProxy, cfg.RemoteProxy}}
	data, _ := json.Marshal(bridgeConfig)
	configPath := filepath.Join(c.root, "bridge.json")
	if err := writeConfigBytes(configPath, data); err != nil {
		return errors.New("capture_config_write_failed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	// Never interpolate process names or credentials into a shell command.
	args := []string{"--mode", "local:!" + strconv.Itoa(os.Getpid()) + ",!clash,!Clash,!mihomo,!Mihomo,!sing-box,!Surge", "--set", "confdir=" + c.root, "--set", "connection_strategy=lazy", "--set", "upstream_cert=false", "--set", "flow_detail=0", "--set", "termlog_verbosity=error", "-s", filepath.Join(c.root, "addon.py")}
	cmd := exec.CommandContext(ctx, path, args...)
	configureCaptureProcess(cmd)
	cmd.WaitDelay = 2 * time.Second
	diagnostics := &captureDiagnostics{}
	cmd.Stderr = diagnostics
	// Vendor runtimes carry their own libraries. The gateway's loader override
	// must not replace libraries inside the signed app or standalone helper.
	cmd.Env = captureRuntimeEnvironment()
	cmd.Env = append(cmd.Env, "SILENCER_CAPTURE_BRIDGE_CONFIG="+configPath)
	if runtime.GOOS == "linux" {
		if err := prepareCaptureLinuxAuthorization(cmd, c.root); err != nil {
			cancel()
			return err
		}
	}
	p := &captureProcess{cmd: cmd, cancel: cancel, done: make(chan struct{})}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		cancel()
		return errors.New("capture_shutting_down")
	}
	cleanup, err := startCaptureProcess(cmd)
	if err != nil {
		c.mu.Unlock()
		cancel()
		return errors.New("capture_start_failed")
	}
	p.cleanup = cleanup
	c.process = p
	c.lastError = ""
	c.mu.Unlock()
	go func() {
		err := cmd.Wait()
		c.mu.Lock()
		if c.process == p && err != nil {
			c.lastError = diagnostics.code()
		}
		c.mu.Unlock()
		close(p.done)
	}()
	deadline := time.NewTimer(90 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-p.done:
			return errors.New(diagnostics.code())
		case <-deadline.C:
			p.stop()
			return errors.New("capture_ready_timeout")
		case <-ticker.C:
			c.mu.Lock()
			ready := !p.heartbeat.IsZero()
			c.mu.Unlock()
			if ready {
				if protected {
					return c.activateCapture(p)
				}
				return nil
			}
		}
	}
}

func (c *captureController) activateCapture(p *captureProcess) error {
	c.mu.Lock()
	previous, complete := c.settings.Enabled, c.settings.SetupComplete
	c.settings.Enabled = true
	err := c.saveLocked()
	if err != nil {
		c.settings.Enabled = previous
	}
	c.mu.Unlock()
	if err != nil {
		return errors.New("capture_persist_failed")
	}
	deadline := time.NewTimer(12 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for err == nil {
		select {
		case <-p.done:
			err = errors.New("capture_process_exited")
		case <-deadline.C:
			err = errors.New("capture_activation_timeout")
		case <-ticker.C:
			c.mu.Lock()
			ready := p.protected && time.Since(p.heartbeat) < 10*time.Second
			if c.closed {
				err = errors.New("capture_shutting_down")
			}
			if ready && err == nil {
				c.settings.SetupComplete = true
				if c.saveLocked() == nil {
					c.mu.Unlock()
					return nil
				}
				err = errors.New("capture_persist_failed")
			}
			c.mu.Unlock()
		}
	}
	c.mu.Lock()
	c.settings.Enabled, c.settings.SetupComplete = previous, complete
	if c.saveLocked() != nil {
		err = errors.New("capture_persist_failed")
	}
	c.lastError = err.Error()
	c.mu.Unlock()
	p.stop()
	return err
}

func prepareCaptureLinuxAuthorization(cmd *exec.Cmd, root string) error {
	if err := captureLinuxDependencies(exec.LookPath, os.Getenv("DISPLAY"), os.Getenv("WAYLAND_DISPLAY")); err != nil {
		return err
	}
	dir := filepath.Join(root, "authorization")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return errors.New("capture_authorization_setup_failed")
	}
	for name, asset := range map[string]string{"sudo": "linux-sudo.sh", "askpass": "linux-askpass.sh"} {
		data, err := captureAssets.ReadFile("capture_assets/" + asset)
		if err != nil {
			return err
		}
		path := filepath.Join(dir, name)
		if err := writeConfigBytes(path, data); err != nil {
			return err
		}
		if err := os.Chmod(path, 0700); err != nil {
			return err
		}
	}
	cmd.Env = append(cmd.Env, "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "SUDO_ASKPASS="+filepath.Join(dir, "askpass"))
	return nil
}

func captureRuntimeEnvironment() []string {
	var result []string
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "LD_LIBRARY_PATH=") && !strings.HasPrefix(entry, "DYLD_LIBRARY_PATH=") {
			result = append(result, entry)
		}
	}
	return result
}

func ensureCaptureCA(root string) error {
	path := filepath.Join(root, "mitmproxy-ca.pem")
	if data, err := os.ReadFile(path); err == nil {
		if pair, err := tls.X509KeyPair(data, data); err == nil && len(pair.Certificate) > 0 {
			cert, err := x509.ParseCertificate(pair.Certificate[0])
			if err == nil && cert.IsCA {
				return writeConfigBytes(filepath.Join(root, "mitmproxy-ca-cert.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}))
			}
		}
		return errors.New("capture_certificate_invalid")
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return errors.New("certificate_generation_failed")
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return err
	}
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "Silencer Local Capture", Organization: []string{"Silencer"}}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(2, 0, 0), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return err
	}
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	private := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := writeConfigBytes(path, append(private, cert...)); err != nil {
		return err
	}
	return writeConfigBytes(filepath.Join(root, "mitmproxy-ca-cert.pem"), cert)
}
func captureCertificateTrusted(root string) bool {
	data, err := os.ReadFile(filepath.Join(root, "mitmproxy-ca-cert.pem"))
	if err != nil {
		return false
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return false
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false
	}
	roots := captureSystemCertPool()
	_, err = cert.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}})
	return err == nil
}
func captureSystemCertPool() *x509.CertPool {
	roots, _ := x509.SystemCertPool()
	if runtime.GOOS == "linux" {
		roots = x509.NewCertPool()
		for _, path := range []string{"/etc/ssl/certs/ca-certificates.crt", "/etc/pki/tls/certs/ca-bundle.crt"} {
			if data, e := os.ReadFile(path); e == nil {
				roots.AppendCertsFromPEM(data)
			}
		}
	}
	if roots == nil {
		roots = x509.NewCertPool()
	}
	return roots
}
func trustCaptureCA(ctx context.Context, root string) error {
	cert := filepath.Join(root, "mitmproxy-ca-cert.pem")
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.CommandContext(ctx, "certutil", "-user", "-addstore", "Root", cert)
	case "darwin":
		// AppleScript receives a quoted literal, never untrusted shell text.
		shell := "/usr/bin/security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain " + shellQuote(cert)
		encoded, _ := json.Marshal(shell)
		cmd = exec.CommandContext(ctx, "osascript", "-e", "do shell script "+string(encoded)+" with administrator privileges")
	default:
		if _, err := exec.LookPath("pkexec"); err != nil {
			return errors.New("manual_certificate_trust_required")
		}
		data, err := os.ReadFile(cert)
		if err != nil {
			return errors.New("capture_certificate_missing")
		}
		digest := sha256.Sum256(data)
		destination := "/usr/local/share/ca-certificates/silencer-" + hex.EncodeToString(digest[:8]) + ".crt"
		cmd = exec.CommandContext(ctx, "pkexec", "/bin/sh", "-c", `install -m 0644 -- "$1" "$2" && update-ca-certificates`, "silencer", cert, destination)
	}
	if output, err := cmd.CombinedOutput(); err != nil {
		return captureTrustFailure(runtime.GOOS, string(output))
	}
	return nil
}
func captureTrustFailure(platform, output string) error {
	message := strings.ToLower(output)
	if platform == "darwin" && (strings.Contains(message, "no user interaction was possible") || strings.Contains(message, "interaction is not allowed")) {
		return errors.New("manual_certificate_trust_required")
	}
	return errors.New("certificate_trust_denied")
}

func capturePublicCertificate(root string) ([]byte, map[string]string, error) {
	data, err := os.ReadFile(filepath.Join(root, "mitmproxy-ca-cert.pem"))
	if err != nil {
		return nil, nil, errors.New("capture_certificate_missing")
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, nil, errors.New("capture_certificate_invalid")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil || !cert.IsCA {
		return nil, nil, errors.New("capture_certificate_invalid")
	}
	digest := sha256.Sum256(cert.Raw)
	// Re-encode only the public certificate, even if the file is malformed and
	// contains extra PEM blocks. Never return mitmproxy-ca.pem or private keys.
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), map[string]string{"subject": cert.Subject.CommonName, "sha256": strings.ToUpper(hex.EncodeToString(digest[:])), "filename": "silencer-local-ca.crt"}, nil
}
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
func (s *Server) serveCaptureAsset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.captureError(w, 405, "method_not_allowed")
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/capture-assets/")
	if name != "install.sh" && name != "install.ps1" {
		s.captureError(w, 404, "asset_not_found")
		return
	}
	data, err := captureAssets.ReadFile("capture_assets/" + name)
	if err != nil {
		s.captureError(w, 404, "asset_not_found")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename="+name)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(data)
}
