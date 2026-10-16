package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/silencer/gateway/internal/storage"
)

func TestRequestLedgerCaptureIndependentOfCompareWindow(t *testing.T) {
	for _, size := range []int{64 << 10, 145000, 300000} {
		body := bytes.Repeat([]byte("x"), size)
		r := &requestRecord{maxBytes: 200000, storeRawBody: true}
		r.appendRequest(false, body)
		r.appendRequest(true, body)
		want := size
		if want > 200000 {
			want = 200000
		}
		if len(r.requestRaw) != want || len(r.requestMasked) != want {
			t.Fatalf("size=%d capture raw=%d masked=%d want=%d", size, len(r.requestRaw), len(r.requestMasked), want)
		}
		if r.requestRawTruncated != (size > 200000) || r.requestMaskedTruncated != (size > 200000) {
			t.Fatalf("size=%d retention truncation incorrect", size)
		}
		if len(r.maskCompareRaw) > 64<<10 || len(r.maskCompareMasked) > 64<<10 {
			t.Fatal("comparison memory limit exceeded")
		}
	}
}

func TestRestoreDetailRequiresIndividualEvidence(t *testing.T) {
	items := []any{}
	for i := 0; i < 9; i++ {
		items = append(items, map[string]any{
			"label": "EMAIL", "preview": string(rune('a' + i)), "original": "same original",
			"restore_status": "restored", "restored": true, "restored_count": 42, "unresolved_count": 7,
		})
	}
	meta := map[string]any{"restore_evidence_complete": true, "restore_evidence": map[string]any{
		"a":                              map[string]any{"observed": 2, "remaining": 0},
		"{{IP_PUBLIC_ffffffffffffffff}}": map[string]any{"observed": 1, "remaining": 1},
	}}
	got := restoreDetailItems(items, meta)
	if len(got) != 10 || got[0]["restore_status"] != "restored" || got[0]["restored_count"] != 2 {
		t.Fatalf("individual evidence=%v", got)
	}
	for _, item := range got[1:9] {
		if item["restored"] != false || item["restore_status"] != "not_mentioned" {
			t.Fatalf("unmentioned item=%v", item)
		}
	}
	if got[9]["restore_status"] != "unmapped" || got[9]["original"] != nil {
		t.Fatalf("unknown token=%v", got[9])
	}
	for _, complete := range []any{false, nil} {
		meta["restore_evidence_complete"] = complete
		for _, item := range restoreDetailItems(items, meta) {
			if item["restore_status"] != "unknown" || item["restored"] == true {
				t.Fatalf("incomplete evidence=%v", item)
			}
			if item["original"] != nil && (item["restored_count"] != nil || item["unresolved_count"] != nil) {
				t.Fatalf("legacy counts leaked into incomplete evidence=%v", item)
			}
		}
	}
	if items[0].(map[string]any)["restored_count"] != 42 {
		t.Fatal("request mapping mutated while projecting response evidence")
	}
}

func TestPlaceholderExamplesAreNotSensitiveTokens(t *testing.T) {
	text := []byte("{{IP_PUBLIC_...}} {{EMAIL_0}} {{EMAIL_abc123}} {{TERM_token}} {{IP_PUBLIC_90206bc5f642b5c0}}")
	if got := maskedTokenCount(text); got != 2 {
		t.Fatalf("count=%d", got)
	}
}

func TestRestoreDetailAttachesUnmappedEvidenceWithoutRequestItems(t *testing.T) {
	t.Setenv("SILENCER_STORAGE_MODE", "sqlite")
	t.Setenv("SILENCER_SQLITE_PATH", filepath.Join(t.TempDir(), "evidence.db"))
	store, err := storage.OpenFromEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.AppendRequest(context.Background(), storage.Request{
		ID: "unmapped-response", CreatedAt: time.Now().UTC(),
		Meta: `{"restore_evidence_complete":true,"restore_evidence":{"{{IP_PUBLIC_ffffffffffffffff}}":{"observed":1,"remaining":1}}}`,
	}); err != nil {
		t.Fatal(err)
	}
	s, err := NewServer(Config{MaxConcurrency: 1, Engine: "go-fallback"}, testEngine{})
	if err != nil {
		t.Fatal(err)
	}
	s.control["record_detail_data"] = true
	s.SetStorage(store)
	defer s.Shutdown(context.Background())
	view := map[string]any{"type": "RESTORE"}
	s.attachRequestDetail(view, "unmapped-response")
	items, ok := view["items"].([]map[string]any)
	if !ok || len(items) != 1 || items[0]["restore_status"] != "unmapped" {
		t.Fatalf("detail=%v", view)
	}
	labels, _ := view["matched_rules"].([]string)
	if len(labels) != 1 || labels[0] != "IP_PUBLIC" {
		t.Fatalf("labels=%v", labels)
	}
}

func TestDetailBodyPresentationTruncation(t *testing.T) {
	view := map[string]any{}
	attachLedgerBodies(view, storage.Request{Upstream: bytes.Repeat([]byte("x"), 100000)})
	if len(view["upstream_response"].(string)) != 100000 || view["truncated"] != nil {
		t.Fatal("ordinary response was silently cut")
	}
	attachLedgerBodies(view, storage.Request{Restored: bytes.Repeat([]byte("x"), (4<<20)+1)})
	if view["truncated"] != nil || len(view["restored_response"].(string)) != (4<<20)+1 {
		t.Fatal("complete stored response was truncated for display")
	}
	view = map[string]any{}
	attachLedgerBodies(view, storage.Request{Meta: `{"upstream_truncated":true}`})
	if view["truncated"] != 1 {
		t.Fatal("retention truncation not disclosed")
	}
}

func TestProtocolAbortCannotClaimBufferedTokenRestored(t *testing.T) {
	s := newProtocolRestoreSession("sse", func(context.Context) (RestoreSession, error) { return &evidenceBufferSession{}, nil })
	frame, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "{{EMAIL_abc123}}"}}}})
	if _, err := s.Push(context.Background(), append(append([]byte("data: "), frame...), '\n', '\n')); err != nil {
		t.Fatal(err)
	}
	s.Abort()
	c := s.RestoreCounts()
	if c.Total != 1 || c.Restored != 0 || c.Complete {
		t.Fatalf("aborted counts=%+v", c)
	}
}

type evidenceBufferSession struct{}

func (*evidenceBufferSession) Push(context.Context, []byte) ([]byte, error) { return nil, nil }
func (*evidenceBufferSession) Finish(context.Context) ([]byte, error)       { return nil, nil }
func (*evidenceBufferSession) Abort()                                       {}
