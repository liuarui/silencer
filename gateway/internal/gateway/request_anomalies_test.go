package gateway

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/silencer/gateway/internal/storage"
)

func TestAnomalyTotalsCategoriesAndLedgerAgree(t *testing.T) {
	s, store := newLogTestServer(t)
	now := time.Now().UTC().Add(-time.Minute)
	requests := []storage.Request{
		{ID: "auth", Status: "401", Meta: `{"ingress":"proxy","http_status":401}`},
		{ID: "allowed", Status: "404", Meta: `{"ingress":"proxy","method":"GET","path":"/r/sub2api/api/tags"}`},
		{ID: "restore", Status: "error", Error: "upstream_or_engine_error", Meta: `{"restore_count":1,"restore_unresolved":1}`},
		{ID: "block", Status: "403"},
		{ID: "audit", Status: "200"},
		{ID: "cancel", Status: "499", Error: "canceled"},
		{ID: "dns", Status: "error", Error: "dns_error"},
	}
	ids := make(map[string]string)
	for i, item := range requests {
		ids[item.ID] = fmt.Sprintf("%032x", i+1)
		item.ID = ids[item.ID]
		item.CreatedAt, item.StartedAt, item.EndedAt = now, now, now.Add(time.Second)
		if err := store.AppendRequest(context.Background(), item); err != nil {
			t.Fatal(err)
		}
	}
	for i, event := range []storage.Event{
		{RequestID: "auth", Type: "MASK", Status: "401"},
		{RequestID: "auth", Type: "RESTORE", Status: "401"},
		{RequestID: "auth", Type: "ERR", Status: "401"},
		{RequestID: "block", Type: "BLOCK", Status: "403"},
		{RequestID: "audit", Type: "AUDIT_error_leak"},
		{RequestID: "audit", Type: "SCAN_WARN"},
		{RequestID: "restore", Type: "RESTORE", Status: "error", Meta: `{"restore_status":"unresolved","restore_unresolved":1}`},
	} {
		event.RequestID = ids[event.RequestID]
		event.ID, event.CreatedAt = fmt.Sprint(i), now
		appendLogFixture(t, store, event)
	}
	stats := readLogJSON(t, s, "/api/stats/today")
	counts := stats["alert_counts"].(map[string]any)
	for key, want := range map[string]float64{"block": 1, "restore_failed": 1, "error": 3, "response_warning": 1} {
		if counts[key] != want {
			t.Fatalf("%s=%v want %v", key, counts[key], want)
		}
	}
	if stats["alerts"] != float64(6) {
		t.Fatalf("duplicated or missing anomalies: %v", stats["alerts"])
	}
	ledger := readLogJSON(t, s, "/api/requests?outcome=abnormal")["items"].([]any)
	if len(ledger) != 6 {
		t.Fatalf("ledger and dashboard disagree: %v", ledger)
	}
	for _, raw := range ledger {
		row := raw.(map[string]any)
		if row["abnormal"] != true || row["anomaly"] == "" || row["request_id"] == ids["allowed"] {
			t.Fatalf("wrong anomaly: %v", row)
		}
		detail := readLogJSON(t, s, "/api/requests/"+row["request_id"].(string))["request"].(map[string]any)
		if detail["anomaly"] != row["anomaly"] {
			t.Fatalf("detail category differs: %v / %v", detail, row)
		}
	}
	history := readLogJSON(t, s, "/api/stats/history?days=1")["data"].([]any)
	if history[0].(map[string]any)["alerts"] != stats["alerts"] {
		t.Fatalf("history total differs: %v", history)
	}
}

func TestLedgerAnomalyFilterSearchesBeforeLimitAndUsesCurrentRules(t *testing.T) {
	s, store := newLogTestServer(t)
	now := time.Now().UTC().Add(-time.Minute)
	// Timestamp ties deliberately cross several storage pages.
	for i := 0; i < 450; i++ {
		if err := store.AppendRequest(context.Background(), storage.Request{ID: fmt.Sprintf("normal-%04d", i), Status: "200", CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	for _, item := range []storage.Request{
		{ID: "a-auth", Status: "401", Meta: `{"ingress":"proxy"}`},
		{ID: "a-ext", Status: "error", Error: "dns_error", Meta: `{"ingress":"ext"}`},
		{ID: "a-phase", Status: "200", Meta: `{"failure_stage":"downstream_write"}`},
		{ID: "a-restore", Status: "200", Meta: `{"restore_unresolved":1}`},
		{ID: "a-allowed", Status: "404", Meta: `{"ingress":"proxy","method":"GET","path":"/r/sub2api/api/tags"}`},
	} {
		item.CreatedAt = now
		if err := store.AppendRequest(context.Background(), item); err != nil {
			t.Fatal(err)
		}
	}
	all := readLogJSON(t, s, "/api/requests?limit=100")
	if len(all["items"].([]any)) != 100 || all["has_more"] != true {
		t.Fatal("unfiltered limit not applied")
	}
	filtered := readLogJSON(t, s, "/api/requests?outcome=abnormal")
	if len(filtered["items"].([]any)) != 4 {
		t.Fatalf("older anomalies lost behind normal rows: %v", filtered)
	}
	limited := readLogJSON(t, s, "/api/requests?outcome=abnormal&limit=2")
	if len(limited["items"].([]any)) != 2 || limited["has_more"] != true {
		t.Fatal("matching limit not applied")
	}
	proxy := readLogJSON(t, s, "/api/requests?outcome=abnormal&ingress=proxy")["items"].([]any)
	if len(proxy) != 3 {
		t.Fatalf("ingress filter after limit: %v", proxy)
	}
	s.configMu.Lock()
	s.control["log_noise_allowlist"] = []any{}
	s.configMu.Unlock()
	if got := len(readLogJSON(t, s, "/api/requests?outcome=abnormal")["items"].([]any)); got != 5 {
		t.Fatalf("current allowlist not used: %d", got)
	}
	for _, query := range []string{"outcome=bogus", "ingress=bogus", "limit=0", "limit=501", "limit=abc"} {
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/requests?"+query, nil))
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("invalid filter accepted: %s", query)
		}
	}
}

func TestAnomaliesUseLatestRestoreAndKeepStandaloneWarnings(t *testing.T) {
	now := time.Now().UTC()
	events := []storage.Event{
		{ID: "old", RequestID: "recovered", Type: "RESTORE", Status: "unresolved", CreatedAt: now},
		{ID: "new", RequestID: "recovered", Type: "RESTORE", Status: "restored", CreatedAt: now.Add(time.Second)},
		{ID: "standalone", Type: "AUDIT_error_leak", CreatedAt: now},
		{ID: "legacy", RequestID: "failed", Type: "RESTORE", Status: "error", Meta: `{"restore_status":"unresolved"}`, CreatedAt: now},
	}
	got := classifyRequestAnomalies([]storage.Request{{ID: "recovered", Status: "200", Meta: `{"restore_count":1,"restore_unresolved":1}`}}, events, nil)
	if len(got) != 2 || got["recovered"].kind != "" || got["standalone"].kind != "response_warning" || got["failed"].kind != "restore_failed" {
		t.Fatalf("wrong latest outcomes: %v", got)
	}
}

func TestLegacyUnresolvedLedgerMatchesDashboardAndDetail(t *testing.T) {
	s, store := newLogTestServer(t)
	id := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	token := []byte("{{EMAIL_abcdef0123456789}}")
	if err := store.AppendRequest(context.Background(), storage.Request{ID: id, Status: "200", Upstream: token, Restored: token, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	stats := readLogJSON(t, s, "/api/stats/today")
	rows := readLogJSON(t, s, "/api/requests?outcome=restore_failed")["items"].([]any)
	detail := readLogJSON(t, s, "/api/requests/"+id)["request"].(map[string]any)
	if stats["alerts"] != float64(1) || len(rows) != 1 || rows[0].(map[string]any)["anomaly"] != "restore_failed" || detail["anomaly"] != "restore_failed" {
		t.Fatalf("legacy outcome mismatch: stats=%v rows=%v detail=%v", stats, rows, detail)
	}
	if rows[0].(map[string]any)["upstream_response"] != nil || rows[0].(map[string]any)["restored_response"] != nil {
		t.Fatal("legacy bodies leaked into list")
	}
}
