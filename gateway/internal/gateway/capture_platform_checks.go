package gateway

import (
	"errors"
	"strconv"
	"strings"
	"sync"
)

func captureWindowsProxy(output string) (string, string) {
	values := map[string]string{}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.HasPrefix(fields[1], "REG_") {
			values[strings.ToLower(fields[0])] = strings.Join(fields[2:], " ")
		}
	}
	autoDetect, _ := strconv.ParseUint(values["autodetect"], 0, 32)
	if strings.TrimSpace(values["autoconfigurl"]) != "" || autoDetect == 1 {
		return "", "proxy_auto_config_requires_confirmation"
	}
	enabled, err := strconv.ParseUint(values["proxyenable"], 0, 32)
	if values["proxyenable"] == "" || (err == nil && enabled == 0) {
		return "", ""
	}
	if err != nil || enabled != 1 {
		return "", "proxy_config_requires_confirmation"
	}
	value := strings.TrimSpace(values["proxyserver"])
	if strings.Contains(value, "=") {
		protocols := map[string]string{}
		for _, entry := range strings.Split(value, ";") {
			pair := strings.SplitN(entry, "=", 2)
			if len(pair) == 2 {
				protocols[strings.ToLower(strings.TrimSpace(pair[0]))] = strings.TrimSpace(pair[1])
			}
		}
		value = protocols["https"]
		if value == "" {
			value = protocols["http"]
		}
	}
	if value == "" {
		return "", "proxy_config_requires_confirmation"
	}
	if !strings.Contains(value, "://") {
		value = "http://" + value
	}
	if validateCaptureProxy(value) != nil {
		return "", "proxy_config_requires_confirmation"
	}
	return value, ""
}

func captureLinuxDependencies(lookPath func(string) (string, error), display, wayland string) error {
	for _, name := range []string{"sudo", "lsof"} {
		if _, err := lookPath(name); err != nil {
			return errors.New("capture_linux_" + name + "_required")
		}
	}
	_, zenity := lookPath("zenity")
	_, kdialog := lookPath("kdialog")
	if zenity != nil && kdialog != nil {
		return errors.New("capture_linux_authorization_ui_required")
	}
	if display == "" && wayland == "" {
		return errors.New("capture_linux_authorization_display_required")
	}
	return nil
}

// Keep only a bounded in-memory tail. Raw backend errors may contain endpoint
// or credential data, so the UI and ledger receive classified codes only.
type captureDiagnostics struct {
	mu   sync.Mutex
	tail []byte
}

func (d *captureDiagnostics) Write(data []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	const capacity = 8192
	d.tail = append(d.tail, data...)
	if len(d.tail) > capacity {
		d.tail = append([]byte(nil), d.tail[len(d.tail)-capacity:]...)
	}
	return len(data), nil
}

func (d *captureDiagnostics) code() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	text := strings.ToLower(string(d.tail))
	for _, marker := range []string{"cannot open display", "no display name", "could not connect to display"} {
		if strings.Contains(text, marker) {
			return "capture_linux_authorization_display_required"
		}
	}
	for _, marker := range []string{"no password was provided", "a password is required", "incorrect password", "authentication failure", "the operation was canceled by the user"} {
		if strings.Contains(text, marker) {
			return "capture_authorization_denied"
		}
	}
	if strings.Contains(text, "libbpf") || strings.Contains(text, "failed to load bpf") {
		return "capture_linux_backend_unavailable"
	}
	return "capture_authorization_or_start_failed"
}
