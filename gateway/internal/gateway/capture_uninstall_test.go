package gateway

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCaptureUninstallRequiresConfirmedProcessExit(t *testing.T) {
	s := newCaptureTestServer(t, true, testEngine{})
	p := &captureProcess{done: make(chan struct{}), cancel: func() {}}
	s.capture.process = p
	s.capture.settings.Enabled = true
	if err := s.captureAction(context.Background(), "stop"); err == nil || err.Error() != "capture_stop_failed" {
		t.Fatal("unconfirmed stop was accepted", err)
	}
	if s.capture.process != p || s.captureUninstallStatus()["capture_stopped"] != false {
		t.Fatal("lost ownership of a process that has not exited")
	}
	w := captureControl(t, s, "GET", "/api/local/setup/uninstall-script", "")
	if w.Code != 409 {
		t.Fatal("unconfirmed process exit allowed uninstall", w.Code)
	}
	close(p.done)
	if err := s.captureAction(context.Background(), "stop"); err != nil || s.captureUninstallStatus()["capture_stopped"] != true {
		t.Fatal("confirmed exit did not unblock uninstall", err)
	}
}

func TestCaptureUninstallDownloadScopeAndAuthentication(t *testing.T) {
	s := newCaptureTestServer(t, true, testEngine{})
	path := "/api/local/setup/uninstall-script"
	r := httptest.NewRequest("GET", "http://127.0.0.1:8080"+path, nil)
	r.RemoteAddr = "127.0.0.1:34567"
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("uninstall export bypassed authentication", w.Code)
	}
	if err := ensureCaptureCA(s.capture.root); err != nil {
		t.Fatal(err)
	}
	public, info, _ := capturePublicCertificate(s.capture.root)
	private, _ := os.ReadFile(filepath.Join(s.capture.root, "mitmproxy-ca.pem"))
	if err := os.WriteFile(filepath.Join(s.capture.root, "mitmproxy-ca-cert.pem"), append(public, private...), 0600); err != nil {
		t.Fatal(err)
	}
	w = captureControl(t, s, "GET", path, "")
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Body.String(), info["sha256"]) || !strings.Contains(w.Body.String(), base64.StdEncoding.EncodeToString(public)) || strings.Contains(w.Body.String(), base64.StdEncoding.EncodeToString(private)) || strings.Contains(w.Body.String(), "management-secret") || strings.Contains(w.Body.String(), "PRIVATE KEY") {
		t.Fatal("invalid scoped uninstall export", w.Code)
	}
	for _, query := range []string{"?command=rm", "?root=/tmp/other", "?keep_certificate=yes", "?keep_certificate=1&keep_certificate=0"} {
		if w := captureControl(t, s, "GET", path+query, ""); w.Code != 400 {
			t.Fatal("arbitrary uninstall options accepted", query, w.Code)
		}
	}
	if w := captureControl(t, s, "POST", path, `{}`); w.Code != 405 {
		t.Fatal("uninstall export accepted mutation", w.Code)
	}
	s.capture.mu.Lock()
	s.capture.settings.Enabled = true
	s.capture.mu.Unlock()
	for _, endpoint := range []string{path, "/api/local/setup/actions"} {
		method, body := "GET", ""
		if strings.HasSuffix(endpoint, "actions") {
			method, body = "POST", `{"action":"uninstall"}`
		}
		w := captureControl(t, s, method, endpoint, body)
		if w.Code != 409 || !strings.Contains(w.Body.String(), "stop_capture_before_uninstall") {
			t.Fatal("running capture allowed uninstall", endpoint, w.Code)
		}
	}
}

func TestCaptureUninstallScriptQuotingAndEncoding(t *testing.T) {
	root := filepath.Join(t.TempDir(), "安装's $HOME `literal`", "data")
	state := filepath.Join(root, "control.json.capture")
	if err := os.MkdirAll(state, 0700); err != nil {
		t.Fatal(err)
	}
	if err := ensureCaptureCA(state); err != nil {
		t.Fatal(err)
	}
	for _, platform := range []string{"darwin", "linux", "windows"} {
		if err := os.WriteFile(filepath.Join(root, ".silencer-local-install"), []byte(platform), 0600); err != nil {
			t.Fatal(err)
		}
		for _, keep := range []bool{false, true} {
			data, filename, err := captureUninstallScript(platform, root, state, keep)
			if err != nil || strings.Contains(string(data), "__SILENCER_") {
				t.Fatal(platform, err)
			}
			path := filepath.Join(t.TempDir(), filename)
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if platform == "windows" {
				if !strings.HasPrefix(string(data), "\xef\xbb\xbf") || !strings.Contains(string(data), "安装''s $HOME `literal`") {
					t.Fatal("PowerShell literal/UTF-8 encoding lost")
				}
			} else if runtime.GOOS != "windows" {
				if out, err := exec.Command("/bin/bash", "-n", path).CombinedOutput(); err != nil {
					t.Fatal(platform, err, string(out))
				}
			}
		}
	}
}

func TestCaptureCleanupVerificationDoesNotMistakeExpiryForRemoval(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Cleanup test CA"}, NotBefore: time.Now().Add(-48 * time.Hour), NotAfter: time.Now().Add(-24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	trusted := x509.NewCertPool()
	trusted.AddCert(cert)
	if state, err := captureCleanupTrust(cert, trusted); err != nil || state != "trusted" {
		t.Fatal("expired trusted CA was mistaken for removed", state, err)
	}
	if state, err := captureCleanupTrust(cert, x509.NewCertPool()); err != nil || state != "untrusted" {
		t.Fatal(state, err)
	}
	if _, err := captureCleanupTrust(cert, nil); err == nil {
		t.Fatal("unavailable trust store was accepted")
	}
	path := filepath.Join(t.TempDir(), "public.crt")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(der)
	if state, err := CheckCaptureCleanupCA(path, hex.EncodeToString(digest[:]), true); err != nil || state != "matched" {
		t.Fatal(state, err)
	}
	if _, err := CheckCaptureCleanupCA(path, strings.Repeat("0", 64), true); err == nil {
		t.Fatal("different CA identity accepted")
	}
}
