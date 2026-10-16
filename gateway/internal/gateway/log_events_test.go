package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/silencer/gateway/internal/storage"
)

func newLogTestServer(t *testing.T) (*Server, *storage.Storage) {
	t.Helper()
	t.Setenv("SILENCER_STORAGE_MODE", "sqlite")
	t.Setenv("SILENCER_SQLITE_PATH", filepath.Join(t.TempDir(), "logs.db"))
	store, err := storage.OpenFromEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	s, err := NewServer(Config{MaxConcurrency: 2, Engine: "go-fallback"}, testEngine{})
	if err != nil {
		t.Fatal(err)
	}
	s.SetStorage(store)
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	return s, store
}

func readLogJSON(t *testing.T, s *Server, path string) map[string]any {
	t.Helper()
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("%s: status=%d body=%s", path, rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func appendLogFixture(t *testing.T, store *storage.Storage, event storage.Event) {
	t.Helper()
	if err := store.AppendEvent(context.Background(), event); err != nil {
		t.Fatal(err)
	}
}

func TestLogOutcomeFiltersAndNoiseAgree(t *testing.T) {
	s, store := newLogTestServer(t)
	for i, tc := range []struct{ name, status, failure, want string }{
		{"passed", "200", "", "PASS"}, {"failed", "503", "", "ERR"},
		{"canceled", "499", "canceled", "CANCEL"}, {"dns", "error", "dns_error", "DNS_ERROR"},
	} {
		at := time.Now().UTC().Add(time.Duration(i) * time.Second)
		appendLogFixture(t, store, storage.Event{ID: tc.name + "-mask", RequestID: tc.name, Type: "MASK", Status: tc.status, Error: tc.failure, Meta: `{"count":0}`, CreatedAt: at})
		// Legacy RESTORE rows did not preserve their outcome.
		appendLogFixture(t, store, storage.Event{ID: tc.name + "-restore", RequestID: tc.name, Type: "RESTORE", Status: "no_sensitive_data", Meta: `{"restored":0}`, CreatedAt: at.Add(time.Millisecond)})
		body := readLogJSON(t, s, "/api/logs?latest=1&type="+tc.want)
		events := body["events"].([]any)
		if len(events) != 2 {
			t.Fatalf("%s: events=%v", tc.name, events)
		}
		for _, raw := range events {
			if raw.(map[string]any)["display_type"] != tc.want {
				t.Fatalf("%s: event=%v", tc.name, raw)
			}
		}
		detail := readLogJSON(t, s, "/api/logs/detail?id="+tc.name+"-restore")
		if detail["event"].(map[string]any)["display_type"] != tc.want {
			t.Fatalf("%s detail disagrees with list: %v", tc.name, detail)
		}
	}
	passed := readLogJSON(t, s, "/api/logs?type=PASS_COMBINED")
	if len(passed["events"].([]any)) != 2 {
		t.Fatalf("passed=%v", passed)
	}
	noiseHidden := readLogJSON(t, s, "/api/logs?hide_noise=1")
	if len(noiseHidden["events"].([]any)) != 6 {
		t.Fatalf("hidden=%v", noiseHidden)
	}
	for _, raw := range noiseHidden["events"].([]any) {
		if raw.(map[string]any)["display_type"] == "PASS" {
			t.Fatal("PASS survived noise filter")
		}
	}
}

func TestLogSnapshotsPreserveTimestampTiesLateWritesAndClear(t *testing.T) {
	s, store := newLogTestServer(t)
	at := time.Now().UTC().Truncate(time.Second)
	for _, id := range []string{"123", "other"} {
		appendLogFixture(t, store, storage.Event{ID: id, Type: "MASK", Meta: `{"count":1}`, CreatedAt: at})
	}
	body := readLogJSON(t, s, "/api/logs?limit=1")
	if len(body["events"].([]any)) != 2 || body["has_more"] != false {
		t.Fatalf("tie boundary: %v", body)
	}
	detail := readLogJSON(t, s, "/api/logs/detail?id=123")
	if detail["event"].(map[string]any)["detail_id"] != "123" {
		t.Fatalf("numeric stable ID: %v", detail)
	}
	latest := readLogJSON(t, s, "/api/logs?latest=1&limit=1")
	if latest["events"].([]any)[0].(map[string]any)["detail_id"] != "other" {
		t.Fatalf("latest: %v", latest)
	}
	appendLogFixture(t, store, storage.Event{ID: "late", Type: "MASK", Meta: `{"count":1}`, CreatedAt: at.Add(-time.Minute)})
	if len(readLogJSON(t, s, "/api/logs?latest=1")["events"].([]any)) != 3 {
		t.Fatal("late persisted event was lost")
	}
	if err := store.DeleteEvents(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(readLogJSON(t, s, "/api/logs?latest=1")["events"].([]any)) != 0 {
		t.Fatal("clear left stale snapshot rows")
	}
}

func TestLogPhaseQueryKeepsPairAndLegacyIngress(t *testing.T) {
	s, store := newLogTestServer(t)
	at := time.Now().UTC()
	appendLogFixture(t, store, storage.Event{ID: "mask", Type: "MASK", Meta: `{"sid":"legacy-request","count":2,"path":"/chat"}`, CreatedAt: at})
	appendLogFixture(t, store, storage.Event{ID: "restore", Type: "RESTORE", Status: "no_sensitive_data", Meta: `{"sid":"legacy-request","restored":0}`, CreatedAt: at.Add(time.Second)})
	body := readLogJSON(t, s, "/api/logs?latest=1&chains=1&type=MASK&q=chat&ingress=proxy")
	if len(body["events"].([]any)) != 2 {
		t.Fatalf("phase pair or legacy ingress lost: %v", body)
	}
	for _, path := range []string{"/api/logs?latest=1&chains=1&type=MASK&limit=1", "/api/logs?chains=1&type=MASK&limit=1"} {
		boundary := readLogJSON(t, s, path)
		if len(boundary["events"].([]any)) != 2 || boundary["has_more"] != false {
			t.Fatalf("boundary split pair: %v", boundary)
		}
	}
	for _, raw := range body["events"].([]any) {
		if raw.(map[string]any)["detail_id"] == "restore" && raw.(map[string]any)["status"] != "no_placeholder_in_response" {
			t.Fatalf("restore evidence not inherited: %v", raw)
		}
	}
}

func TestLogOperationalEventsKeepCountsAndFailureForEmptyBodies(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		err  error
		want string
	}{
		{"empty-success", 200, nil, "MASK"}, {"upstream-http-error", 503, nil, "ERR"},
		{"cancel", 499, context.Canceled, "CANCEL"}, {"dns", 502, &net.OpError{Err: &net.DNSError{Err: "not found", Name: "example.invalid"}}, "DNS_ERROR"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, store := newLogTestServer(t)
			item := storage.Request{ID: tc.name, Status: "running", StartedAt: time.Now().UTC().Add(-time.Second), CreatedAt: time.Now().UTC(), Meta: `{"path":"/chat"}`}
			if err := store.AppendRequest(context.Background(), item); err != nil {
				t.Fatal(err)
			}
			record := &requestRecord{store: store, item: item, maskedTokenCount: 3}
			record.finish(tc.code, tc.err)
			events, err := store.ListEvents(context.Background(), item.ID, 10)
			if err != nil || len(events) != 2 {
				t.Fatalf("events=%v err=%v", events, err)
			}
			for _, event := range events {
				meta := eventMeta(event)
				if meta["mask_count"] != float64(3) || meta["http_status"] != float64(tc.code) || meta["total_ms"].(float64) <= 0 {
					t.Fatalf("lost metadata: %v", meta)
				}
				if event.Type == "MASK" && maskEventItemCount(event) != 3 {
					t.Fatalf("preview count replaced exact count: %v", event)
				}
				if tc.err != nil && event.Error == "" {
					t.Fatal("event lost failure category")
				}
				if tc.want != "MASK" && displayLogEventType(event, nil) != tc.want {
					t.Fatalf("wrong outcome: %v", event)
				}
			}
		})
	}
}

func TestLogFulltextSearchesRetainedBodiesBeyondPreview(t *testing.T) {
	s, store := newLogTestServer(t)
	id := strings.Repeat("a", 32)
	if err := store.AppendRequest(context.Background(), storage.Request{ID: id, Meta: `{"body_store":true}`, Masked: []byte("short preview"), CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.logDirectory(), id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "raw"), []byte(strings.Repeat("x", 70000)+" BeyondPreview消息"), 0600); err != nil {
		t.Fatal(err)
	}
	appendLogFixture(t, store, storage.Event{ID: "body-event", RequestID: id, Type: "MASK", Meta: `{"count":1}`, CreatedAt: time.Now().UTC()})
	appendLogFixture(t, store, storage.Event{ID: "legacy-body-event", Type: "MASK", Meta: fmt.Sprintf(`{"sid":%q,"count":1}`, id), CreatedAt: time.Now().UTC()})
	if len(readLogJSON(t, s, "/api/logs?q=beyondpreview")["events"].([]any)) != 0 {
		t.Fatal("structured search unexpectedly scanned bodies")
	}
	if len(readLogJSON(t, s, "/api/logs?q=beyondpreview&fulltext=1")["events"].([]any)) != 2 {
		t.Fatal("fulltext missed retained body beyond preview")
	}
}

func TestLogCountUsesCompleteMappingBeforePlaceholderScan(t *testing.T) {
	_, store := newLogTestServer(t)
	at := time.Now().UTC()
	item := storage.Request{ID: "complete-map", Status: "200", StartedAt: at, EndedAt: at.Add(time.Second), Meta: `{"items_complete":true,"mask_token_count":3,"items":[{"label":"EMAIL","preview":"{{EMAIL_abc123}}"}]}`}
	record := &requestRecord{store: store}
	record.appendOperationalEvents(item)
	events, err := store.ListEvents(context.Background(), item.ID, 10)
	if err != nil || len(events) != 2 {
		t.Fatalf("events=%v err=%v", events, err)
	}
	for _, event := range events {
		if event.Type == "MASK" && maskEventItemCount(event) != 1 {
			t.Fatalf("preexisting placeholders inflated count: %v", event)
		}
	}
}

func TestLogExpiryRemovesRowsFromOrdinaryAndAuditFeeds(t *testing.T) {
	s, store := newLogTestServer(t)
	for _, event := range []storage.Event{
		{ID: "expired-mask", Type: "MASK", Meta: `{"count":1,"log_expired":true}`},
		{ID: "expired-audit", Type: "AUDIT_error_leak", Meta: `{"log_expired":true}`},
	} {
		appendLogFixture(t, store, event)
	}
	for _, path := range []string{"/api/logs", "/api/logs/export?include_audit=1", "/api/audit/events"} {
		if len(readLogJSON(t, s, path)["events"].([]any)) != 0 {
			t.Fatalf("expired row survived %s", path)
		}
	}
}

func TestLogExportFiltersNoiseAndIncludesMoreThanDefaultPage(t *testing.T) {
	s, store := newLogTestServer(t)
	for i := 0; i < 110; i++ {
		appendLogFixture(t, store, storage.Event{ID: fmt.Sprintf("passed-%03d", i), Type: "MASK", Status: "200", Meta: `{"count":0}`, CreatedAt: time.Now().UTC().Add(time.Duration(i) * time.Second)})
	}
	appendLogFixture(t, store, storage.Event{ID: "failed", Type: "MASK", Status: "503", Meta: `{"count":0}`, CreatedAt: time.Now().UTC()})
	appendLogFixture(t, store, storage.Event{ID: "audit", Type: "AUDIT_error_leak", Meta: `{"signal_type":"error_leak","severity":"LOW"}`, CreatedAt: time.Now().UTC()})
	if got := len(readLogJSON(t, s, "/api/logs/export?type=PASS_COMBINED")["events"].([]any)); got != 110 {
		t.Fatalf("export dropped filter or truncated at default page: %d", got)
	}
	if got := len(readLogJSON(t, s, "/api/logs/export?hide_noise=1&include_audit=1")["events"].([]any)); got != 2 {
		t.Fatalf("export noise/audit mismatch: %d", got)
	}
	if got := len(readLogJSON(t, s, "/api/logs?limit=5000")["events"].([]any)); got != 111 {
		t.Fatalf("audit event appeared twice through ordinary feed: %d", got)
	}
}

func TestLogWindowLimitIsExplicitAndLatestSnapshotStartsAtNewest(t *testing.T) {
	s, store := newLogTestServer(t)
	at := time.Now().UTC().Add(-time.Minute)
	for i := 0; i <= maxLogQueryEvents; i++ {
		appendLogFixture(t, store, storage.Event{ID: fmt.Sprintf("window-%05d", i), Type: "MASK", Status: "200", Meta: `{"count":0}`, CreatedAt: at.Add(time.Duration(i) * time.Microsecond)})
	}
	body := readLogJSON(t, s, "/api/logs?latest=1&limit=2")
	events := body["events"].([]any)
	if len(events) != 2 || events[1].(map[string]any)["detail_id"] != "window-05000" || body["window_limited"] != true || body["has_more"] != true {
		t.Fatalf("snapshot window contract: %v", body)
	}
	exported := readLogJSON(t, s, "/api/logs/export")
	if len(exported["events"].([]any)) != maxLogQueryEvents || exported["window_limited"] != true {
		t.Fatal("export silently claimed all history")
	}
}
