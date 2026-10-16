package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMySQLRoundTrip(t *testing.T) {
	dsn := os.Getenv("SILENCER_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set SILENCER_TEST_MYSQL_DSN to run the MySQL integration test")
	}
	t.Setenv("SILENCER_STORAGE_MODE", "mysql")
	t.Setenv("SILENCER_MYSQL_DSN", dsn)
	s, err := OpenFromEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	id := fmt.Sprintf("mysql-test-%d", time.Now().UnixNano())
	if err := s.AppendRequest(context.Background(), Request{ID: id, Payload: []byte("payload"), Status: "running"}); err != nil {
		t.Fatal(err)
	}
	item, err := s.GetRequest(context.Background(), id)
	if err != nil || item.ID != id || string(item.Payload) != "payload" {
		t.Fatalf("inserted request not found: item=%+v err=%v", item, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := OpenFromEnv(context.Background())
	if err != nil {
		t.Fatalf("reopen MySQL storage: %v", err)
	}
	defer restarted.Close()
	item, err = restarted.GetRequest(context.Background(), id)
	if err != nil || item.ID != id || string(item.Payload) != "payload" {
		t.Fatalf("request was not durable across writer restart: item=%+v err=%v", item, err)
	}
}

func TestMySQLCopiesLegacySQLiteRows(t *testing.T) {
	dsn := os.Getenv("SILENCER_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set SILENCER_TEST_MYSQL_DSN to run the MySQL integration test")
	}
	ctx := context.Background()
	source, err := OpenDatabase(ctx, DatabaseConfig{Mode: "sqlite", SQLitePath: filepath.Join(t.TempDir(), "legacy.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if _, err := source.ExecContext(ctx, `CREATE TABLE requests (request_id TEXT PRIMARY KEY, started REAL NOT NULL, ended REAL, status TEXT NOT NULL, path TEXT, raw BLOB, masked BLOB, upstream_response BLOB, restored_response BLOB)`); err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprintf("mysql-copy-test-%d", time.Now().UnixNano())
	large := []byte(strings.Repeat("payload-", (9<<20)/len("payload-")))
	if _, err := source.ExecContext(ctx, `INSERT INTO requests(request_id,started,ended,status,path,raw,masked,upstream_response,restored_response) VALUES(?,?,?,?,?,?,?,?,?)`, id, 1700000000.0, 1700000001.0, "completed", "/v1/chat", large, []byte("masked"), []byte("upstream"), large); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SILENCER_STORAGE_MODE", "mysql")
	t.Setenv("SILENCER_MYSQL_DSN", dsn)
	target, err := OpenFromEnv(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	report, err := target.CopyFrom(ctx, source, CopyOptions{SourceMode: "sqlite", BatchSize: 1, Verify: true})
	if err != nil || !report.Verified || report.RequestsCopied != 1 || report.RequestsVerified != 1 {
		t.Fatalf("legacy copy report=%+v err=%v", report, err)
	}
	items, err := target.ListRequests(ctx, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.ID == id && len(item.Raw) == len(large) && len(item.Restored) == len(large) && string(item.Raw) == string(large) && string(item.Restored) == string(large) {
			return
		}
	}
	t.Fatalf("legacy row missing after MySQL copy: %+v", items)
}

func TestMySQLEntityMappingsConcurrentDurability(t *testing.T) {
	dsn := os.Getenv("SILENCER_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set SILENCER_TEST_MYSQL_DSN for isolated MySQL integration")
	}
	t.Setenv("SILENCER_STORAGE_MODE", "mysql")
	t.Setenv("SILENCER_MYSQL_DSN", dsn)
	ctx := context.Background()
	a, err := OpenFromEnv(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := OpenFromEnv(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	digest := sha256.Sum256([]byte(fmt.Sprint(time.Now().UnixNano())))
	scope := hex.EncodeToString(digest[:])
	original := strings.Repeat("quoted \"\\ 中文\x00 ", 1000)
	var wg sync.WaitGroup
	tokens := make(chan string, 64)
	failures := make(chan error, 64)
	for i := 0; i < 64; i++ {
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
			t.Fatal("MySQL canonical identity forked")
		}
	}
	var count int
	if err = a.db.QueryRow(`SELECT COUNT(*) FROM entity_mapping_tokens WHERE scope=?`, scope).Scan(&count); err != nil || count != 1 {
		t.Fatalf("token rows=%d err=%v", count, err)
	}
	if err = a.DeleteEvents(ctx); err != nil {
		t.Fatal(err)
	}
	a.Close()
	b.Close()
	c, err := OpenFromEnv(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	got, ok, err := c.ResolveEntityMapping(ctx, scope, canonical)
	if err != nil || !ok || got.Original != original {
		t.Fatal("MySQL restart lost mapping", err)
	}
	again, err := c.IssueEntityMapping(ctx, scope, original, "SECRET")
	if err != nil || again.Token != canonical {
		t.Fatal("MySQL restart changed identity", err)
	}
}

func TestMySQLEntityMappingConcurrentAliasImport(t *testing.T) {
	dsn := os.Getenv("SILENCER_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("isolated MySQL integration")
	}
	t.Setenv("SILENCER_STORAGE_MODE", "mysql")
	t.Setenv("SILENCER_MYSQL_DSN", dsn)
	ctx := context.Background()
	a, err := OpenFromEnv(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := OpenFromEnv(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	for iteration := 0; iteration < 16; iteration++ {
		digest := sha256.Sum256([]byte(fmt.Sprint(time.Now().UnixNano(), iteration)))
		scope := hex.EncodeToString(digest[:])
		start := make(chan struct{})
		failures := make(chan error, 2)
		var wg sync.WaitGroup
		for i, s := range []*Storage{a, b} {
			wg.Add(1)
			go func(i int, s *Storage) {
				defer wg.Done()
				<-start
				label := "IP_PUBLIC"
				if i == 1 {
					label = "IPPUBLIC"
				}
				failures <- s.importEntityMapping(ctx, scope, EntityMapping{Token: "{{" + label + "_bcdfgh}}", Label: label, Original: fmt.Sprint(i)})
			}(i, s)
		}
		close(start)
		wg.Wait()
		close(failures)
		for err := range failures {
			if err != nil {
				t.Fatal(err)
			}
		}
		for i, label := range []string{"IP_PUBLIC", "IPPUBLIC"} {
			if _, ok, err := a.ResolveEntityMapping(ctx, scope, "{{"+label+"_bcdfgh}}"); err == nil || ok {
				t.Fatal("concurrent alias conflict resolved")
			}
			m, err := a.IssueEntityMapping(ctx, scope, fmt.Sprint(i), label)
			if err != nil || strings.HasSuffix(m.Token, "_bcdfgh}}") {
				t.Fatal("conflicting alias reissued", err)
			}
		}
	}
}
