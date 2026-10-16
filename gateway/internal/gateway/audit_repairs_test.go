package gateway

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/silencer/gateway/internal/storage"
)

func TestConfigRejectedCandidateKeepsRuntimeAndDisk(t *testing.T) {
	s, _ := extensionLedgerServer(t, testEngine{})
	s.cfg.UpstreamURL = "http://127.0.0.1:1"
	before := s.cfg
	for _, body := range []string{`{"upstream_url":"http://127.0.0.1:2","max_response_bytes":0}`, `{"max_concurrency":0.5}`, `{"max_concurrency":"2"}`, `{"max_concurrency":1e99}`, `{"max_response_bytes":1.5}`, `{"upstream_url":"file:///tmp/example"}`} {
		w := httptest.NewRecorder()
		s.handleConfig(w, httptest.NewRequest("POST", "/api/config", strings.NewReader(body)))
		if w.Code != 400 || s.cfg != before || cap(s.limiter) != before.MaxConcurrency {
			t.Fatalf("rejected candidate mutated runtime: status=%d", w.Code)
		}
	}
	runtimePath := filepath.Join(t.TempDir(), "runtime.json")
	old := []byte(`{"old":"runtime"}`)
	os.WriteFile(runtimePath, old, 0600)
	t.Setenv("SILENCER_CONFIG_FILE", runtimePath)
	s.configPath = filepath.Join(t.TempDir(), "missing", "control.json")
	w := httptest.NewRecorder()
	s.handleConfig(w, httptest.NewRequest("POST", "/api/config", strings.NewReader(`{"max_concurrency":2,"origin_check":false}`)))
	disk, _ := os.ReadFile(runtimePath)
	if w.Code == 200 || s.cfg != before || s.control["origin_check"] != true || !bytes.Equal(old, disk) {
		t.Fatal("persistence failure partially installed candidate")
	}
}
func TestConfigCandidateListDoesNotMutateOriginal(t *testing.T) {
	original := map[string]any{"words": []any{"Alice", "Bob"}}
	candidate := cloneMap(original)
	applyConfigPatch(candidate, []string{"words"}, "list_remove", []any{"Alice"}, "")
	if original["words"].([]any)[0] != "Alice" {
		t.Fatal("candidate mutation leaked to old list")
	}
}
func TestExtensionReleaseGateCannotBeReenabled(t *testing.T) {
	t.Setenv("SILENCER_EXTENSION_DISABLED", "1")
	s, _ := extensionLedgerServer(t, testEngine{})
	for _, path := range []string{"/api/ext/ping", "/api/ext/mask", "/api/ext/restore", "/api/ext/token/rotate", "/downloads/silencer-extension.zip"} {
		code, _ := extensionLedgerCall(t, s, "POST", path, map[string]any{"text": "private"})
		if code != 404 {
			t.Fatalf("disabled endpoint %s returned %d", path, code)
		}
	}
	for _, v := range []struct {
		path string
		body any
	}{
		{"/api/config", map[string]any{"ext_bridge_enabled": true}},
		{"/api/config/patch", map[string]any{"key": "ext_bridge_enabled", "op": "set", "value": true}},
	} {
		code, _ := extensionLedgerCall(t, s, "POST", v.path, v.body)
		if code != 200 || s.control["ext_bridge_enabled"] != false {
			t.Fatal("API reenabled extension")
		}
	}
}
func TestExtensionDefaultRequiresManagementCredential(t *testing.T) {
	s, _ := extensionLedgerServer(t, testEngine{})
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("POST", "/api/ext/mask", strings.NewReader(`{"text":"private"}`)))
	if w.Code != 403 {
		t.Fatalf("anonymous extension access status=%d", w.Code)
	}
}
func TestDuplicateCategoryCreateRetainsExistingWords(t *testing.T) {
	s, _ := extensionLedgerServer(t, testEngine{})
	s.control["sensitive"] = map[string]any{"Customer": []any{"Alice", "Bob"}}
	code, _ := extensionLedgerCall(t, s, "POST", "/api/config/patch", map[string]any{"key": "sensitive", "op": "create_if_absent", "path": []string{"Customer"}, "value": []any{}})
	if code != 409 || len(s.control["sensitive"].(map[string]any)["Customer"].([]any)) != 2 {
		t.Fatal("duplicate creation cleared words")
	}
}
func TestProtocolLineEndingsEmitWithoutEOF(t *testing.T) {
	for _, ending := range []string{"\n", "\r\n", "\r"} {
		wire := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}" + ending + ending
		for split := 1; split <= len(wire); split++ {
			s := newProtocolRestoreSession("sse", func(context.Context) (RestoreSession, error) { return &repairIdentityRestore{}, nil })
			a, e := s.Push(context.Background(), []byte(wire[:split]))
			if e != nil {
				t.Fatal(e)
			}
			b, e := s.Push(context.Background(), []byte(wire[split:]))
			if e != nil {
				t.Fatal(e)
			}
			if !bytes.Contains(append(a, b...), []byte("hello")) {
				t.Fatalf("complete event buffered ending=%q split=%d", ending, split)
			}
		}
	}
}

type repairIdentityRestore struct{}

func (*repairIdentityRestore) Push(_ context.Context, b []byte) ([]byte, error) { return b, nil }
func (*repairIdentityRestore) Finish(context.Context) ([]byte, error)           { return nil, nil }
func TestFailedInitialCaptureAndOrphansAreCollected(t *testing.T) {
	s, store := extensionLedgerServer(t, contextualTestEngine{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.newExtensionRecord(ctx, "private-original", "test.invalid", "https://test.invalid/path", "POST"); err == nil {
		t.Fatal("canceled insert succeeded")
	}
	entries, _ := os.ReadDir(s.logDirectory())
	for _, e := range entries {
		if e.IsDir() {
			t.Fatal("failed insert left plaintext capture")
		}
	}
	abandoned := newBodyCapture(s.logDirectory(), newRequestID())
	abandoned.append("raw", []byte("private"))
	abandoned.close()
	abandoned.release()
	active := newBodyCapture(s.logDirectory(), newRequestID())
	defer active.discard()
	id := newRequestID()
	existing := newBodyCapture(s.logDirectory(), id)
	existing.close()
	existing.release()
	if err := store.AppendRequest(context.Background(), storage.Request{ID: id, Status: "200", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	collision := newBodyCapture(s.logDirectory(), id)
	collision.discard()
	s.sweepLogsWithDays(context.Background(), 0)
	if _, err := os.Stat(abandoned.dir); !os.IsNotExist(err) {
		t.Fatal("unreferenced body retained")
	}
	for _, dir := range []string{active.dir, existing.dir} {
		if _, err := os.Stat(dir); err != nil {
			t.Fatal("live/referenced/collision capture removed", err)
		}
	}
}
func TestExpiryClearsLegacyPayloadAndPreservesExpiredCounters(t *testing.T) {
	s, store := extensionLedgerServer(t, testEngine{})
	old := time.Now().Add(-5 * 24 * time.Hour)
	for _, expired := range []bool{false, true} {
		id := newRequestID()
		meta := `{"count":7,"mask_summary":{"EMAIL":7}}`
		if expired {
			meta = `{"log_expired":true,"count":7,"mask_summary":{"EMAIL":7},"restore_count":4}`
		}
		if err := store.AppendRequest(context.Background(), storage.Request{ID: id, Payload: []byte("legacy-plaintext"), Raw: []byte("legacy"), Status: "200", CreatedAt: old, Meta: meta}); err != nil {
			t.Fatal(err)
		}
		eventID := newRequestID()
		if err := store.AppendEvent(context.Background(), storage.Event{ID: eventID, Payload: []byte("audit-evidence"), Type: "MASK", CreatedAt: old, Meta: meta}); err != nil {
			t.Fatal(err)
		}
		s.sweepLogsWithDays(context.Background(), 3)
		row, _ := store.GetRequest(context.Background(), id)
		if len(row.Payload)+len(row.Raw) != 0 {
			t.Fatal("expired payload retained")
		}
		if expired && row.Meta != meta {
			t.Fatal("repair rewrote existing accounting")
		}
		events, _ := store.ListEvents(context.Background(), "", 100)
		for _, e := range events {
			if e.ID == eventID && len(e.Payload) != 0 {
				t.Fatal("audit evidence retained")
			}
		}
	}
}
func TestStatsRangeAndFailureContract(t *testing.T) {
	s, store := extensionLedgerServer(t, testEngine{})
	old := time.Now().Add(-48 * time.Hour)
	for _, status := range []string{"200", "401", "500", "499", "error"} {
		if err := store.AppendRequest(context.Background(), storage.Request{ID: newRequestID(), Status: status, CreatedAt: old}); err != nil {
			t.Fatal(err)
		}
	}
	for _, v := range []string{"7", "7d", "30", "30d", "90", "90d"} {
		code, stats := extensionLedgerCall(t, s, "GET", "/api/stats/today?range="+v, nil)
		if code != 200 || stats["requests"] != float64(5) {
			t.Fatalf("range %s ignored", v)
		}
	}
	code, _ := extensionLedgerCall(t, s, "GET", "/api/stats/today?range=invalid", nil)
	if code != 400 {
		t.Fatal("invalid range accepted")
	}
	_, models := extensionLedgerCall(t, s, "GET", "/api/stats/models?days=7", nil)
	row := models["models"].([]any)[0].(map[string]any)
	if row["requests"] != float64(5) || row["errors"] != float64(4) {
		t.Fatal("failure counts incorrect", row)
	}
}

func TestConfigFileCommitRecoversFirstRenameOnFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runtime.json")
	original := []byte("{ \"original\": true }\n")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	first, err := prepareConfigFile(path, map[string]any{"changed": true})
	if err != nil {
		t.Fatal(err)
	}
	// A directory replacing the second target after preparation causes rename
	// failure; the first file must be restored using its exact original bytes.
	second, err := prepareConfigFile(filepath.Join(dir, "control.json"), map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(second.path, 0700); err != nil {
		t.Fatal(err)
	}
	if err = persistConfigCandidate(first, second); err == nil {
		t.Fatal("rename failure ignored")
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, original) {
		t.Fatal("first configuration file not restored")
	}
}

func TestConfigPathAliasesAndInvalidRestoreLeaveDiskUnchanged(t *testing.T) {
	s, _ := extensionLedgerServer(t, testEngine{})
	dir := t.TempDir()
	path := filepath.Join(dir, "runtime.json")
	os.WriteFile(path, []byte(`{"original":true}`), 0600)
	workingDir, _ := os.Getwd()
	relative, err := filepath.Rel(workingDir, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SILENCER_CONFIG_FILE", relative)
	s.configPath = path
	s.configMu.Lock()
	err = s.commitConfigLocked(s.cfg, cloneMap(s.control), false)
	s.configMu.Unlock()
	if err == nil {
		t.Fatal("relative/absolute alias accepted")
	}
	t.Setenv("SILENCER_CONFIG_FILE", "")
	s.configPath = filepath.Join(dir, "control.json")
	os.WriteFile(s.configPath, []byte(`{}`), 0600)
	before, _ := os.ReadDir(dir)
	code, _ := extensionLedgerCall(t, s, "POST", "/api/config/restore", map[string]any{"config": map[string]any{"max_concurrency": 0}})
	after, _ := os.ReadDir(dir)
	if code == 200 || len(before) != len(after) {
		t.Fatal("invalid restore created durable backup")
	}
}
