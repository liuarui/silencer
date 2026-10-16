package storage

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCloseDrainsQueueAndRejectsNewWork(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SILENCER_STORAGE_MODE", "sqlite")
	t.Setenv("SILENCER_SQLITE_PATH", filepath.Join(dir, "close.db"))
	s, err := OpenFromEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{}, 32)
	for i := 0; i < cap(done); i++ {
		if err := s.Enqueue(func(*sql.DB) error { done <- struct{}{}; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if got := len(done); got != cap(done) {
		t.Fatalf("close dropped queued operations: got %d want %d", got, cap(done))
	}
	if err := s.Enqueue(func(*sql.DB) error { return nil }); err == nil {
		t.Fatal("enqueue after close succeeded")
	}
}

func TestSQLiteRoundTripAndQueue(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SILENCER_STORAGE_MODE", "sqlite")
	t.Setenv("SILENCER_SQLITE_PATH", filepath.Join(dir, "silencer.db"))
	s, err := OpenFromEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := s.AppendRequest(context.Background(), Request{ID: "r1", Payload: []byte(`{"ok":true}`), CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendEvent(context.Background(), Event{ID: "e1", RequestID: "r1", Type: "MASK", Payload: []byte("event"), CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	request, err := s.GetRequest(context.Background(), "r1")
	if err != nil || request.ID != "r1" || string(request.Payload) != `{"ok":true}` {
		t.Fatalf("request detail=%+v err=%v", request, err)
	}
	requests, err := s.ListRequests(context.Background(), 10)
	if err != nil || len(requests) != 1 || requests[0].ID != "r1" {
		t.Fatalf("requests=%+v err=%v", requests, err)
	}
	events, err := s.ListEvents(context.Background(), "r1", 10)
	if err != nil || len(events) != 1 || events[0].Type != "MASK" {
		t.Fatalf("events=%+v err=%v", events, err)
	}
	done := make(chan struct{})
	if err := s.Enqueue(func(db *sql.DB) error {
		close(done)
		_, err := db.ExecContext(context.Background(), `INSERT INTO events(id,request_id,event_type,payload,created_at) VALUES(?,?,?,?,?)`, "e2", "r1", "AUDIT", []byte("queued"), now)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("queued operation did not run")
	}
	if _, err := os.Stat(filepath.Join(dir, "silencer.db")); err != nil {
		t.Fatal(err)
	}
}

func TestSQLiteEnablesWALAndBusyTimeout(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SILENCER_STORAGE_MODE", "sqlite")
	t.Setenv("SILENCER_SQLITE_PATH", filepath.Join(dir, "pragmas.db"))
	s, err := OpenFromEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var journal string
	if err := s.db.QueryRowContext(context.Background(), "PRAGMA journal_mode").Scan(&journal); err != nil {
		t.Fatal(err)
	}
	if strings.ToLower(journal) != "wal" {
		t.Fatalf("journal_mode=%q, want wal", journal)
	}
	var timeout int
	if err := s.db.QueryRowContext(context.Background(), "PRAGMA busy_timeout").Scan(&timeout); err != nil {
		t.Fatal(err)
	}
	if timeout < 1 {
		t.Fatalf("busy_timeout=%d", timeout)
	}
	if got := s.Snapshot().DB.MaxOpenConnections; got < 2 {
		t.Fatalf("sqlite pool max open=%d, want concurrent read capacity", got)
	}
}

func TestSQLiteRestartPreservesLedgerAndReinitializesWriter(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SILENCER_STORAGE_MODE", "sqlite")
	t.Setenv("SILENCER_SQLITE_PATH", filepath.Join(dir, "restart.db"))
	store, err := OpenFromEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AppendRequest(context.Background(), Request{ID: "restart-1", Payload: []byte("before-restart"), Status: "200"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := OpenFromEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	items, err := restarted.ListRequests(context.Background(), 10)
	if err != nil || len(items) != 1 || items[0].ID != "restart-1" || string(items[0].Payload) != "before-restart" {
		t.Fatalf("ledger after restart=%+v err=%v", items, err)
	}
	if err := restarted.AppendRequest(context.Background(), Request{ID: "restart-2", Payload: []byte("after-restart"), Status: "200"}); err != nil {
		t.Fatal(err)
	}
	if snapshot := restarted.Snapshot(); snapshot.QueueDepth != 0 || snapshot.LastWriteErr {
		t.Fatalf("writer did not recover cleanly: %+v", snapshot)
	}
}

func TestDeleteAuditEventsKeepsOperationalEvents(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SILENCER_STORAGE_MODE", "sqlite")
	t.Setenv("SILENCER_SQLITE_PATH", filepath.Join(dir, "audit-delete.db"))
	s, err := OpenFromEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC()
	for _, event := range []Event{
		{ID: "audit-1", RequestID: "r", Type: "AUDIT_error_leak", Payload: []byte{}, CreatedAt: now},
		{ID: "audit-2", RequestID: "r", Type: "AUDIT_stream", Payload: []byte{}, CreatedAt: now},
		{ID: "normal-1", RequestID: "r", Type: "MASK", Payload: []byte{}, CreatedAt: now},
		{ID: "audit-prefix-like", RequestID: "r", Type: "AUDITX_internal", Payload: []byte{}, CreatedAt: now},
	} {
		if err := s.AppendEvent(context.Background(), event); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeleteAuditEvents(context.Background()); err != nil {
		t.Fatal(err)
	}
	events, err := s.ListAllEvents(context.Background(), 10)
	if err != nil || len(events) != 2 {
		t.Fatalf("events after audit delete=%+v err=%v", events, err)
	}
	ids := map[string]bool{events[0].ID: true, events[1].ID: true}
	if !ids["normal-1"] || !ids["audit-prefix-like"] {
		t.Fatalf("unexpected events after audit delete=%+v", events)
	}
}

func TestSQLiteStoresLargePayloadsAndNullTerminalTimes(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SILENCER_STORAGE_MODE", "sqlite")
	t.Setenv("SILENCER_SQLITE_PATH", filepath.Join(dir, "large.db"))
	s, err := OpenFromEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	created := time.Now().UTC().Truncate(time.Microsecond)
	large := make([]byte, 128<<10)
	for i := range large {
		large[i] = byte('a' + i%26)
	}
	if err := s.AppendRequest(context.Background(), Request{ID: "large", Payload: large, CreatedAt: created}); err != nil {
		t.Fatal(err)
	}
	items, err := s.ListRequests(context.Background(), 10)
	if err != nil || len(items) != 1 {
		t.Fatalf("items=%d err=%v", len(items), err)
	}
	if len(items[0].Payload) != len(large) || !items[0].StartedAt.Equal(created) {
		t.Fatalf("payload/time mismatch: payload=%d started=%v", len(items[0].Payload), items[0].StartedAt)
	}
	if !items[0].EndedAt.IsZero() {
		t.Fatalf("zero ended time was not stored as NULL: %v", items[0].EndedAt)
	}
}

func TestNullableTime(t *testing.T) {
	if got := nullableTime(time.Time{}); got != nil {
		t.Fatalf("zero time argument = %#v, want nil", got)
	}
	now := time.Now()
	if got := nullableTime(now); got != now {
		t.Fatalf("non-zero time changed: %#v", got)
	}
}

func TestMySQLPoolSizeRejectsInvalidValueBeforeConnecting(t *testing.T) {
	t.Setenv("SILENCER_STORAGE_MODE", "mysql")
	t.Setenv("SILENCER_MYSQL_DSN", "invalid-dsn")
	t.Setenv("SILENCER_MYSQL_POOL_SIZE", "0")
	if _, err := OpenFromEnv(context.Background()); err == nil {
		t.Fatal("invalid pool size was accepted")
	}
}

func TestSnapshotAndMetricsExposeWriterBackpressure(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SILENCER_STORAGE_MODE", "sqlite")
	t.Setenv("SILENCER_SQLITE_PATH", filepath.Join(dir, "snapshot.db"))
	s, err := OpenFromEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	snapshot := s.Snapshot()
	if snapshot.Mode != "sqlite" || snapshot.QueueCapacity != WriterQueueCapacity || snapshot.QueueDepth != 0 {
		t.Fatalf("unexpected snapshot: %+v", snapshot)
	}
	metrics := s.MetricsText()
	for _, want := range []string{
		"silencer_storage_queue_depth 0",
		"silencer_storage_queue_capacity 256",
		"silencer_storage_last_write_error 0",
	} {
		if !strings.Contains(metrics, want) {
			t.Fatalf("metrics missing %q:\n%s", want, metrics)
		}
	}
}

func TestWriterQueueRejectsWhenFull(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SILENCER_STORAGE_MODE", "sqlite")
	t.Setenv("SILENCER_SQLITE_PATH", filepath.Join(dir, "queue-full.db"))
	s, err := OpenFromEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	started := make(chan struct{})
	release := make(chan struct{})
	if err := s.Enqueue(func(*sql.DB) error {
		close(started)
		<-release
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("blocking writer did not start")
	}
	for i := 0; i < WriterQueueCapacity; i++ {
		if err := s.Enqueue(func(*sql.DB) error { return nil }); err != nil {
			t.Fatalf("queue rejected before capacity: i=%d err=%v", i, err)
		}
	}
	if err := s.Enqueue(func(*sql.DB) error { return nil }); err == nil || !strings.Contains(err.Error(), "queue full") {
		t.Fatalf("full queue error=%v", err)
	}
	close(release)
}

func TestCanceledQueuedWriteIsSkipped(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SILENCER_STORAGE_MODE", "sqlite")
	t.Setenv("SILENCER_SQLITE_PATH", filepath.Join(dir, "canceled.db"))
	s, err := OpenFromEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	started := make(chan struct{})
	release := make(chan struct{})
	if err := s.Enqueue(func(*sql.DB) error {
		close(started)
		<-release
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("blocking writer did not start")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = s.AppendRequest(ctx, Request{ID: "must-not-write", Payload: []byte("payload")})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled append error=%v, want context.Canceled", err)
	}
	close(release)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		count, countErr := s.CountRequests(context.Background())
		if countErr == nil && count == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	count, err := s.CountRequests(context.Background())
	if err != nil || count != 0 {
		t.Fatalf("canceled write reached database: count=%d err=%v", count, err)
	}
}

func TestWriterRecoversFromPanicAndContinues(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SILENCER_STORAGE_MODE", "sqlite")
	t.Setenv("SILENCER_SQLITE_PATH", filepath.Join(dir, "panic.db"))
	s, err := OpenFromEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	started := make(chan struct{})
	if err := s.Enqueue(func(*sql.DB) error {
		close(started)
		panic("test writer panic")
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("panic task did not start")
	}
	if err := s.AppendRequest(context.Background(), Request{ID: "after-panic", Payload: []byte("ok")}); err != nil {
		t.Fatalf("writer did not continue after panic: %v", err)
	}
	if last := s.LastError(); last == nil || !strings.Contains(last.Error(), "storage writer panic") {
		t.Fatalf("panic was not recorded: %v", last)
	}
}

func TestDeleteEventsUsesWriterQueue(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SILENCER_STORAGE_MODE", "sqlite")
	t.Setenv("SILENCER_SQLITE_PATH", filepath.Join(dir, "delete.db"))
	s, err := OpenFromEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.AppendEvent(context.Background(), Event{ID: "delete-me", RequestID: "r", Type: "MASK", Payload: []byte("event")}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteEvents(context.Background()); err != nil {
		t.Fatal(err)
	}
	events, err := s.ListAllEvents(context.Background(), 10)
	if err != nil || len(events) != 0 {
		t.Fatalf("events after queued delete=%+v err=%v", events, err)
	}
}

func TestCanceledCallerGetsDefinitiveCommittedResult(t *testing.T) {
	t.Setenv("SILENCER_STORAGE_MODE", "sqlite")
	t.Setenv("SILENCER_SQLITE_PATH", filepath.Join(t.TempDir(), "commit-cancel.db"))
	s, err := OpenFromEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err = s.enqueueAndWait(ctx, func(taskCtx context.Context, db *sql.DB) error {
		_, err := db.ExecContext(taskCtx, "CREATE TABLE committed_cancel(value INTEGER)")
		cancel() // Simulate cancellation after a durable write, before acknowledgment.
		return err
	})
	if err != nil {
		t.Fatal("committed write misreported as canceled", err)
	}
}
