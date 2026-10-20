package gateway

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Verify an actual TLS handshake against a leaf issued by this installation's
// CA. It uses a private ephemeral listener, without changing host trust or routes.
func captureTLSProbe(ctx context.Context, root string, roots *x509.CertPool) error {
	data, err := os.ReadFile(filepath.Join(root, "mitmproxy-ca.pem"))
	if err != nil {
		return errors.New("capture_certificate_missing")
	}
	ca, err := tls.X509KeyPair(data, data)
	if err != nil || len(ca.Certificate) == 0 {
		return errors.New("capture_certificate_invalid")
	}
	issuer, err := x509.ParseCertificate(ca.Certificate[0])
	if err != nil {
		return errors.New("capture_certificate_invalid")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return errors.New("capture_tls_probe_failed")
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return errors.New("capture_tls_probe_failed")
	}
	leaf := &x509.Certificate{SerialNumber: serial, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Minute), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, leaf, issuer, &key.PublicKey, ca.PrivateKey)
	if err != nil {
		return errors.New("capture_tls_probe_failed")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return errors.New("capture_tls_probe_failed")
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		defer connection.Close()
		server := tls.Server(connection, &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{der, issuer.Raw}, PrivateKey: key}}})
		_ = server.HandshakeContext(ctx)
	}()
	connection, err := (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
	if err != nil {
		return errors.New("capture_tls_probe_failed")
	}
	defer connection.Close()
	client := tls.Client(connection, &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: "127.0.0.1"})
	if err := client.HandshakeContext(ctx); err != nil {
		return errors.New("capture_tls_trust_failed")
	}
	return nil
}

type captureSetupJob struct {
	ID        string    `json:"id"`
	Action    string    `json:"action"`
	State     string    `json:"state"`
	Error     string    `json:"error,omitempty"`
	PID       int       `json:"pid,omitempty"`
	CreatedAt time.Time `json:"created_at,omitempty"`
}

func (s *Server) queueCaptureMaintenance(action string) error {
	c := s.capture
	if action == "uninstall" && s.captureUninstallStatus()["capture_stopped"] != true {
		return errors.New("stop_capture_before_uninstall")
	}
	root := filepath.Dir(s.configPath)
	if _, err := os.Stat(filepath.Join(root, ".silencer-local-install")); err != nil {
		return errors.New("managed_install_required")
	}
	name := "install.sh"
	if runtime.GOOS == "windows" {
		name = "install.ps1"
	}
	data, err := captureAssets.ReadFile("capture_assets/" + name)
	if err != nil {
		return errors.New("installer_missing")
	}
	if existing := readCaptureJob(c.root); existing != nil {
		job := existing.(captureSetupJob)
		if job.State == "queued" || job.State == "running" {
			if job.Error == "maintenance_status_unavailable" {
				return errors.New(job.Error)
			}
			return errors.New("maintenance_in_progress")
		}
	}
	job := captureSetupJob{ID: newRequestID(), Action: action, State: "queued", CreatedAt: time.Now().UTC()}
	// A unique filename lets process verification distinguish this maintenance
	// task from a reused PID or another installer. No credentials enter argv.
	path := filepath.Join(c.root, "setup-"+job.ID+filepath.Ext(name))
	if err := writeConfigBytes(path, data); err != nil {
		return errors.New("installer_write_failed")
	}
	encoded, _ := json.Marshal(job)
	if err := writeConfigBytes(filepath.Join(c.root, "setup-job.json"), encoded); err != nil {
		return errors.New("setup_job_persist_failed")
	}
	// Start immediately and persist the actual process identity. The installer
	// reports its PID too, then pauses before changing the service so this HTTP
	// response can finish. It survives the gateway's restart/uninstall.
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		args := []string{"-NoProfile", "-ExecutionPolicy", "RemoteSigned", "-File", path}
		if action == "uninstall" {
			args = append(args, "-Uninstall")
		}
		cmd = exec.Command("powershell.exe", args...)
	} else {
		args := []string{path}
		if action == "uninstall" {
			args = append(args, "--uninstall")
		}
		cmd = exec.Command("/bin/bash", args...)
	}
	cmd.Env = append(os.Environ(), "SILENCER_DATA_DIR="+root, "SILENCER_SETUP_JOB_ID="+job.ID, "SILENCER_SETUP_JOB_ACTION="+action, "SILENCER_SETUP_JOB_CREATED_AT="+job.CreatedAt.Format(time.RFC3339Nano), "SILENCER_SETUP_JOB_FILE="+filepath.Join(c.root, "setup-job.json"))
	detachMaintenance(cmd)
	if err := cmd.Start(); err != nil {
		job.State, job.Error = "failed", "maintenance_start_failed"
		encoded, _ := json.Marshal(job)
		_ = writeConfigBytes(filepath.Join(c.root, "setup-job.json"), encoded)
		return errors.New("maintenance_start_failed")
	}
	job.State = "running"
	job.PID = cmd.Process.Pid
	encoded, _ = json.Marshal(job)
	if err := writeConfigBytes(filepath.Join(c.root, "setup-job.json"), encoded); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return errors.New("setup_job_persist_failed")
	}
	go func() {
		if err := cmd.Wait(); err != nil {
			job.State = "failed"
			job.Error = "maintenance_failed"
		} else {
			job.State = "complete"
		}
		c.finishMaintenance(job)
	}()
	return nil
}

func (c *captureController) finishMaintenance(job captureSetupJob) {
	// Serialize the identity check and final write with task creation. The old
	// installer's exit must never release the lock held by a newer task.
	c.actionMu.Lock()
	defer c.actionMu.Unlock()
	if current, err := readCaptureJobRaw(c.root); err == nil && current != nil && current.ID == job.ID {
		encoded, _ := json.Marshal(job)
		_ = writeConfigBytes(filepath.Join(c.root, "setup-job.json"), encoded)
	}
}

func (s *Server) checkCapturePipeline(ctx context.Context) error {
	streamer, ok := s.currentEngine().(StreamingMaskingEngine)
	if !ok {
		return errors.New("pipeline_streaming_unavailable")
	}
	session, err := streamer.NewMaskingSession(ctx)
	if err != nil {
		return errors.New("pipeline_mask_failed")
	}
	owner, ok := session.(ContextualMaskingSession)
	if !ok {
		if abort, ok := session.(AbortableMaskingSession); ok {
			abort.Abort()
		}
		return errors.New("pipeline_context_unavailable")
	}
	defer owner.Release()
	original := []byte(`{"probe":"alice@example.com"}`)
	masked, err := session.Push(ctx, original)
	if err != nil {
		if abort, ok := session.(AbortableMaskingSession); ok {
			abort.Abort()
		}
		return errors.New("pipeline_mask_failed")
	}
	tail, err := session.Finish(ctx)
	if err != nil {
		return errors.New("pipeline_mask_failed")
	}
	masked = append(masked, tail...)
	restore, err := owner.NewRestoreSession(ctx)
	if err != nil {
		return errors.New("pipeline_restore_failed")
	}
	finished := false
	defer func() {
		if abort, ok := restore.(AbortableRestoreSession); ok && !finished {
			abort.Abort()
		}
	}()
	restored, err := restore.Push(ctx, masked)
	if err != nil {
		return errors.New("pipeline_restore_failed")
	}
	tail, err = restore.Finish(ctx)
	finished = true
	restored = append(restored, tail...)
	if err != nil || !bytes.Equal(original, restored) {
		return errors.New("pipeline_roundtrip_failed")
	}
	return nil
}

// StartLocalCapture is called after storage initialization. The process
// supervisor retries crashes with backoff; it does not claim that every client
// is protected just because the runtime is running.
func (s *Server) StartLocalCapture() {
	if !s.Config().LocalMode {
		return
	}
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			c := s.capture
			c.mu.Lock()
			closed := c.closed
			enabled := c.settings.Enabled
			process := c.process
			alive := false
			if process != nil {
				select {
				case <-process.done:
				default:
					alive = true
				}
			}
			c.mu.Unlock()
			if closed {
				return
			}
			if enabled && !alive {
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				err := s.captureAction(ctx, "start")
				cancel()
				if err != nil {
					c.mu.Lock()
					c.lastError = err.Error()
					c.mu.Unlock()
				}
			}
			select {
			case <-ticker.C:
			}
		}
	}()
}

func detectCaptureSystemProxy(ctx context.Context) (string, string) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if runtime.GOOS == "darwin" {
		data, err := exec.CommandContext(ctx, "/usr/sbin/scutil", "--proxy").Output()
		if err != nil {
			return "", "proxy_detection_unavailable"
		}
		values := map[string]string{}
		for _, line := range strings.Split(string(data), "\n") {
			parts := strings.SplitN(strings.TrimSpace(line), " : ", 2)
			if len(parts) == 2 {
				values[parts[0]] = parts[1]
			}
		}
		if values["ProxyAutoConfigEnable"] == "1" {
			return "", "proxy_auto_config_requires_confirmation"
		}
		for _, prefix := range []string{"HTTPS", "HTTP"} {
			if values[prefix+"Enable"] == "1" {
				candidate := "http://" + values[prefix+"Proxy"] + ":" + values[prefix+"Port"]
				if validateCaptureProxy(candidate) == nil {
					return candidate, ""
				}
			}
		}
	}
	if runtime.GOOS == "windows" {
		data, err := exec.CommandContext(ctx, "reg.exe", "query", `HKCU\Software\Microsoft\Windows\CurrentVersion\Internet Settings`).Output()
		if err != nil {
			return "", "proxy_detection_unavailable"
		}
		return captureWindowsProxy(string(data))
	}
	if runtime.GOOS == "linux" {
		mode, _ := exec.CommandContext(ctx, "gsettings", "get", "org.gnome.system.proxy", "mode").Output()
		if strings.TrimSpace(string(mode)) == "'auto'" {
			return "", "proxy_auto_config_requires_confirmation"
		}
		if strings.TrimSpace(string(mode)) == "'manual'" {
			host, _ := exec.CommandContext(ctx, "gsettings", "get", "org.gnome.system.proxy.http", "host").Output()
			port, _ := exec.CommandContext(ctx, "gsettings", "get", "org.gnome.system.proxy.http", "port").Output()
			candidate := "http://" + strings.Trim(strings.TrimSpace(string(host)), "'") + ":" + strings.TrimSpace(string(port))
			if validateCaptureProxy(candidate) == nil {
				return candidate, ""
			}
		}
	}
	return "", ""
}

func (s *Server) probeRemoteCapture(ctx context.Context, p processingProfile, egress string) error {
	if err := s.checkRemoteProcessor(ctx, p, egress); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.URL+"/api/capture/probe", nil)
	if err != nil {
		return errors.New("remote_url_invalid")
	}
	req.Header.Set("X-Silencer-Token", p.Token)
	req.Header.Set("X-Silencer-Capture-Protocol", "1")
	client := &http.Client{Transport: s.capture.transport(s.Config(), egress), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("remote_connection_failed")
	}
	defer resp.Body.Close()
	var result struct {
		OK bool `json:"ok"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(io.LimitReader(resp.Body, 65536)).Decode(&result) != nil || !result.OK {
		return errors.New("remote_pipeline_check_failed")
	}
	return nil
}
