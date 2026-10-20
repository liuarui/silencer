package gateway

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// CheckCaptureCleanupCA is a read-only CLI used by the downloaded uninstaller.
// Only an explicit unknown-authority result means untrusted. Expiry, a broken
// trust store, a mismatched certificate, and tool failures remain errors.
func CheckCaptureCleanupCA(path, fingerprint string, identityOnly bool) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", errors.New("capture_certificate_invalid")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil || !cert.IsCA {
		return "", errors.New("capture_certificate_invalid")
	}
	digest := sha256.Sum256(cert.Raw)
	if len(fingerprint) != 64 || !strings.EqualFold(hex.EncodeToString(digest[:]), fingerprint) {
		return "", errors.New("capture_certificate_identity_changed")
	}
	if identityOnly {
		return "matched", nil
	}
	roots, err := x509.SystemCertPool()
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "linux" {
		// Read the current bundle afresh. A missing/unreadable bundle must never
		// become an empty trust store and a false cleanup success.
		data, err := os.ReadFile("/etc/ssl/certs/ca-certificates.crt")
		if err != nil {
			return "", err
		}
		roots = x509.NewCertPool()
		if !roots.AppendCertsFromPEM(data) {
			return "", errors.New("capture_system_trust_unavailable")
		}
	}
	return captureCleanupTrust(cert, roots)
}

func captureCleanupTrust(cert *x509.Certificate, roots *x509.CertPool) (string, error) {
	if roots == nil || !cert.NotAfter.After(cert.NotBefore) {
		return "", errors.New("capture_system_trust_unavailable")
	}
	// Probe inside the CA validity window: an expired but trusted CA still needs
	// removal. This never changes the verification policy of live requests.
	at := cert.NotBefore.Add(cert.NotAfter.Sub(cert.NotBefore) / 2)
	_, err := cert.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: at, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}})
	if err == nil {
		return "trusted", nil
	}
	var unknown x509.UnknownAuthorityError
	if errors.As(err, &unknown) {
		return "untrusted", nil
	}
	return "", err
}

func captureManagedInstall(dataRoot, platform string) bool {
	data, err := os.ReadFile(filepath.Join(dataRoot, ".silencer-local-install"))
	return err == nil && strings.TrimSpace(string(data)) == platform
}

func (s *Server) captureUninstallStatus() map[string]any {
	c := s.capture
	c.mu.Lock()
	defer c.mu.Unlock()
	alive := false
	if c.process != nil {
		select {
		case <-c.process.done:
		default:
			alive = true
		}
	}
	dataRoot := filepath.Dir(s.configPath)
	revision := ""
	if info, err := os.Stat(filepath.Join(dataRoot, ".silencer-local-install")); err == nil {
		revision = strconv.FormatInt(info.ModTime().UnixNano(), 10)
	}
	return map[string]any{"managed_install": captureManagedInstall(dataRoot, runtime.GOOS), "capture_stopped": !c.settings.Enabled && !alive, "install_revision": revision}
}

// Downloads contain only this installation's public CA and fixed maintenance
// code. No management token, remote credentials or private CA key is exported.
func captureUninstallScript(platform, dataRoot, captureRoot string, keepCertificate bool) ([]byte, string, error) {
	if platform != "darwin" && platform != "linux" && platform != "windows" {
		return nil, "", errors.New("capture_uninstall_unsupported")
	}
	name := "uninstall.sh"
	installer := "install.sh"
	quote := shellQuote
	if platform == "windows" {
		name, installer = "uninstall.ps1", "install.ps1"
		quote = func(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }
	}
	template, err := captureAssets.ReadFile("capture_assets/" + name)
	if err != nil {
		return nil, "", err
	}
	install, err := captureAssets.ReadFile("capture_assets/" + installer)
	if err != nil {
		return nil, "", err
	}
	public, info, certErr := capturePublicCertificate(captureRoot)
	if certErr != nil {
		if _, err := os.Stat(filepath.Join(captureRoot, "mitmproxy-ca-cert.pem")); !os.IsNotExist(err) {
			return nil, "", certErr
		}
	}
	// Linux's existing trust installer names the file using the stored PEM hash,
	// while the visible identity is the DER hash. Preserve both exact identities.
	stored, _ := os.ReadFile(filepath.Join(captureRoot, "mitmproxy-ca-cert.pem"))
	digest := sha256.Sum256(stored)
	keep := "false"
	if keepCertificate {
		keep = "true"
	}
	managed := "false"
	if captureManagedInstall(dataRoot, platform) {
		managed = "true"
	}
	verifier, err := os.Executable()
	if err != nil {
		return nil, "", err
	}
	replacer := strings.NewReplacer(
		"__SILENCER_DATA_ROOT__", quote(dataRoot),
		"__SILENCER_CAPTURE_ROOT__", quote(captureRoot),
		"__SILENCER_PLATFORM__", quote(platform),
		"__SILENCER_VERIFIER__", quote(verifier),
		"__SILENCER_MANAGED__", managed,
		"__SILENCER_KEEP_CA__", keep,
		"__SILENCER_CA_SHA256__", quote(info["sha256"]),
		"__SILENCER_LINUX_CA_ID__", quote(hex.EncodeToString(digest[:8])),
		"__SILENCER_CA_BASE64__", base64.StdEncoding.EncodeToString(public),
		"__SILENCER_INSTALLER_BASE64__", base64.StdEncoding.EncodeToString(install),
	)
	result := []byte(replacer.Replace(string(template)))
	if platform == "windows" {
		// Windows PowerShell 5.1 needs a BOM to preserve non-ASCII user paths.
		result = append([]byte{0xef, 0xbb, 0xbf}, result...)
	}
	return result, "silencer-" + name, nil
}

func (s *Server) serveCaptureUninstallScript(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.captureError(w, 405, "method_not_allowed")
		return
	}
	query := r.URL.Query()
	for key, values := range query {
		if key != "keep_certificate" || len(values) != 1 || (values[0] != "0" && values[0] != "1") {
			s.captureError(w, 400, "invalid_uninstall_options")
			return
		}
	}
	if s.captureUninstallStatus()["capture_stopped"] != true {
		s.captureError(w, 409, "stop_capture_before_uninstall")
		return
	}
	if existing, ok := readCaptureJob(s.capture.root).(captureSetupJob); ok && (existing.State == "queued" || existing.State == "running") {
		s.captureError(w, 409, "maintenance_in_progress")
		return
	}
	data, filename, err := captureUninstallScript(runtime.GOOS, filepath.Dir(s.configPath), s.capture.root, query.Get("keep_certificate") == "1")
	if err != nil {
		s.captureError(w, 409, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename="+filename)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(data)
}
