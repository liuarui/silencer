package hostagent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func requestPayload(t *testing.T, value any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func responsePlan(t *testing.T, response Response) Plan {
	t.Helper()
	data, ok := response.Data.(map[string]any)
	if !ok {
		t.Fatalf("data type %T", response.Data)
	}
	b, err := json.Marshal(data["plan"])
	if err != nil {
		t.Fatal(err)
	}
	var plan Plan
	if err := json.Unmarshal(b, &plan); err != nil {
		t.Fatal(err)
	}
	return plan
}

func responseChange(t *testing.T, response Response) Change {
	t.Helper()
	data, ok := response.Data.(map[string]any)
	if !ok {
		t.Fatalf("data type %T", response.Data)
	}
	b, err := json.Marshal(data["change"])
	if err != nil {
		t.Fatal(err)
	}
	var change Change
	if err := json.Unmarshal(b, &change); err != nil {
		t.Fatal(err)
	}
	return change
}

func TestPlanApplyRollbackAndExternalModification(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "client.json")
	original := "{\n  \"base_url\": \"https://alice:secret@api.example.com/v1?token=private\",\n  \"api_key\": \"credential-value\",\n  \"nested\": {\"api_base\": \"https://api.example.com\"},\n  \"large_number\": 9007199254740993\n}\n"
	if err := os.WriteFile(path, []byte(original), 0640); err != nil {
		t.Fatal(err)
	}
	originalInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	originalMode := uint32(originalInfo.Mode().Perm())
	s := &Server{ConfigPath: path, StatePath: filepath.Join(dir, "history.json")}

	discover := s.process(Request{Op: "discover"})
	if !discover.OK || !discover.Available {
		t.Fatalf("discover=%+v", discover)
	}
	discoverData := discover.Data.(map[string]any)
	clients := discoverData["clients"].([]map[string]any)
	if len(clients) != 1 || clients[0]["client"] != "client" {
		t.Fatalf("discover clients=%#v", clients)
	}

	planResponse := s.process(Request{Op: "plan", Payload: requestPayload(t, map[string]any{
		"config_path": path,
		"proxy_url":   "http://127.0.0.1:8080/r/default/v1",
	})})
	if !planResponse.OK {
		t.Fatalf("plan=%+v", planResponse)
	}
	plan := responsePlan(t, planResponse)
	if plan.Status != "planned" || plan.ID == "" || len(plan.Changes) != 2 {
		t.Fatalf("plan=%+v", plan)
	}
	publicPlan, _ := json.Marshal(plan)
	for _, secret := range []string{"credential-value", "secret", "private"} {
		if strings.Contains(string(publicPlan), secret) {
			t.Fatalf("plan leaked %q: %s", secret, publicPlan)
		}
	}

	applyResponse := s.process(Request{Op: "apply", Payload: requestPayload(t, map[string]any{"plan": plan})})
	if !applyResponse.OK {
		t.Fatalf("apply=%+v", applyResponse)
	}
	change := responseChange(t, applyResponse)
	if change.Status != "applied" || change.Backup == "" || change.Mode != originalMode {
		t.Fatalf("change=%+v", change)
	}
	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(updated), "http://127.0.0.1:8080/r/default/v1") {
		t.Fatalf("updated config=%s", updated)
	}
	if !strings.Contains(string(updated), "9007199254740993") {
		t.Fatalf("large integer changed in update: %s", updated)
	}
	if mode, err := os.Stat(path); err != nil || uint32(mode.Mode().Perm()) != originalMode {
		t.Fatalf("mode=%v err=%v", mode, err)
	}
	forgedRollback := change
	forgedRollback.OldSHA256 = "forged"
	if response := s.process(Request{Op: "rollback", Payload: requestPayload(t, map[string]any{"change": forgedRollback})}); response.OK || response.Error != "invalid_change" {
		t.Fatalf("forged rollback response=%+v", response)
	}

	rollbackResponse := s.process(Request{Op: "rollback", Payload: requestPayload(t, map[string]any{"change": change})})
	if !rollbackResponse.OK || responseChange(t, rollbackResponse).Status != "rolled_back" {
		t.Fatalf("rollback=%+v", rollbackResponse)
	}
	restored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != original {
		t.Fatalf("restored=%q", restored)
	}

	secondPlanResponse := s.process(Request{Op: "plan", Payload: requestPayload(t, map[string]any{"config_path": path, "proxy_url": "https://proxy.example/v1"})})
	secondPlan := responsePlan(t, secondPlanResponse)
	if err := os.WriteFile(path, []byte(`{"base_url":"https://changed.example"}`), 0640); err != nil {
		t.Fatal(err)
	}
	rejected := s.process(Request{Op: "apply", Payload: requestPayload(t, map[string]any{"plan": secondPlan})})
	if rejected.OK || rejected.Error != "external_modification" {
		t.Fatalf("external modification response=%+v", rejected)
	}
}

func TestHostAgentUnixSocketAndSocketSafety(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "client.json")
	if err := os.WriteFile(path, []byte(`{"base_url":"https://api.example.com"}`), 0600); err != nil {
		t.Fatal(err)
	}
	socketDir, err := os.MkdirTemp("", "ha-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socket := filepath.Join(socketDir, "agent.sock")
	server := &Server{Socket: socket, ConfigPath: path, StatePath: filepath.Join(dir, "history.json")}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe(ctx) }()
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		if _, err := os.Stat(socket); err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	resp, err := (Client{Path: socket, Timeout: time.Second}).Call(context.Background(), "discover", nil)
	if err != nil || !resp.OK || !resp.Available {
		t.Fatalf("discover over socket response=%+v err=%v", resp, err)
	}
	if mode, err := os.Stat(socket); err != nil || (runtime.GOOS != "windows" && mode.Mode().Perm() != 0600) {
		t.Fatalf("socket mode=%v err=%v", mode, err)
	}
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("host-agent did not stop")
	}

	occupied := filepath.Join(dir, "occupied.sock")
	if err := os.WriteFile(occupied, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := prepareSocketPath(occupied); err == nil {
		t.Fatal("occupied regular file accepted as socket")
	}
	kept, err := os.ReadFile(occupied)
	if err != nil || string(kept) != "keep" {
		t.Fatalf("occupied path changed: %q err=%v", kept, err)
	}
}

func TestReadFrameEnforcesBoundaries(t *testing.T) {
	if _, err := readFrame(bufio.NewReader(bytes.NewReader([]byte("{\"x\"}\n"))), 3); err == nil {
		t.Fatal("oversize frame accepted")
	}
	frame, err := readFrame(bufio.NewReader(bytes.NewReader([]byte("{}\n"))), 3)
	if err != nil || string(frame) != "{}" {
		t.Fatalf("frame=%q err=%v", frame, err)
	}
}

func TestPlanCapacityDoesNotLeaveMutexLocked(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "client.json")
	if err := os.WriteFile(path, []byte(`{"base_url":"https://api.example.com"}`), 0600); err != nil {
		t.Fatal(err)
	}
	s := &Server{ConfigPath: path, StatePath: filepath.Join(dir, "history.json")}
	body := requestPayload(t, map[string]any{"config_path": path, "proxy_url": "http://127.0.0.1:8080/r/default/v1"})
	for i := 0; i < MaxPlans; i++ {
		if p := responsePlan(t, s.process(Request{Op: "plan", Payload: body})); p.Status != "planned" {
			t.Fatal(p)
		}
	}
	p := responsePlan(t, s.process(Request{Op: "plan", Payload: body}))
	if p.Reason != "too_many_pending_plans" {
		t.Fatal(p)
	}
	done := make(chan struct{})
	go func() { s.process(Request{Op: "history"}); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("capacity path left mutex locked")
	}
	s.mu.Lock()
	for id := range s.plans {
		delete(s.plans, id)
		break
	}
	s.mu.Unlock()
	if p := responsePlan(t, s.process(Request{Op: "plan", Payload: body})); p.Status != "planned" {
		t.Fatal("capacity did not recover")
	}
}
