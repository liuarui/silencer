package gateway

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Runs the real pinned capture adapter and TLS MITM, but uses an explicit test
// listener instead of altering OS interception, trust or proxy configuration.
func TestCaptureMitmTLSIntegration(t *testing.T) {
	path, err := captureExecutable()
	if err != nil {
		t.Skip("mitmdump is not installed")
	}
	if ok, code := captureRuntimeCheck(context.Background()); !ok {
		t.Skip(code)
	}
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "alice@example.com") {
			t.Error("plaintext reached the model")
		}
		if r.Header.Get("X-Silencer-Capture-Bridge") != "" {
			t.Error("bridge credential leaked")
		}
		if r.Header.Get("Authorization") != "Bearer provider-key" {
			t.Error("provider credential changed")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer upstream.Close()
	s := newCaptureTestServer(t, true, contextualTestEngine{})
	u, _ := url.Parse(upstream.URL)
	s.capture.settings.Targets = []string{u.Host}
	s.capture.settings.Enabled = true
	s.capture.transports[""] = upstream.Client().Transport.(*http.Transport)
	if err := ensureCaptureCA(s.capture.root); err != nil {
		t.Fatal(err)
	}
	addon, _ := captureAssets.ReadFile("capture_assets/addon.py")
	addonPath := filepath.Join(s.capture.root, "adapter.py")
	if err := os.WriteFile(addonPath, addon, 0600); err != nil {
		t.Fatal(err)
	}
	config, _ := json.Marshal(map[string]any{"bridge_url": s.capture.bridgeURL, "bridge_token": s.capture.bridgeToken, "targets": []string{u.Host}})
	configPath := filepath.Join(s.capture.root, "test-bridge.json")
	_ = os.WriteFile(configPath, config, 0600)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	command := exec.CommandContext(ctx, path, "--mode", "regular@"+address, "--set", "confdir="+s.capture.root, "--set", "connection_strategy=lazy", "--set", "upstream_cert=false", "--set", "flow_detail=0", "-s", addonPath)
	configureCaptureProcess(command)
	command.Env = append(os.Environ(), "SILENCER_CAPTURE_BRIDGE_CONFIG="+configPath)
	logPath := filepath.Join(s.capture.root, "mitm-test.log")
	logFile, _ := os.Create(logPath)
	defer logFile.Close()
	command.Stdout = logFile
	command.Stderr = logFile
	cleanup, err := startCaptureProcess(command)
	if err != nil {
		t.Fatal(err)
	}
	process := &captureProcess{cmd: command, done: make(chan struct{}), cancel: cancel, cleanup: cleanup}
	go func() { _ = command.Wait(); close(process.done) }()
	defer func() {
		if err := process.stop(); err != nil {
			t.Errorf("capture fixture cleanup: %v", err)
		}
	}()
	deadline := time.Now().Add(10 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		connection, e := net.DialTimeout("tcp", address, 100*time.Millisecond)
		if e == nil {
			connection.Close()
			ready = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !ready {
		t.Fatalf("mitmdump did not start; inspect %s", logPath)
	}
	ca, _ := os.ReadFile(filepath.Join(s.capture.root, "mitmproxy-ca-cert.pem"))
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca)
	proxyURL, _ := url.Parse("http://" + address)
	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: roots}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	r, _ := http.NewRequest("POST", upstream.URL+"/v1/chat/completions?z=2&a=1", strings.NewReader(`{"text":"alice@example.com"}`))
	r.Header.Set("Authorization", "Bearer provider-key")
	r.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(body), "alice@example.com") {
		t.Fatalf("TLS/bridge roundtrip: %d %s", resp.StatusCode, body)
	}
}
