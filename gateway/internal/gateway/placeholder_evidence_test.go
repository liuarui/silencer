package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/silencer/gateway/internal/storage"
)

func TestCanonicalPlaceholderFormatsAndExamples(t *testing.T) {
	for _, token := range []string{"{{IP_PUBLIC_0123456789abcdef}}", "{{EMAIL_abc123}}", "{{TERM_bcdfgh}}", "{{email_BCDFGH}}"} {
		if !isCanonicalPlaceholder(token) {
			t.Fatalf("native token rejected: %q", token)
		}
	}
	for _, token := range []string{"{{LABEL_TEMPLATE}}", "{{TERM_token}}", "{{IP_PUBLIC_TEST}}", "{{EMAIL_0}}", "{{TERM_example}}", "{{TERM_placeholder}}", "{{LONGER_THAN_12_0123456789abcdef}}", "{{TERM_0123456789abcdefa}}"} {
		if maskedTokenCount([]byte(token)) != 0 {
			t.Fatalf("template counted as an identity: %q", token)
		}
		var tail string
		for _, char := range token {
			if observeMaskedTokenCount(&tail, []byte(string(char))) != 0 {
				t.Fatalf("split template counted as an identity: %q", token)
			}
		}
	}
}

func TestHistoricalTemplateEvidenceProjectionIsConsistent(t *testing.T) {
	s, store := newLogTestServer(t)
	now := time.Now().UTC().Add(-time.Minute)
	for index, realRemaining := range []int{0, 1} {
		id := fmt.Sprintf("%032x", index+1)
		meta := map[string]any{
			"mask_token_count": 1, "mask_count": 1, "items_complete": true,
			"restore_count": 2, "restore_restored": 1 - realRemaining, "restore_unresolved": 1 + realRemaining,
			"count": 2, "restored": 1 - realRemaining, "unresolved": 1 + realRemaining, "restore_status": "unresolved",
			"items":                     []any{map[string]any{"label": "EMAIL", "preview": "{{EMAIL_0123456789abcdef}}", "original": "synthetic@example.test"}},
			"restore_evidence_complete": true, "restore_evidence": map[string]any{
				"{{EMAIL_0123456789abcdef}}": map[string]any{"observed": 1, "remaining": realRemaining},
				"{{LABEL_TEMPLATE}}":         map[string]any{"observed": 1, "remaining": 1},
			},
		}
		raw, _ := json.Marshal(meta)
		if err := store.AppendRequest(context.Background(), storage.Request{ID: id, Meta: string(raw), Status: "200", CreatedAt: now, StartedAt: now, EndedAt: now}); err != nil {
			t.Fatal(err)
		}
		if err := store.AppendEvent(context.Background(), storage.Event{ID: "restore-" + id, RequestID: id, Type: "RESTORE", Status: "unresolved", Meta: string(raw), CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
		view := readLogJSON(t, s, "/api/requests/"+id)["request"].(map[string]any)
		if view["unresolved_count"] != float64(realRemaining) || view["restored_count"] != float64(1-realRemaining) || view["abnormal"] != (realRemaining > 0) {
			t.Fatalf("summary inconsistent: %+v", view)
		}
		items := restoreDetailItems(meta["items"], meta)
		for _, item := range items {
			if item["preview"] == "{{LABEL_TEMPLATE}}" {
				t.Fatal("template persisted as unmapped evidence")
			}
		}
		if meta["restore_unresolved"] != 1+realRemaining {
			t.Fatal("historical metadata was mutated")
		}
	}
	stats := readLogJSON(t, s, "/api/stats/today")
	if stats["alerts"] != float64(1) || stats["restore_failed"] != float64(1) || stats["restored"] != float64(1) {
		t.Fatalf("dashboard includes template failures: %+v", stats)
	}
	rows := readLogJSON(t, s, "/api/requests?outcome=restore_failed")["items"].([]any)
	if len(rows) != 1 {
		t.Fatalf("filtered ledger includes template failure: %+v", rows)
	}
	events := readLogJSON(t, s, "/api/logs?latest=1")["events"].([]any)
	for _, row := range events {
		event := row.(map[string]any)
		if event["request_id"] == fmt.Sprintf("%032x", 1) && (event["status"] != "restored" || event["unresolved"] != float64(0)) {
			t.Fatalf("log differs from ledger: %+v", event)
		}
	}
}

func TestIncompleteTemplateEvidenceCannotProveRestoration(t *testing.T) {
	meta := map[string]any{"restore_evidence_complete": false, "restore_unresolved": 1, "restore_evidence": map[string]any{"{{LABEL_TEMPLATE}}": map[string]any{"observed": 1, "remaining": 1}}}
	if canonicalRestoreEvidence(meta)["restore_unresolved"] != 1 {
		t.Fatal("incomplete response classified as successful")
	}
	inner := newProtocolRestoreSession("sse", func(context.Context) (RestoreSession, error) { return testRestore{}, nil })
	text := "{{LABEL_TEMPLATE}} {{TERM_token}}"
	frame, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": text}}}})
	output, err := inner.Push(context.Background(), append(append([]byte("data: "), frame...), '\n', '\n'))
	if err != nil {
		t.Fatal(err)
	}
	tail, err := inner.Finish(context.Background())
	if err != nil || !strings.Contains(string(append(output, tail...)), text) || inner.RestoreCounts().Total != 0 {
		t.Fatal("ordinary template changed or counted", err, inner.RestoreCounts())
	}
}

func TestHistoricalTemplateAliasesWithoutRestoreEvent(t *testing.T) {
	meta := map[string]any{
		"restore_evidence_complete": true, "restore_unresolved": 1, "unresolved": 1, "restored": 0, "count": 42,
		"restore_evidence": map[string]any{"{{LABEL_TEMPLATE}}": map[string]any{"observed": 1, "remaining": 1}},
	}
	projected := canonicalRestoreEvidence(meta)
	if projected["unresolved"] != 0 || projected["restored"] != 0 || projected["count"] != 42 || outcomeAnomaly("200", "", projected, nil) != "" {
		t.Fatalf("stale aliases still affect the outcome: %+v", projected)
	}
	if outcomeAnomaly("499", "canceled", projected, nil) != "error" {
		t.Fatal("independent cancellation was hidden")
	}
}
