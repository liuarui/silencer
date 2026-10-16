package storage

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestMySQLLogRetentionAndSummary(t *testing.T) {
	dsn := os.Getenv("SILENCER_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("requires disposable MySQL")
	}
	t.Setenv("SILENCER_STORAGE_MODE", "mysql")
	t.Setenv("SILENCER_MYSQL_DSN", dsn)
	s, err := OpenFromEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	old := time.Now().Add(-5 * 24 * time.Hour)
	id := "retention-mysql-" + time.Now().Format("150405.000000000")
	if err = s.AppendRequest(ctx, Request{ID: id, Status: "200", Raw: []byte("retention-fixture"), CreatedAt: old}); err != nil {
		t.Fatal(err)
	}
	if err = s.AppendEvent(ctx, Event{ID: id + "-event", RequestID: id, Type: "MASK", Masked: []byte("fixture"), CreatedAt: old}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListRequestSummaries(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range rows {
		if row.ID == id {
			found = true
			if row.Raw != nil || row.SummaryBodyAvailable == nil || !*row.SummaryBodyAvailable {
				t.Fatal("summary body projection incorrect")
			}
		}
	}
	if !found {
		t.Fatal("summary missing before expiry")
	}
	ids, n, err := s.ExpireLogDetails(ctx, time.Now().Add(-3*24*time.Hour), 32, func(Request) string { return `{"log_expired":true,"body_cleanup_pending":true}` }, func(Event) string { return `{"log_expired":true}` }, func(string) bool { return false })
	if err != nil || len(ids) == 0 || n < 2 {
		t.Fatal("MySQL expiry failed", err)
	}
	row, err := s.GetRequest(ctx, id)
	if err != nil || len(row.Raw) != 0 {
		t.Fatal("body not cleared", err)
	}
	if err = s.CompleteBodyCleanup(ctx, id); err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingBodyCleanup(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pending {
		if p == id {
			t.Fatal("pending cleanup acknowledgement failed")
		}
	}
	rows, err = s.ListRequestSummaries(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.ID == id {
			t.Fatal("expired summary returned")
		}
	}
}
