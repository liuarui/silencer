package storage

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestCopyPreservesPayloadsLargerThanLegacyLimit(t *testing.T) {
	ctx := context.Background()
	sourcePath := filepath.Join(t.TempDir(), "legacy-large.db")
	source, err := OpenDatabase(ctx, DatabaseConfig{Mode: "sqlite", SQLitePath: sourcePath})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if _, err := source.ExecContext(ctx, `CREATE TABLE requests (
		request_id TEXT PRIMARY KEY, started REAL NOT NULL, ended REAL,
		status TEXT NOT NULL, raw BLOB, masked BLOB, upstream_response BLOB,
		restored_response BLOB)`); err != nil {
		t.Fatal(err)
	}
	large := bytes.Repeat([]byte("payload-"), (9<<20)/len("payload-"))
	if _, err := source.ExecContext(ctx, `INSERT INTO requests(request_id,started,status,raw,masked,upstream_response,restored_response) VALUES(?,?,?,?,?,?,?)`, "large-1", 1700000000.0, "completed", large, large, large, large); err != nil {
		t.Fatal(err)
	}

	t.Setenv("SILENCER_STORAGE_MODE", "sqlite")
	t.Setenv("SILENCER_SQLITE_PATH", filepath.Join(t.TempDir(), "target.db"))
	target, err := OpenFromEnv(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	report, err := target.CopyFrom(ctx, source, CopyOptions{SourceMode: "sqlite", Verify: true})
	if err != nil || !report.Verified || report.RequestsVerified != 1 {
		t.Fatalf("large copy report=%+v err=%v", report, err)
	}
	items, err := target.ListRequests(ctx, 10)
	if err != nil || len(items) != 1 {
		t.Fatalf("requests=%d err=%v", len(items), err)
	}
	item := items[0]
	for name, got := range map[string][]byte{"raw": item.Raw, "masked": item.Masked, "upstream": item.Upstream, "restored": item.Restored} {
		if !bytes.Equal(got, large) {
			t.Fatalf("%s payload truncated: got=%d want=%d", name, len(got), len(large))
		}
	}
}

func TestRuntimeRejectsLegacySQLiteSchemaWithCopyGuidance(t *testing.T) {
	ctx := context.Background()
	legacyPath := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := OpenDatabase(ctx, DatabaseConfig{Mode: "sqlite", SQLitePath: legacyPath})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.ExecContext(ctx, `CREATE TABLE requests (request_id TEXT PRIMARY KEY, started REAL NOT NULL, ended REAL, status TEXT NOT NULL, payload BLOB)`); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.ExecContext(ctx, `CREATE TABLE events (id INTEGER PRIMARY KEY, ts REAL NOT NULL, type TEXT NOT NULL, sid TEXT, payload TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SILENCER_STORAGE_MODE", "sqlite")
	t.Setenv("SILENCER_SQLITE_PATH", legacyPath)
	_, err = OpenFromEnv(ctx)
	if err == nil || !strings.Contains(err.Error(), "legacy requests schema detected") || !strings.Contains(err.Error(), "silencer-migrate -copy") {
		t.Fatalf("expected actionable legacy-schema error, got %v", err)
	}
}

func TestCopyFromLegacySQLite(t *testing.T) {
	ctx := context.Background()
	sourcePath := filepath.Join(t.TempDir(), "legacy.db")
	source, err := OpenDatabase(ctx, DatabaseConfig{Mode: "sqlite", SQLitePath: sourcePath})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	_, err = source.ExecContext(ctx, `CREATE TABLE requests (
		request_id TEXT PRIMARY KEY, started REAL NOT NULL, ended REAL,
		status TEXT NOT NULL, client TEXT, upstream TEXT, model TEXT, path TEXT,
		raw BLOB, masked BLOB, upstream_response BLOB, restored_response BLOB,
		body_encoding TEXT NOT NULL DEFAULT 'plain', truncated INTEGER NOT NULL DEFAULT 0,
		body_unavailable INTEGER NOT NULL DEFAULT 0)`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = source.ExecContext(ctx, `CREATE TABLE events (
		id INTEGER PRIMARY KEY AUTOINCREMENT, ts REAL NOT NULL, type TEXT NOT NULL,
		sid TEXT, host TEXT, method TEXT, path TEXT, count INTEGER DEFAULT 0,
		restored INTEGER DEFAULT 0, status TEXT, http_status INTEGER, ingress TEXT,
		payload TEXT NOT NULL)`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = source.ExecContext(ctx, `CREATE TABLE audit_events (
		id INTEGER PRIMARY KEY AUTOINCREMENT, ts REAL NOT NULL, sid TEXT, host TEXT,
		method TEXT, path TEXT, signal_type TEXT NOT NULL, severity TEXT NOT NULL,
		evidence TEXT, request_hash TEXT, response_hash TEXT, probe_id TEXT)`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = source.ExecContext(ctx, `INSERT INTO requests(request_id,started,ended,status,client,upstream,model,path,raw,masked,upstream_response,restored_response,body_encoding,truncated,body_unavailable) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		"req-1", 1700000000.25, 1700000001.5, "completed", "cli", "mock", "model-a", "/v1/chat", []byte("raw"), []byte("masked"), []byte("upstream"), []byte("restored"), "plain", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = source.ExecContext(ctx, `INSERT INTO audit_events(ts,sid,host,method,path,signal_type,severity,evidence,request_hash,response_hash,probe_id) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		1700000001.5, "req-1", "mock", "POST", "/v1/chat", "credential_echo", "HIGH", "evidence", "rh", "sh", "probe-1")
	if err != nil {
		t.Fatal(err)
	}
	_, err = source.ExecContext(ctx, `INSERT INTO events(ts,type,sid,host,method,path,count,restored,status,http_status,ingress,payload) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		1700000001.5, "RESTORE", "req-1", "mock", "POST", "/v1/chat", 1, 1, "ok", 200, "proxy", `{"count":1}`)
	if err != nil {
		t.Fatal(err)
	}

	targetPath := filepath.Join(t.TempDir(), "target.db")
	t.Setenv("SILENCER_STORAGE_MODE", "sqlite")
	t.Setenv("SILENCER_SQLITE_PATH", targetPath)
	target, err := OpenFromEnv(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	report, err := target.CopyFrom(ctx, source, CopyOptions{SourceMode: "sqlite", BatchSize: 1, Verify: true})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Verified || report.RequestsSource != 1 || report.EventsSource != 1 || report.AuditEventsSource != 1 || report.RequestsVerified != 1 || report.EventsVerified != 1 || report.AuditEventsVerified != 1 {
		t.Fatalf("unexpected report: %+v", report)
	}
	requests, err := target.ListRequests(ctx, 10)
	if err != nil || len(requests) != 1 {
		t.Fatalf("requests=%+v err=%v", requests, err)
	}
	if requests[0].ID != "req-1" || string(requests[0].Upstream) != "upstream" || requests[0].Meta == "" {
		t.Fatalf("legacy request was not mapped: %+v", requests[0])
	}
	events, err := target.ListAllEvents(ctx, 10)
	if err != nil || len(events) != 2 {
		t.Fatalf("events=%+v err=%v", events, err)
	}
	byID := map[string]Event{}
	for _, event := range events {
		byID[event.ID] = event
	}
	if audit := byID["legacy-audit-1"]; audit.Type != "AUDIT_credential_echo" || audit.RequestID != "req-1" {
		t.Fatalf("legacy audit event was not mapped: %+v", audit)
	}
	if event := byID["legacy-event-1"]; event.Type != "RESTORE" || event.RequestID != "req-1" {
		t.Fatalf("legacy event was not mapped: %+v", event)
	}

	second, err := target.CopyFrom(ctx, source, CopyOptions{SourceMode: "sqlite", Verify: true})
	if err != nil || !second.Verified || second.RequestsCopied != 1 || second.EventsCopied != 1 || second.AuditEventsCopied != 1 {
		t.Fatalf("idempotent copy failed: report=%+v err=%v", second, err)
	}
}
