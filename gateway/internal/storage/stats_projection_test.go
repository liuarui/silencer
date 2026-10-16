package storage

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestDashboardStatsAvoidsCompleteBodiesAndKeepsLegacyFallback(t *testing.T) {
	t.Setenv("SILENCER_STORAGE_MODE", "sqlite")
	t.Setenv("SILENCER_SQLITE_PATH", filepath.Join(t.TempDir(), "projection.db"))
	s, err := OpenFromEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	large := bytes.Repeat([]byte("large body"), 200000)
	now := time.Now().UTC()
	for _, r := range []Request{
		{ID: "complete", CreatedAt: now, Raw: large, Masked: large, Upstream: large, Restored: large, Meta: `{"items_complete":true,"mask_token_count":1,"restore_count":1,"items":[{"label":"EMAIL","original":"user@example.test"}]}`},
		{ID: "legacy", CreatedAt: now, Masked: []byte("{{EMAIL_legacy}}"), Upstream: []byte("{{EMAIL_legacy}}"), Restored: []byte("user@example.test")},
		{ID: "out-of-range", CreatedAt: now.Add(-48 * time.Hour), Raw: large},
	} {
		if err = s.AppendRequest(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
	rows, _, err := s.DashboardStats(context.Background(), now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatal("range changed")
	}
	for _, r := range rows {
		if len(r.Raw) != 0 || len(r.Payload) != 0 {
			t.Fatal("unused columns fetched")
		}
		if r.ID == "complete" && len(r.Masked)+len(r.Upstream)+len(r.Restored) != 0 {
			t.Fatal("complete body columns fetched")
		}
		if r.ID == "legacy" && (string(r.Masked) != "{{EMAIL_legacy}}" || string(r.Restored) != "user@example.test") {
			t.Fatal("legacy fallback lost")
		}
	}
}

func BenchmarkDashboardStatsBodySize(b *testing.B) {
	for _, size := range []int{64 << 10, 4 << 20} {
		b.Run(fmt.Sprintf("%dKiB", size>>10), func(b *testing.B) {
			b.Setenv("SILENCER_STORAGE_MODE", "sqlite")
			b.Setenv("SILENCER_SQLITE_PATH", filepath.Join(b.TempDir(), "bench.db"))
			s, err := OpenFromEnv(context.Background())
			if err != nil {
				b.Fatal(err)
			}
			defer s.Close()
			body := bytes.Repeat([]byte("x"), size)
			if err = s.AppendRequest(context.Background(), Request{ID: "complete", CreatedAt: time.Now(), Raw: body, Masked: body, Upstream: body, Restored: body, Meta: `{"items_complete":true,"mask_token_count":1,"restore_count":1,"items":[{"label":"EMAIL"}]}`}); err != nil {
				b.Fatal(err)
			}
			if err = s.AppendEvent(context.Background(), Event{ID: "complete-event", RequestID: "complete", Type: "MASK", CreatedAt: time.Now(), Raw: body, Masked: body, Upstream: body, Restored: body, Meta: `{"items_complete":true,"count":1}`}); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, _, err := s.DashboardStats(context.Background(), time.Now().Add(-time.Hour)); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
