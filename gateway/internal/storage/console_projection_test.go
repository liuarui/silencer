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

func TestConsoleReadProjections(t *testing.T) {
	for _, mode := range []string{"sqlite", "mysql"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("SILENCER_STORAGE_MODE", mode)
			if mode == "mysql" {
				dsn := os.Getenv("SILENCER_TEST_MYSQL_DSN")
				if dsn == "" {
					t.Skip("requires disposable MySQL")
				}
				t.Setenv("SILENCER_MYSQL_DSN", dsn)
			} else {
				t.Setenv("SILENCER_SQLITE_PATH", filepath.Join(t.TempDir(), "console.db"))
			}
			ctx := context.Background()
			s, err := OpenFromEnv(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			now := time.Now().UTC()
			prefix := fmt.Sprintf("projection-%d-", time.Now().UnixNano())
			large := bytes.Repeat([]byte("synthetic-body"), 160000)
			meta := `{"items_complete":true,"mask_token_count":1,"count":1,"restore_count":1,"restore_restored":1,"restore_unresolved":0}`
			for _, e := range []Event{
				{ID: prefix + "modern", RequestID: prefix + "request", Type: "MASK", Raw: large, Masked: large, Upstream: large, Restored: large, Meta: meta, CreatedAt: now},
				{ID: prefix + "legacy", Type: "MASK", Masked: []byte("{{EMAIL_0123456789abcdef}}"), CreatedAt: now.Add(-time.Second)},
			} {
				if err = s.AppendEvent(ctx, e); err != nil {
					t.Fatal(err)
				}
			}
			if err = s.AppendRequest(ctx, Request{ID: prefix + "request", Raw: large, Masked: large, Upstream: large, Restored: large, Meta: meta, CreatedAt: now}); err != nil {
				t.Fatal(err)
			}
			events, err := s.ListLogSummaries(ctx, 5000)
			if err != nil {
				t.Fatal(err)
			}
			found := map[string]bool{}
			for _, e := range events {
				if e.ID == prefix+"modern" {
					found[e.ID] = true
					if len(e.Raw)+len(e.Masked)+len(e.Upstream)+len(e.Restored) != 0 || e.StatsMeta["count"] != float64(1) {
						t.Fatal("modern list loaded bodies or lost counters")
					}
				}
				if e.ID == prefix+"legacy" {
					found[e.ID] = true
					if string(e.Masked) != "{{EMAIL_0123456789abcdef}}" {
						t.Fatal("legacy masked evidence lost")
					}
				}
			}
			if len(found) != 2 {
				t.Fatal("lost event ordering/window")
			}
			for i := 0; i < 3; i++ {
				if err = s.AppendEvent(ctx, Event{ID: fmt.Sprintf("%snoise-%d", prefix, i), Type: "MASK", Meta: "invalid old metadata", CreatedAt: now.Add(time.Second)}); err != nil {
					t.Fatal(err)
				}
			}
			if err = s.AppendEvent(ctx, Event{ID: prefix + "associated", Type: "MASK", Meta: fmt.Sprintf(`{"sid":%q,"items_complete":true,"count":1}`, prefix+"sid"), CreatedAt: now}); err != nil {
				t.Fatal(err)
			}
			related, err := s.RelatedLogSummaries(ctx, prefix+"sid", 1)
			if err != nil || len(related) != 1 || related[0].ID != prefix+"associated" {
				t.Fatalf("legacy association displaced by unrelated rows: %v", err)
			}
			requests, err := s.RequestStats(ctx, now.Add(-time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range requests {
				if r.ID == prefix+"request" && len(r.Raw)+len(r.Masked)+len(r.Upstream)+len(r.Restored) != 0 {
					t.Fatal("request aggregate loaded complete bodies")
				}
			}
			bodyRequests, bodyEvents, err := s.RestoreBodyStats(ctx, now.Add(-time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			if len(bodyRequests) == 0 || len(bodyEvents) == 0 {
				t.Fatal("restore byte summary lost rows")
			}
			for _, r := range bodyRequests {
				if r.ID == prefix+"request" && r.Bytes != len(large) {
					t.Fatal("request byte length changed")
				}
			}
			for _, e := range bodyEvents {
				if e.ID == prefix+"modern" && e.Bytes != len(large) {
					t.Fatal("event byte length changed")
				}
			}
		})
	}
}
