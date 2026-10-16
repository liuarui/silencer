package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/silencer/gateway/internal/hostagent"
)

func TestHostAgentControlPlaneUsesReactContract(t *testing.T) {
	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "client.json")
	if err := os.WriteFile(configPath, []byte(`{"base_url":"https://api.example.com/v1"}`), 0600); err != nil {
		t.Fatal(err)
	}
	socketDir, err := os.MkdirTemp("/tmp", "gateway-ha-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socket := filepath.Join(socketDir, "agent.sock")
	agent := &hostagent.Server{Socket: socket, ConfigPath: configPath, StatePath: filepath.Join(configDir, "history.json")}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- agent.ListenAndServe(ctx) }()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(socket); err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}

	s, err := NewServer(Config{MaxConcurrency: 2, Engine: "go-fallback", HostAgentSocket: socket}, testEngine{})
	if err != nil {
		t.Fatal(err)
	}
	handler := s.Handler()
	request := func(method, path string, body string) map[string]any {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s: status=%d body=%s", method, path, rec.Code, rec.Body.String())
		}
		var value map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &value); err != nil {
			t.Fatal(err)
		}
		return value
	}

	discover := request(http.MethodGet, "/api/clients/discover", "")
	if discover["scope"] != "host" || discover["available"] != true {
		t.Fatalf("discover=%#v", discover)
	}
	clients, ok := discover["clients"].([]any)
	if !ok || len(clients) != 1 {
		t.Fatalf("discover clients=%#v", discover["clients"])
	}

	planResponse := request(http.MethodPost, "/api/clients/plan", `{"config_path":"`+configPath+`","proxy_url":"http://127.0.0.1:8080/r/default/v1"}`)
	plan, ok := planResponse["plan"].(map[string]any)
	if !ok || plan["status"] != "planned" || plan["id"] == "" {
		t.Fatalf("plan=%#v", planResponse)
	}
	applyResponse := request(http.MethodPost, "/api/clients/apply", `{"plan":`+mustJSON(t, plan)+`}`)
	change, ok := applyResponse["change"].(map[string]any)
	if !ok || change["status"] != "applied" {
		t.Fatalf("apply=%#v", applyResponse)
	}
	history := request(http.MethodGet, "/api/clients/history", "")
	if entries, ok := history["changes"].([]any); !ok || len(entries) != 1 {
		t.Fatalf("history=%#v", history)
	}
	rollback := request(http.MethodPost, "/api/clients/rollback", `{"change":`+mustJSON(t, change)+`}`)
	if rollbackChange, ok := rollback["change"].(map[string]any); !ok || rollbackChange["status"] != "rolled_back" {
		t.Fatalf("rollback=%#v", rollback)
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
