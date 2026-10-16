package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/silencer/gateway/internal/storage"
)

func TestConsoleCompactResponsesKeepLegacyAliases(t *testing.T) {
	s, store := newLogTestServer(t)
	now := time.Now().UTC()
	if err := store.AppendRequest(context.Background(), storage.Request{ID: "compact-request", Status: "200", CreatedAt: now, Meta: `{"items_complete":true,"restore_count":0}`}); err != nil {
		t.Fatal(err)
	}
	appendLogFixture(t, store, storage.Event{ID: "compact-mask", RequestID: "compact-request", Type: "MASK", CreatedAt: now, Meta: `{"count":0,"items_complete":true}`})
	for _, tc := range []struct{ path, alias, content string }{
		{"/api/logs?events_only=1", "list", "events"},
		{"/api/requests?items_only=1", "requests", "items"},
	} {
		compact := readLogJSON(t, s, tc.path)
		if _, exists := compact[tc.alias]; exists {
			t.Fatal("duplicate collection in compact response")
		}
		if len(compact[tc.content].([]any)) != 1 {
			t.Fatal("compact response lost row")
		}
	}
	for _, path := range []string{"/api/logs", "/api/requests"} {
		legacy := readLogJSON(t, s, path)
		alias, key := "list", "events"
		if path == "/api/requests" {
			alias, key = "requests", "items"
		}
		a, _ := json.Marshal(legacy[alias])
		b, _ := json.Marshal(legacy[key])
		if string(a) != string(b) {
			t.Fatal("legacy alias changed")
		}
	}
}

func TestPreparedMetadataKeepsHistoricalRepairAndIsolation(t *testing.T) {
	meta := `{"restore_evidence_complete":true,"restore_evidence":{"{{IP_PUBLIC_TEST}}":{"observed":1,"remaining":1}},"restore_unresolved":1,"restore_count":1,"restore_status":"unresolved"}`
	requests := []storage.Request{{Meta: meta}}
	events := []storage.Event{{Type: "RESTORE", Meta: meta}}
	prepareReadMetadata(requests, events)
	if !requests[0].StatsMetaCanonical || !events[0].StatsMetaCanonical || requestMeta(requests[0])["restore_unresolved"] != 0 || eventMeta(events[0])["unresolved"] != 0 {
		t.Fatal("historical repair lost in cached projection")
	}
	other := []storage.Event{{Type: "RESTORE", Meta: meta}}
	prepareReadMetadata(nil, other)
	projected := projectLogNoise(events[0], nil)
	projected.StatsMeta["marker"] = true
	if eventMeta(other[0])["marker"] != nil || eventMeta(events[0])["marker"] != nil {
		t.Fatal("projection shared mutable metadata")
	}
}

func TestRelatedLogDetailsKeepLegacyAssociation(t *testing.T) {
	for _, ids := range [][2]string{{"legacy-request", ""}, {"", "legacy-request"}} {
		s, store := newLogTestServer(t)
		now := time.Now().UTC()
		appendLogFixture(t, store, storage.Event{ID: "legacy-assoc-mask", RequestID: ids[0], Type: "MASK", Status: "503", Meta: `{"sid":"legacy-request","count":1}`, CreatedAt: now})
		appendLogFixture(t, store, storage.Event{ID: "legacy-assoc-restore", RequestID: ids[1], Type: "RESTORE", Status: "no_sensitive_data", Meta: `{"sid":"legacy-request","restored":0}`, CreatedAt: now.Add(time.Millisecond)})
		detail := readLogJSON(t, s, "/api/logs/detail?id=legacy-assoc-restore")["event"].(map[string]any)
		if detail["display_type"] != "ERR" || detail["status"] != "503" {
			t.Fatalf("lost legacy phase: %+v", detail)
		}
	}
}

func TestHistoryKeepsLegacyRequestRestorationWithoutEventStatus(t *testing.T) {
	s, store := newLogTestServer(t)
	now := time.Now().Add(-time.Minute)
	if err := store.AppendRequest(context.Background(), storage.Request{ID: "legacy-history", Status: "200", CreatedAt: now, Upstream: []byte("{{EMAIL_0123456789abcdef}}"), Restored: []byte("restored@example.test"), Meta: `{"items_complete":true,"mask_token_count":1}`}); err != nil {
		t.Fatal(err)
	}
	appendLogFixture(t, store, storage.Event{ID: "legacy-history-restore", RequestID: "legacy-history", Type: "RESTORE", Status: "no_sensitive_data", CreatedAt: now.Add(time.Second)})
	value := readLogJSON(t, s, "/api/stats/history?days=2&granularity=hour")
	var total float64
	for _, raw := range value["data"].([]any) {
		total += raw.(map[string]any)["restored"].(float64)
	}
	if total != 1 {
		t.Fatalf("legacy request restoration dropped: %v", total)
	}
}

func TestFullTextSearchAcrossBodyBatchesKeepsChains(t *testing.T) {
	s, store := newLogTestServer(t)
	now := time.Now()
	for i := 0; i < 35; i++ {
		id := fmt.Sprintf("%032x", i+100)
		body := "ordinary synthetic body"
		if i == 0 || i == 34 {
			body = "synthetic batch-needle only in ledger"
		}
		if err := store.AppendRequest(context.Background(), storage.Request{ID: id, Raw: []byte(body), CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
		for _, phase := range []string{"MASK", "RESTORE"} {
			appendLogFixture(t, store, storage.Event{ID: id + phase, RequestID: id, Type: phase, CreatedAt: now, Meta: `{"count":1}`})
		}
	}
	value := readLogJSON(t, s, "/api/logs?latest=1&chains=1&limit=100&fulltext=1&q=batch-needle")
	if len(value["events"].([]any)) != 4 {
		t.Fatalf("cross-batch matches lost: %v", value["total"])
	}
}

func TestReadProjectionsKeepSourceMetadataImmutable(t *testing.T) {
	meta := map[string]any{
		"restore_evidence_complete": true, "restore_unresolved": 1, "unresolved": 1,
		"restore_status": "unresolved", "mask_count": 1,
		"restore_evidence": map[string]any{"{{LABEL_TEMPLATE}}": map[string]any{"observed": 1, "remaining": 1}},
		"items":            []any{map[string]any{"label": "EMAIL", "original": "synthetic@example.test"}},
	}
	before, _ := json.Marshal(meta)
	event := storage.Event{ID: "immutable-restore", RequestID: "immutable", Type: "RESTORE", StatsMeta: meta, StatsMetaCanonical: true}
	canonicalRestoreEventMeta(meta, "RESTORE")
	status, failure, projected := withoutStaleRestoreFailure("unresolved", "restore_failed", meta)
	if status != "" || failure != "" || projected["restore_unresolved"] != nil {
		t.Fatal("stale result was not projected")
	}
	projected["marker"] = true
	projectLogNoise(event, nil)
	normalizeRestoreEvent(event, map[string]struct{}{"immutable": {}})
	normalizeLogOutcome(event, map[string]storage.Event{"immutable": {Status: "503", StatsMetaCanonical: true, StatsMeta: map[string]any{}}})
	after, _ := json.Marshal(meta)
	if string(before) != string(after) {
		t.Fatal("read projection changed source metadata")
	}
}

func BenchmarkReadProjectionMetadata(b *testing.B) {
	items := make([]any, 1000)
	for i := range items {
		items[i] = map[string]any{"label": "EMAIL", "original": "synthetic@example.test", "preview": "{{EMAIL_0123456789abcdef}}"}
	}
	meta := map[string]any{"restore_unresolved": 0, "restore_status": "restored", "items": items}
	b.Run("deep_copy_reference", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			copy := cloneMap(meta)
			delete(copy, "restore_unresolved")
			delete(copy, "restore_status")
		}
	})
	b.Run("read_projection", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			withoutStaleRestoreFailure("200", "", meta)
		}
	})
}
