package storage

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRequestSummariesExcludeExpiredAndLargeBodies(t *testing.T) {
	t.Setenv("SILENCER_STORAGE_MODE", "sqlite")
	t.Setenv("SILENCER_SQLITE_PATH", filepath.Join(t.TempDir(), "summary.db"))
	s, err := OpenFromEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	large := bytes.Repeat([]byte("x"), 2<<20)
	for _, r := range []Request{
		{ID: "expired", CreatedAt: now.Add(time.Hour), Meta: `{"log_expired":true}`},
		{ID: "latest", CreatedAt: now, Raw: large, Masked: large, Upstream: large, Restored: large},
		{ID: "older", CreatedAt: now.Add(-time.Hour), Meta: `{"body_store":true}`},
	} {
		if err = s.AppendRequest(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.ListRequestSummaries(context.Background(), 2)
	if err != nil || len(rows) != 2 || rows[0].ID != "latest" || rows[1].ID != "older" {
		t.Fatal("summary ordering/retention incorrect", err)
	}
	for _, r := range rows {
		if len(r.Raw)+len(r.Masked)+len(r.Upstream)+len(r.Restored) != 0 {
			t.Fatal("summary fetched bodies")
		}
	}
	if rows[0].SummaryBodyAvailable == nil || !*rows[0].SummaryBodyAvailable {
		t.Fatal("legacy body availability lost")
	}
}

func TestRequestSummarySeekAndLegacyResponseProjection(t *testing.T) {
	for _, mode := range []string{"sqlite", "mysql"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("SILENCER_STORAGE_MODE", mode)
			if mode == "mysql" {
				dsn := os.Getenv("SILENCER_TEST_MYSQL_DSN")
				if dsn == "" {
					t.Skip("set SILENCER_TEST_MYSQL_DSN for MySQL verification")
				}
				t.Setenv("SILENCER_MYSQL_DSN", dsn)
			} else {
				t.Setenv("SILENCER_SQLITE_PATH", filepath.Join(t.TempDir(), "seek.db"))
			}
			s, err := OpenFromEnv(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			now := time.Now().UTC().Truncate(time.Second)
			prefix := fmt.Sprintf("seek-%d-", now.UnixNano())
			for _, item := range []Request{
				{ID: prefix + "a", Status: "200", Upstream: []byte("legacy-upstream"), Restored: []byte("legacy-restored")},
				{ID: prefix + "b", Status: "200", Meta: `{"restore_count":1}`, Upstream: []byte("not-needed")},
				{ID: prefix + "c", Status: "200", Upstream: []byte("event-authoritative")},
				{ID: prefix + "d", Status: "200", Meta: `{"log_expired":true}`},
			} {
				item.CreatedAt = now
				if err := s.AppendRequest(context.Background(), item); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.AppendEvent(context.Background(), Event{ID: prefix + "restore", RequestID: prefix + "c", Type: "RESTORE", Status: "restored", Raw: []byte("private-body"), CreatedAt: now}); err != nil {
				t.Fatal(err)
			}
			var items []Request
			var before time.Time
			beforeID := ""
			for {
				page, err := s.ListRequestSummariesBefore(context.Background(), 2, before, beforeID)
				if err != nil {
					t.Fatal(err)
				}
				items = append(items, page...)
				if len(page) < 2 {
					break
				}
				last := page[len(page)-1]
				before, beforeID = last.CreatedAt, last.ID
			}
			var fixture []Request
			for _, item := range items {
				if len(item.ID) >= len(prefix) && item.ID[:len(prefix)] == prefix {
					fixture = append(fixture, item)
				}
			}
			if len(fixture) != 3 || fixture[0].ID != prefix+"c" || fixture[1].ID != prefix+"b" || fixture[2].ID != prefix+"a" {
				t.Fatalf("seek lost timestamp ties: %v", fixture)
			}
			events, err := s.RequestEventSummaries(context.Background(), []string{prefix + "a", prefix + "b", prefix + "c"})
			if err != nil || len(events) != 1 || len(events[0].Raw) != 0 {
				t.Fatal("event summary read bodies or lost outcome", err)
			}
			if err := s.HydrateLegacyResponseSummaries(context.Background(), fixture, events); err != nil {
				t.Fatal(err)
			}
			if len(fixture[0].Upstream) != 0 || len(fixture[1].Upstream) != 0 || string(fixture[2].Upstream) != "legacy-upstream" || string(fixture[2].Restored) != "legacy-restored" {
				t.Fatal("legacy fallback differs from dashboard")
			}
		})
	}
}
