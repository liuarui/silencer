package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func openEntityTestStore(t *testing.T, path string) *Storage {
	t.Helper()
	t.Setenv("SILENCER_STORAGE_MODE", "sqlite")
	t.Setenv("SILENCER_SQLITE_PATH", path)
	s, err := OpenFromEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func TestEntityMappingsSurviveRestartLogClearAndConcurrentIssuance(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mappings.db")
	a := openEntityTestStore(t, path)
	b := openEntityTestStore(t, path)
	scope := strings.Repeat("a", 64)
	original := "value \"quote\" \\ unicode 中文\x00end"
	var wg sync.WaitGroup
	tokens := make(chan string, 32)
	failures := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s := a
			if i%2 == 1 {
				s = b
			}
			m, err := s.IssueEntityMapping(ctx, scope, original, "SECRET")
			if err != nil {
				failures <- err
				return
			}
			tokens <- m.Token
		}(i)
	}
	wg.Wait()
	close(tokens)
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	canonical := ""
	for token := range tokens {
		if canonical == "" {
			canonical = token
		}
		if token != canonical {
			t.Fatal("concurrent processes issued different identities")
		}
	}
	other, err := a.IssueEntityMapping(ctx, strings.Repeat("b", 64), original, "SECRET")
	if err != nil || other.Token == canonical {
		t.Fatal("mapping scope not isolated", err)
	}
	if err = a.DeleteEvents(ctx); err != nil {
		t.Fatal(err)
	}
	a.Close()
	b.Close()
	c := openEntityTestStore(t, path)
	found, ok, err := c.ResolveEntityMapping(ctx, scope, canonical)
	if err != nil || !ok || found.Original != original {
		t.Fatal("mapping lost after restart/log clear", err)
	}
	again, err := c.IssueEntityMapping(ctx, scope, original, "SECRET")
	if err != nil || again.Token != canonical {
		t.Fatal("restart changed entity identity", err)
	}
	if _, ok, err = c.ResolveEntityMapping(ctx, strings.Repeat("c", 64), canonical); err != nil || ok {
		t.Fatal("mapping escaped scope")
	}
	wrong := strings.Replace(canonical, "SECRET_", "EMAIL_", 1)
	if _, ok, err = c.ResolveEntityMapping(ctx, scope, wrong); err != nil || ok {
		t.Fatal("unknown label was guessed")
	}
}
func TestEntityMappingMigrationHasNoAgeOr5000RowCutoff(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	s := openEntityTestStore(t, path)
	scope := strings.Repeat("d", 64)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	alphabet := "bcdfghjkmnpqrstvwxz"
	var oldest string
	for i := 0; i < 5005; i++ {
		suffix := make([]byte, 6)
		n := i
		for j := range suffix {
			suffix[j] = alphabet[n%len(alphabet)]
			n /= len(alphabet)
		}
		token := "{{IP_PUBLIC_" + string(suffix) + "}}"
		if i == 0 {
			oldest = token
		}
		meta, _ := json.Marshal(map[string]any{"mapping_mode": "entity-v1", "mapping_scope": scope, "items": []any{map[string]any{"preview": token, "original": fmt.Sprint("host-", i)}}})
		if _, err = tx.ExecContext(ctx, `INSERT INTO events(id,request_id,event_type,payload,meta,created_at) VALUES(?,?,?,?,?,?)`, fmt.Sprintf("legacy-%05d", i), "legacy", "MASK", []byte{}, string(meta), time.Now().Add(-10*24*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM entity_mapping_migrations`); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	s.Close()
	reopened := openEntityTestStore(t, path)
	var count int
	if err = reopened.db.QueryRow(`SELECT COUNT(*) FROM entity_mapping_tokens`).Scan(&count); err != nil || count != 5005 {
		t.Fatalf("migration count=%d err=%v", count, err)
	}
	mapping, ok, err := reopened.ResolveEntityMapping(ctx, scope, oldest)
	if err != nil || !ok || mapping.Original != "host-0" {
		t.Fatal("oldest mapping unavailable", err)
	}
	canonical, err := reopened.IssueEntityMapping(ctx, scope, "host-0", "IP_PUBLIC")
	if err != nil || canonical.Token != oldest {
		t.Fatal("migration changed canonical token", err)
	}
	normalized := strings.Replace(oldest, "IP_PUBLIC_", "IPPUBLIC_", 1)
	if _, ok, err = reopened.ResolveEntityMapping(ctx, scope, normalized); err != nil || !ok {
		t.Fatal("safe normalized label unavailable", err)
	}
}

func TestEntityMappingAliasAmbiguityNeverReissuesConflictingToken(t *testing.T) {
	ctx := context.Background()
	s := openEntityTestStore(t, filepath.Join(t.TempDir(), "aliases.db"))
	scope := strings.Repeat("e", 64)
	labels := []string{"IP_PUBLIC", "IPPUBLIC", "I_PPUB_LIC", "IP_P_UBLIC"}
	for i, label := range labels {
		original := "same"
		if i == 3 {
			original = "conflict"
		}
		if err := s.importEntityMapping(ctx, scope, EntityMapping{Token: "{{" + label + "_bcdfgh}}", Original: original, Label: label}); err != nil {
			t.Fatal(err)
		}
	}
	for i, label := range labels {
		token := "{{" + label + "_bcdfgh}}"
		if _, ok, err := s.ResolveEntityMapping(ctx, scope, token); err == nil || ok {
			t.Fatal("ambiguous alias resolved")
		}
		original := "same"
		if i == 3 {
			original = "conflict"
		}
		m, err := s.IssueEntityMapping(ctx, scope, original, label)
		if err != nil || m.Token == token {
			t.Fatal("ambiguous token reissued", err)
		}
		got, ok, err := s.ResolveEntityMapping(ctx, scope, m.Token)
		if err != nil || !ok || got.Original != original {
			t.Fatal("fresh token cannot round trip", err)
		}
	}
}

func TestEntityMappingsCopyIndependentStateAndFormattedLegacyEvidence(t *testing.T) {
	ctx := context.Background()
	source := openEntityTestStore(t, filepath.Join(t.TempDir(), "source.db"))
	scope := strings.Repeat("f", 64)
	m, err := source.IssueEntityMapping(ctx, scope, "durable-only", "SECRET")
	if err != nil {
		t.Fatal(err)
	}
	// Logs may already be cleared while protocol state must remain copyable.
	if err = source.DeleteEvents(ctx); err != nil {
		t.Fatal(err)
	}
	legacy := "{{IP_PUBLIC_bcdfgh}}"
	meta, _ := json.MarshalIndent(map[string]any{"mapping_mode": "entity-v1", "mapping_scope": scope, "items": []any{map[string]any{"preview": legacy, "original": "legacy-host"}}}, "", "  ")
	if _, err = source.db.Exec(`INSERT INTO events(id,request_id,event_type,payload,meta,created_at) VALUES(?,?,?,?,?,?)`, "formatted-legacy", "copy", "MASK", []byte{}, string(meta), time.Now()); err != nil {
		t.Fatal(err)
	}
	targetPath := filepath.Join(t.TempDir(), "target.db")
	target := openEntityTestStore(t, targetPath)
	for i := 0; i < 2; i++ {
		report, err := target.CopyFrom(ctx, source.db, CopyOptions{SourceMode: "sqlite", Verify: true, BatchSize: 1})
		if err != nil || !report.Verified || report.EntityMappingsCopied != 1 {
			t.Fatal("mapping copy failed", report, err)
		}
	}
	target.Close()
	target = openEntityTestStore(t, targetPath)
	for token, original := range map[string]string{m.Token: "durable-only", legacy: "legacy-host"} {
		got, ok, err := target.ResolveEntityMapping(ctx, scope, token)
		if err != nil || !ok || got.Original != original {
			t.Fatal("copied historical mapping missing", err)
		}
		again, err := target.IssueEntityMapping(ctx, scope, original, got.Label)
		if err != nil || again.Token != token {
			t.Fatal("copy changed canonical identity", err)
		}
	}
}

func TestEntityMappingsDatabaseFailureReturnsError(t *testing.T) {
	s := openEntityTestStore(t, filepath.Join(t.TempDir(), "failure.db"))
	scope := strings.Repeat("a", 64)
	m, err := s.IssueEntityMapping(context.Background(), scope, "original", "SECRET")
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err = s.IssueEntityMapping(context.Background(), scope, "original", "SECRET"); err == nil {
		t.Fatal("closed storage issued token")
	}
	if _, ok, err := s.ResolveEntityMapping(context.Background(), scope, m.Token); err == nil || ok {
		t.Fatal("closed storage resolved token")
	}
}
