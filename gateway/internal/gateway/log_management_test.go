package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/silencer/gateway/internal/storage"
)

func TestFullLedgerKeepsAllStagesBeyondEightMiB(t *testing.T) {
	t.Setenv("SILENCER_STORAGE_MODE", "sqlite")
	t.Setenv("SILENCER_SQLITE_PATH", filepath.Join(t.TempDir(), "full.db"))
	store, err := storage.OpenFromEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s, err := NewServer(Config{MaxConcurrency: 1}, testEngine{})
	if err != nil {
		t.Fatal(err)
	}
	s.store = store
	if !s.recordDetailDataEnabled() || s.logRetentionDays() != 3 {
		t.Fatal("logging defaults incorrect")
	}
	id := newRequestID()
	item := storage.Request{ID: id, Payload: []byte{}, Status: "running", CreatedAt: time.Now().UTC(), Meta: `{"ingress":"proxy"}`}
	if err = store.AppendRequest(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	record := &requestRecord{store: store, item: item, maxBytes: 64 << 10, storeRawBody: true, capture: newBodyCapture(s.logDirectory(), id), maskedItemsComplete: true}
	body := append(bytes.Repeat([]byte("test-日本\n"), 900000), []byte("END-OF-FULL-BODY")...)
	if len(body) <= 8<<20 {
		t.Fatal("fixture does not exceed previous limit")
	}
	for start := 0; start < len(body); start += 8192 {
		end := start + 8192
		if end > len(body) {
			end = len(body)
		}
		chunk := body[start:end]
		record.appendRequest(false, chunk)
		record.appendRequest(true, chunk)
		record.appendResponse(false, chunk)
		record.appendResponse(true, chunk)
	}
	record.finish(200, nil)
	got, err := store.GetRequest(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if requestMeta(got)["body_store"] != true || requestMeta(got)["body_truncated"] == true {
		t.Fatalf("complete capture was not retained: capture=%v meta=%s", record.capture.err, got.Meta)
	}
	if len(got.Raw) > 64<<10 || len(got.Masked) > 64<<10 || len(got.Upstream) > 64<<10 || len(got.Restored) > 64<<10 {
		t.Fatal("accounting preview memory became unbounded")
	}
	view := map[string]any{}
	s.attachFullLedgerBodies(view, got)
	for _, stage := range ledgerStages {
		if text, ok := view[stage].(string); !ok || !bytes.Equal([]byte(text), body) {
			t.Fatalf("%s not retained completely", stage)
		}
	}
	if view["truncated"] != nil || view["body_unavailable"] != false {
		t.Fatal("full body falsely marked incomplete")
	}
	if err = os.Remove(filepath.Join(s.logDirectory(), id, "restored_response")); err != nil {
		t.Fatal(err)
	}
	view = map[string]any{}
	s.attachFullLedgerBodies(view, got)
	if view["body_storage_error"] != true || view["raw"] != nil {
		t.Fatal("missing body silently fell back to a prefix")
	}
}

func TestLogExpiryRemovesPlaintextAndKeepsAccounting(t *testing.T) {
	t.Setenv("SILENCER_STORAGE_MODE", "sqlite")
	t.Setenv("SILENCER_SQLITE_PATH", filepath.Join(t.TempDir(), "expiry.db"))
	store, err := storage.OpenFromEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s, err := NewServer(Config{MaxConcurrency: 1}, testEngine{})
	if err != nil {
		t.Fatal(err)
	}
	s.store = store
	oldID, newID, runningID := newRequestID(), newRequestID(), newRequestID()
	for _, row := range []storage.Request{
		{ID: oldID, Status: "200", CreatedAt: time.Now().Add(-4 * 24 * time.Hour), Raw: []byte("alice@example.test"), Masked: []byte("{{EMAIL_a}}"), Meta: `{"items_complete":true,"items":[{"label":"EMAIL","original":"alice@example.test","preview":"{{EMAIL_a}}"}],"restore_count":1,"restore_restored":1,"usage_found":true,"usage_prompt_tokens":19,"body_store":true}`},
		{ID: newID, Status: "200", CreatedAt: time.Now().Add(-2 * 24 * time.Hour), Raw: []byte("recent-body")},
		{ID: runningID, Status: "running", CreatedAt: time.Now().Add(-5 * 24 * time.Hour), Raw: []byte("active-body")},
	} {
		if err = store.AppendRequest(context.Background(), row); err != nil {
			t.Fatal(err)
		}
	}
	activeCapture := newBodyCapture(s.logDirectory(), runningID)
	defer activeCapture.release()
	defer activeCapture.close()
	capture := newBodyCapture(s.logDirectory(), oldID)
	capture.append("raw", []byte("alice@example.test"))
	if !capture.close() {
		t.Fatal("fixture capture failed")
	}
	capture.release()
	if err = store.AppendEvent(context.Background(), storage.Event{ID: newRequestID(), Type: "MASK", CreatedAt: time.Now().Add(-4 * 24 * time.Hour), Meta: `{"ingress":"ext","count":2,"items":[{"label":"IP_PUBLIC","original":"198.52.100.11"},{"label":"IP_PUBLIC","original":"198.52.100.12"}]}`}); err != nil {
		t.Fatal(err)
	}
	s.sweepLogs(context.Background())
	old, err := store.GetRequest(context.Background(), oldID)
	if err != nil {
		t.Fatal(err)
	}
	if len(old.Raw)+len(old.Masked)+len(old.Upstream)+len(old.Restored) != 0 || strings.Contains(old.Meta, "alice") || requestMeta(old)["log_expired"] != true {
		t.Fatal("expired plaintext remains")
	}
	if _, err = os.Stat(filepath.Join(s.logDirectory(), oldID)); !os.IsNotExist(err) {
		t.Fatal("expired files remain")
	}
	for _, id := range []string{newID, runningID} {
		row, _ := store.GetRequest(context.Background(), id)
		if len(row.Raw) == 0 {
			t.Fatal("retained or active request expired")
		}
	}
	rr := httptest.NewRecorder()
	s.handleStatsToday(rr, httptest.NewRequest("GET", "/api/stats/today?range=30d", nil))
	var stats map[string]any
	if err = json.Unmarshal(rr.Body.Bytes(), &stats); err != nil {
		t.Fatal(err)
	}
	if stats["requests"] != float64(3) || stats["masked_items"] != float64(3) || stats["restored"] != float64(1) || stats["tokens"].(map[string]any)["prompt"] != float64(19) {
		t.Fatalf("expiry damaged accounting: %v", stats)
	}
	s.control["log_retention_days"] = 0
	if err = store.AppendRequest(context.Background(), storage.Request{ID: newRequestID(), Status: "200", CreatedAt: time.Now().Add(-10 * 24 * time.Hour), Raw: []byte("forever")}); err != nil {
		t.Fatal(err)
	}
	s.sweepLogs(context.Background())
	rows, _ := store.ListRequests(context.Background(), 100)
	found := false
	for _, row := range rows {
		if string(row.Raw) == "forever" {
			found = true
		}
	}
	if !found {
		t.Fatal("zero retention did not keep logs")
	}
}

func TestLogSettingsRejectInvalidPeriods(t *testing.T) {
	for _, value := range []any{-1, 1.5, "3", false, 36501} {
		if validateLogSettings(map[string]any{"log_retention_days": value}) == nil {
			t.Fatalf("accepted invalid retention %v", value)
		}
	}
	for _, value := range []any{0, 3, float64(7)} {
		if err := validateLogSettings(map[string]any{"log_retention_days": value}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRetentionSkipsLiveLeasesAndExpiresAbandonedRequests(t *testing.T) {
	t.Setenv("SILENCER_STORAGE_MODE", "sqlite")
	t.Setenv("SILENCER_SQLITE_PATH", filepath.Join(t.TempDir(), "leases.db"))
	store, err := storage.OpenFromEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s, err := NewServer(Config{MaxConcurrency: 1}, testEngine{})
	if err != nil {
		t.Fatal(err)
	}
	s.store = store
	old := time.Now().Add(-5 * 24 * time.Hour)
	var liveIDs []string
	for i := 0; i < 35; i++ {
		id := newRequestID()
		liveIDs = append(liveIDs, id)
		capture := newRequestCapture(s.logDirectory(), id, false)
		if capture.err != nil {
			t.Fatal(capture.err)
		}
		defer capture.release()
		status := "running"
		if i == 0 {
			status = "200"
		}
		if err = store.AppendRequest(context.Background(), storage.Request{ID: id, Status: status, CreatedAt: old, Raw: []byte("live")}); err != nil {
			t.Fatal(err)
		}
	}
	abandoned := newRequestID()
	capture := newRequestCapture(s.logDirectory(), abandoned, false)
	if capture.err != nil {
		t.Fatal(capture.err)
	}
	capture.release()
	if err = store.AppendRequest(context.Background(), storage.Request{ID: abandoned, Status: "running", CreatedAt: old.Add(time.Hour), Raw: []byte("abandoned")}); err != nil {
		t.Fatal(err)
	}
	s.sweepLogs(context.Background())
	for _, id := range liveIDs {
		row, e := store.GetRequest(context.Background(), id)
		if e != nil || string(row.Raw) != "live" {
			t.Fatal("live lease expired", e)
		}
	}
	row, err := store.GetRequest(context.Background(), abandoned)
	if err != nil || requestMeta(row)["log_expired"] != true || len(row.Raw) != 0 {
		t.Fatal("abandoned request behind active page did not expire", err)
	}
}

func TestPendingBodyCleanupSurvivesRestartAndPermanentRetention(t *testing.T) {
	t.Setenv("SILENCER_STORAGE_MODE", "sqlite")
	t.Setenv("SILENCER_SQLITE_PATH", filepath.Join(t.TempDir(), "pending.db"))
	store, err := storage.OpenFromEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewServer(Config{MaxConcurrency: 1}, testEngine{})
	if err != nil {
		t.Fatal(err)
	}
	s.store = store
	id := newRequestID()
	capture := newBodyCapture(s.logDirectory(), id)
	capture.append("raw", []byte("expired-private-body"))
	if !capture.close() {
		t.Fatal(capture.err)
	}
	capture.release()
	if err = store.AppendRequest(context.Background(), storage.Request{ID: id, Status: "200", CreatedAt: time.Now().Add(-4 * 24 * time.Hour), Meta: `{"log_expired":true,"body_cleanup_pending":true}`}); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = storage.OpenFromEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s.store = store
	s.control["log_retention_days"] = 0
	s.sweepLogs(context.Background())
	if _, err = os.Stat(capture.dir); !os.IsNotExist(err) {
		t.Fatal("pending private body not removed after restart")
	}
	pending, err := store.PendingBodyCleanup(context.Background(), 10)
	if err != nil || len(pending) != 0 {
		t.Fatal("cleanup not acknowledged", err)
	}
}

func TestNonStreamingProxyCapturesCompleteRequestWithSmallDatabasePreview(t *testing.T) {
	t.Setenv("SILENCER_STORAGE_MODE", "sqlite")
	t.Setenv("SILENCER_SQLITE_PATH", filepath.Join(t.TempDir(), "proxy-full.db"))
	store, err := storage.OpenFromEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"message":"ok"}`)
	}))
	defer upstream.Close()
	s, err := NewServer(Config{UpstreamURL: upstream.URL, MaxConcurrency: 1}, testEngine{})
	if err != nil {
		t.Fatal(err)
	}
	s.store = store
	body := []byte(`{"message":"` + strings.Repeat("ordinary text ", 12000) + `END"}`)
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, httptest.NewRequest("POST", "/api/requests/v1/chat/completions", bytes.NewReader(body)))
	if response.Code != 200 {
		t.Fatalf("proxy status=%d", response.Code)
	}
	rows, err := store.ListRequests(context.Background(), 10)
	if err != nil || len(rows) != 1 {
		t.Fatal(err)
	}
	row := rows[0]
	if len(row.Raw) > 64<<10 || len(row.Masked) > 64<<10 {
		t.Fatal("initial nonstream request preview too large")
	}
	view := map[string]any{}
	s.attachFullLedgerBodies(view, row)
	if view["raw"] != string(body) || view["truncated"] != nil {
		t.Fatal("full nonstream request not retained")
	}
}
