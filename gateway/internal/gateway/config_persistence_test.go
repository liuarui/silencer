package gateway

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestControlOnlyDeploymentPersistsRuntimeAndPassword(t *testing.T) {
	control := filepath.Join(t.TempDir(), "control.json")
	t.Setenv("SILENCER_CONFIG_FILE", "")
	t.Setenv("SILENCER_CONTROL_CONFIG_FILE", control)
	t.Setenv("SILENCER_GATEWAY_TOKEN", "initial-test-password")
	t.Setenv("SILENCER_MAX_CONCURRENCY", "")
	t.Setenv("SILENCER_MAX_RESPONSE_BYTES", "")
	t.Setenv("SILENCER_ENGINE", "test")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal("first startup failed", err)
	}
	s, err := NewServer(cfg, testEngine{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	request := func(path, body string) {
		t.Helper()
		r := httptest.NewRequest("POST", path, strings.NewReader(body))
		r.Header.Set("X-Silencer-Token", "initial-test-password")
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("%s returned %d: %s", path, w.Code, w.Body.String())
		}
	}
	request("/api/config", `{"max_concurrency":7,"max_response_bytes":1048576,"session_ttl":777}`)
	request("/api/auth/change-password", `{"current_password":"initial-test-password","new_password":" replacement-test-password "}`)
	loaded, err := LoadConfig()
	if err != nil || loaded.MaxConcurrency != 7 || loaded.MaxResponseBytes != 1048576 || loaded.AuthToken != " replacement-test-password " {
		t.Fatalf("settings lost on reload: concurrency=%d response=%d authChanged=%t err=%v", loaded.MaxConcurrency, loaded.MaxResponseBytes, loaded.AuthToken != cfg.AuthToken, err)
	}
	for _, path := range []string{control, runtimeConfigPath(), authPersistencePath()} {
		checkPrivateConfigPermissions(t, path)
	}
	// Operator environment overrides remain intentional and take precedence.
	t.Setenv("SILENCER_MAX_CONCURRENCY", "9")
	loaded, err = LoadConfig()
	if err != nil || loaded.MaxConcurrency != 9 {
		t.Fatal("explicit environment override lost", err)
	}
}

func TestExplicitMissingRuntimeConfigStillFails(t *testing.T) {
	t.Setenv("SILENCER_CONFIG_FILE", filepath.Join(t.TempDir(), "missing.json"))
	t.Setenv("SILENCER_CONTROL_CONFIG_FILE", "")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("missing explicitly requested config was accepted")
	}
}
