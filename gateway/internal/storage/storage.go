package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/go-sql-driver/mysql"
	_ "github.com/mattn/go-sqlite3"
	"os"
	"sync"
	"time"
)

type Request struct {
	StatsMeta                       map[string]any `json:"-"`
	StatsMetaCanonical              bool           `json:"-"`
	SummaryBodyAvailable            *bool          `json:"-"`
	ID                              string
	Payload                         []byte // legacy payload (typically masked)
	Raw, Masked, Upstream, Restored []byte
	Status, Error, Meta             string
	StartedAt, EndedAt, CreatedAt   time.Time
}
type Event struct {
	StatsMeta                       map[string]any `json:"-"`
	StatsMetaCanonical              bool           `json:"-"`
	ID, RequestID, Type             string
	Payload                         []byte
	Raw, Masked, Upstream, Restored []byte
	Status, Error, Meta             string
	CreatedAt, StartedAt, EndedAt   time.Time
}
type Storage struct {
	db       *sql.DB
	mode     string
	queue    chan writeTask
	wg       sync.WaitGroup
	mu       sync.RWMutex
	closed   bool
	writeCtx context.Context
	cancel   context.CancelFunc
	errMu    sync.RWMutex
	lastErr  error
}

// WriterQueueCapacity is the fixed bound used to protect the database writer
// from unbounded memory growth when the database is slower than the gateway.
const WriterQueueCapacity = 256

// Snapshot is a point-in-time view of storage health and backpressure. It is
// intentionally free of database handles so callers cannot bypass the writer
// lifecycle or issue queries outside the storage package.
type Snapshot struct {
	Mode          string
	QueueDepth    int
	QueueCapacity int
	LastWriteErr  bool
	DB            sql.DBStats
}

// LatestSchemaVersion is the monotonically increasing schema revision.
const LatestSchemaVersion = 2

// MigrationStatusFromEnv inspects the configured database without applying
// migrations. A missing schema_migrations table is reported as version 0.
func MigrationStatusFromEnv(ctx context.Context) (version int, err error) {
	mode := os.Getenv("SILENCER_STORAGE_MODE")
	if mode == "" {
		mode = "sqlite"
	}
	driver, dsn := "sqlite3", os.Getenv("SILENCER_SQLITE_PATH")
	if mode == "mysql" {
		driver, dsn = "mysql", os.Getenv("SILENCER_MYSQL_DSN")
	}
	if dsn == "" {
		if mode == "sqlite" {
			dsn = "silencer.db"
		} else {
			return 0, fmt.Errorf("SILENCER_MYSQL_DSN required")
		}
	}
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	if err = db.PingContext(ctx); err != nil {
		return 0, err
	}
	err = db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&version)
	if err != nil && isMissingTableError(err) {
		return 0, nil
	}
	return version, err
}

// writeTask is the bounded writer-queue unit. A nil done channel means the
// caller only requested asynchronous execution through Enqueue; synchronous
// ledger writes attach a buffered completion channel so they can surface
// database failures without allowing concurrent writers to race.
type writeTask struct {
	ctx     context.Context
	release func()
	op      func(context.Context, *sql.DB) error
	done    chan error
}

func OpenFromEnv(ctx context.Context) (*Storage, error) {
	mode := os.Getenv("SILENCER_STORAGE_MODE")
	if mode == "" {
		mode = "sqlite"
	}
	var drv, dsn string
	if mode == "mysql" {
		drv = "mysql"
		dsn = os.Getenv("SILENCER_MYSQL_DSN")
		if dsn == "" {
			return nil, fmt.Errorf("SILENCER_MYSQL_DSN required")
		}
	} else if mode == "sqlite" {
		drv = "sqlite3"
		dsn = os.Getenv("SILENCER_SQLITE_PATH")
		if dsn == "" {
			if _, err := os.Stat("/data"); err == nil {
				dsn = "/data/silencer.db"
			} else {
				dsn = "silencer.db"
			}
		}
	} else {
		return nil, fmt.Errorf("unsupported SILENCER_STORAGE_MODE %q", mode)
	}
	db, e := sql.Open(drv, dsn)
	if e != nil {
		return nil, e
	}
	if mode == "sqlite" {
		// The writer is serialized by the bounded queue, while reads (ledger
		// polling, health checks, and the management UI) should not wait behind
		// that writer. WAL keeps readers from blocking a commit; the busy timeout
		// covers the short lock window during checkpoints and startup DDL.
		if err := configureSQLite(ctx, db, dsn); err != nil {
			_ = db.Close()
			return nil, err
		}
		if sqliteMemoryDSN(dsn) {
			// Each :memory: connection is a separate database unless callers use a
			// shared URI. Keep one connection for the simple in-memory test mode.
			db.SetMaxOpenConns(1)
			db.SetMaxIdleConns(1)
		} else {
			db.SetMaxOpenConns(4)
			db.SetMaxIdleConns(4)
		}
	} else {
		poolSize, err := positiveEnvInt("SILENCER_MYSQL_POOL_SIZE", 32)
		if err != nil {
			_ = db.Close()
			return nil, err
		}
		idleSize, err := positiveEnvInt("SILENCER_MYSQL_MAX_IDLE_CONNS", minInt(8, poolSize))
		if err != nil {
			_ = db.Close()
			return nil, err
		}
		if idleSize > poolSize {
			idleSize = poolSize
		}
		db.SetMaxOpenConns(poolSize)
		db.SetMaxIdleConns(idleSize)
	}
	writeCtx, cancel := context.WithCancel(context.Background())
	s := &Storage{db: db, mode: mode, queue: make(chan writeTask, WriterQueueCapacity), writeCtx: writeCtx, cancel: cancel}
	if e = db.PingContext(ctx); e != nil {
		db.Close()
		return nil, e
	}
	if e = s.init(ctx); e != nil {
		db.Close()
		return nil, e
	}
	if e = s.migrateEntityMappings(ctx); e != nil {
		cancel()
		db.Close()
		return nil, fmt.Errorf("migrate entity mappings: %w", e)
	}
	s.wg.Add(1)
	go s.writer()
	return s, nil
}

func sqliteMemoryDSN(dsn string) bool {
	lower := strings.ToLower(strings.TrimSpace(dsn))
	return lower == ":memory:" || strings.Contains(lower, "mode=memory")
}

func configureSQLite(ctx context.Context, db *sql.DB, dsn string) error {
	timeout, err := positiveEnvInt("SILENCER_SQLITE_BUSY_TIMEOUT", 5000)
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout=%d", timeout)); err != nil {
		return fmt.Errorf("configure sqlite busy timeout: %w", err)
	}
	if sqliteMemoryDSN(dsn) {
		return nil
	}
	if _, err := db.ExecContext(ctx, "PRAGMA journal_mode=WAL"); err != nil {
		return fmt.Errorf("configure sqlite journal mode: %w", err)
	}
	if _, err := db.ExecContext(ctx, "PRAGMA synchronous=NORMAL"); err != nil {
		return fmt.Errorf("configure sqlite synchronous mode: %w", err)
	}
	return nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func positiveEnvInt(name string, fallback int) (int, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 1 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return parsed, nil
}
func (s *Storage) init(ctx context.Context) error {
	blob := "BLOB"
	if s.mode == "mysql" {
		// MySQL BLOB is limited to 64 KiB, while the gateway accepts bodies up
		// to 64 MiB by default. Keep payload columns large enough for the
		// configured request/response limit.
		blob = "LONGBLOB"
	}
	statements := []string{
		// Always specify an explicit default for required timestamps. MySQL's
		// implicit TIMESTAMP default is a zero date under some 5.7 modes and is
		// rejected by strict NO_ZERO_DATE settings (and is not portable to 8.x).
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS requests (id VARCHAR(128) PRIMARY KEY,payload %s NOT NULL,raw %s,masked %s,upstream %s,restored %s,status VARCHAR(64),error TEXT,meta TEXT,started_at TIMESTAMP NULL,ended_at TIMESTAMP NULL,created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP)`, blob, blob, blob, blob, blob),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS events (id VARCHAR(128) PRIMARY KEY,request_id VARCHAR(128) NOT NULL,event_type VARCHAR(128) NOT NULL,payload %s NOT NULL,raw %s,masked %s,upstream %s,restored %s,status VARCHAR(64),error TEXT,meta TEXT,started_at TIMESTAMP NULL,ended_at TIMESTAMP NULL,created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP)`, blob, blob, blob, blob, blob),
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	if err := s.initEntityMappings(ctx, blob); err != nil {
		return err
	}
	if err := rejectLegacySchema(ctx, s.db, s.mode); err != nil {
		return err
	}
	// SQLite supports CREATE INDEX IF NOT EXISTS; MySQL does not. Use
	// dialect-specific idempotent index creation so startup works on both.
	indexStatements := []string{
		`CREATE INDEX IF NOT EXISTS idx_requests_created_at ON requests(created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_events_request_created_at ON events(request_id,created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_events_created_at ON events(created_at)`,
	}
	if s.mode == "mysql" {
		indexStatements = []string{
			`CREATE INDEX idx_requests_created_at ON requests(created_at)`,
			`CREATE INDEX idx_events_request_created_at ON events(request_id,created_at)`,
			`CREATE INDEX idx_events_created_at ON events(created_at)`,
		}
	}
	for _, statement := range indexStatements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil && !isAlreadyExistsError(err) {
			return fmt.Errorf("create index: %w", err)
		}
	}
	// Additive migrations for databases created by older versions.
	for _, table := range []string{"requests", "events"} {
		for _, col := range []string{fmt.Sprintf("raw %s", blob), fmt.Sprintf("masked %s", blob), fmt.Sprintf("upstream %s", blob), fmt.Sprintf("restored %s", blob), "status VARCHAR(64)", "error TEXT", "meta TEXT", "started_at TIMESTAMP NULL", "ended_at TIMESTAMP NULL"} {
			if _, err := s.db.ExecContext(ctx, "ALTER TABLE "+table+" ADD COLUMN "+col); err != nil && !isAlreadyExistsError(err) {
				return fmt.Errorf("migrate %s: %w", table, err)
			}
		}
	}
	if s.mode == "mysql" {
		// Existing installations may have been created with BLOB columns by an
		// earlier rewrite build. Upgrade all payload columns in place so large
		// bodies do not fail only after deployment.
		for _, table := range []string{"requests", "events"} {
			columns := []string{"payload", "raw", "masked", "upstream", "restored"}
			for _, column := range columns {
				nullability := " NULL"
				if column == "payload" {
					nullability = " NOT NULL"
				}
				statement := fmt.Sprintf("ALTER TABLE %s MODIFY COLUMN %s LONGBLOB%s", table, column, nullability)
				if _, err := s.db.ExecContext(ctx, statement); err != nil && !isAlreadyExistsError(err) {
					return fmt.Errorf("migrate %s.%s: %w", table, column, err)
				}
			}
			// CREATE TABLE IF NOT EXISTS does not repair a pre-existing column.
			// Normalize the timestamp default for databases created by an earlier
			// build so strict MySQL modes can start successfully after upgrade.
			statement := fmt.Sprintf("ALTER TABLE %s MODIFY COLUMN created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP", table)
			if _, err := s.db.ExecContext(ctx, statement); err != nil && !isAlreadyExistsError(err) {
				return fmt.Errorf("migrate %s.created_at: %w", table, err)
			}
		}
	}
	// Record the schema revision after all idempotent DDL has succeeded. Older
	// installations have no migration table; their existing schema is treated
	// as revision 1 because this revision contains only additive changes.
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	if s.mode == "mysql" {
		if _, err := s.db.ExecContext(ctx, `ALTER TABLE schema_migrations MODIFY COLUMN applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP`); err != nil && !isAlreadyExistsError(err) {
			return fmt.Errorf("migrate schema_migrations.applied_at: %w", err)
		}
	}
	var version int
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version < LatestSchemaVersion {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO schema_migrations(version,applied_at) VALUES(?,?)`, LatestSchemaVersion, time.Now().UTC()); err != nil && !isAlreadyExistsError(err) {
			return fmt.Errorf("record schema version: %w", err)
		}
	}
	return nil
}

func rejectLegacySchema(ctx context.Context, db *sql.DB, mode string) error {
	requests, hasRequests, err := tableColumns(ctx, db, mode, "requests")
	if err != nil {
		return fmt.Errorf("inspect requests schema: %w", err)
	}
	if hasRequests && !hasColumn(requests, "id") && hasColumn(requests, "request_id") {
		return fmt.Errorf("legacy requests schema detected; copy it into a new database with silencer-migrate -copy")
	}
	events, hasEvents, err := tableColumns(ctx, db, mode, "events")
	if err != nil {
		return fmt.Errorf("inspect events schema: %w", err)
	}
	if hasEvents && !hasColumn(events, "event_type") && hasColumn(events, "type") {
		return fmt.Errorf("legacy events schema detected; copy it into a new database with silencer-migrate -copy")
	}
	return nil
}

// SchemaVersion returns the highest applied migration revision.
func (s *Storage) SchemaVersion(ctx context.Context) (int, error) {
	var version int
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&version)
	return version, err
}

func nullableTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value
}

// isAlreadyExistsError identifies idempotent DDL failures while preserving
// real migration errors. SQLite reports duplicate columns/indexes as text;
// MySQL exposes stable numeric error codes through MySQLError.
func isAlreadyExistsError(err error) bool {
	if err == nil {
		return false
	}
	var myErr *mysql.MySQLError
	if errors.As(err, &myErr) {
		return myErr.Number == 1060 || myErr.Number == 1061 || myErr.Number == 1050
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "duplicate column") ||
		strings.Contains(msg, "duplicate key name") ||
		strings.Contains(msg, "already exists")
}

func isMissingTableError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "no such table") ||
		strings.Contains(message, "doesn't exist") ||
		strings.Contains(message, "does not exist")
}
func (s *Storage) writer() {
	defer s.wg.Done()
	for task := range s.queue {
		err := s.runWriteTask(task)
		if err != nil {
			s.errMu.Lock()
			s.lastErr = err
			s.errMu.Unlock()
		}
		if task.done != nil {
			task.done <- err
			close(task.done)
		}
		if task.release != nil {
			task.release()
		}
	}
}

func (s *Storage) runWriteTask(task writeTask) (err error) {
	if task.op == nil {
		return errors.New("storage writer received nil operation")
	}
	if task.ctx != nil {
		select {
		case <-task.ctx.Done():
			return task.ctx.Err()
		default:
		}
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("storage writer panic: %v", recovered)
		}
	}()
	return task.op(task.ctx, s.db)
}

// taskContext combines the caller's cancellation with the storage lifecycle.
// A shutdown therefore interrupts queued and in-flight database operations,
// while a request timeout prevents a queued write from running later.
func (s *Storage) taskContext(ctx context.Context, cancelOnClose bool) (context.Context, func()) {
	if ctx == nil {
		ctx = context.Background()
	}
	merged, cancel := context.WithCancel(ctx)
	var stop func() bool
	if cancelOnClose && s.writeCtx != nil {
		stop = context.AfterFunc(s.writeCtx, cancel)
	}
	return merged, func() {
		if stop != nil {
			stop()
		}
		cancel()
	}
}
func (s *Storage) enqueue(task writeTask) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return fmt.Errorf("storage is closed")
	}
	select {
	case s.queue <- task:
		return nil
	default:
		return fmt.Errorf("storage queue full")
	}
}
func (s *Storage) enqueueAndWait(ctx context.Context, op func(context.Context, *sql.DB) error) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	done := make(chan error, 1)
	taskCtx, release := s.taskContext(ctx, true)
	task := writeTask{ctx: taskCtx, release: release, done: done, op: op}
	if err := s.enqueue(task); err != nil {
		release()
		return err
	}
	// Once queued, return the writer's definitive result. Cancellation still
	// reaches the SQL operation through taskCtx, but cannot disguise a commit
	// as failure and cause the caller to discard its retained body files.
	return <-done
}
func (s *Storage) AppendRequest(ctx context.Context, r Request) error {
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now()
	}
	if r.StartedAt.IsZero() {
		r.StartedAt = r.CreatedAt
	}
	if r.Payload == nil {
		r.Payload = []byte{}
	}
	return s.enqueueAndWait(ctx, func(taskCtx context.Context, db *sql.DB) error {
		_, err := db.ExecContext(taskCtx, `INSERT INTO requests(id,payload,raw,masked,upstream,restored,status,error,meta,started_at,ended_at,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, r.ID, r.Payload, r.Raw, r.Masked, r.Upstream, r.Restored, r.Status, r.Error, r.Meta, nullableTime(r.StartedAt), nullableTime(r.EndedAt), r.CreatedAt)
		return err
	})
}

// UpdateRequest stores the terminal state and response material for a request.
// It is synchronous so callers can surface durable-write failures.
func (s *Storage) UpdateRequest(ctx context.Context, r Request) error {
	var affected int64
	err := s.enqueueAndWait(ctx, func(taskCtx context.Context, db *sql.DB) error {
		res, err := db.ExecContext(taskCtx, `UPDATE requests SET payload=?,raw=?,masked=?,upstream=?,restored=?,status=?,error=?,meta=?,started_at=?,ended_at=?,created_at=? WHERE id=?`, r.Payload, r.Raw, r.Masked, r.Upstream, r.Restored, r.Status, r.Error, r.Meta, nullableTime(r.StartedAt), nullableTime(r.EndedAt), r.CreatedAt, r.ID)
		if err != nil {
			return err
		}
		affected, err = res.RowsAffected()
		return err
	})
	if err != nil {
		return err
	}
	if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}
func (s *Storage) AppendEvent(ctx context.Context, e Event) error {
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now()
	}
	if e.Payload == nil {
		e.Payload = []byte{}
	}
	return s.enqueueAndWait(ctx, func(taskCtx context.Context, db *sql.DB) error {
		_, err := db.ExecContext(taskCtx, `INSERT INTO events(id,request_id,event_type,payload,raw,masked,upstream,restored,status,error,meta,started_at,ended_at,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, e.ID, e.RequestID, e.Type, e.Payload, e.Raw, e.Masked, e.Upstream, e.Restored, e.Status, e.Error, e.Meta, nullableTime(e.StartedAt), nullableTime(e.EndedAt), e.CreatedAt)
		return err
	})
}

// DeleteAuditEvents removes only events produced by the audit pipeline.
// Operational request events remain available in the ledger.
func (s *Storage) DeleteAuditEvents(ctx context.Context) error {
	return s.enqueueAndWait(ctx, func(taskCtx context.Context, db *sql.DB) error {
		_, err := db.ExecContext(taskCtx, `DELETE FROM events WHERE SUBSTR(event_type,1,6) = 'AUDIT_'`)
		return err
	})
}

func (s *Storage) ListRequests(ctx context.Context, limit int) ([]Request, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, e := s.db.QueryContext(ctx, `SELECT id,payload,raw,masked,upstream,restored,status,error,meta,started_at,ended_at,created_at FROM requests ORDER BY created_at LIMIT ?`, limit)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []Request
	for rows.Next() {
		var r Request
		var status, failure, meta sql.NullString
		var started, ended sql.NullTime
		if e := rows.Scan(&r.ID, &r.Payload, &r.Raw, &r.Masked, &r.Upstream, &r.Restored, &status, &failure, &meta, &started, &ended, &r.CreatedAt); e != nil {
			return nil, e
		}
		r.Status, r.Error, r.Meta = status.String, failure.String, meta.String
		if started.Valid {
			r.StartedAt = started.Time
		}
		if ended.Valid {
			r.EndedAt = ended.Time
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetRequest fetches one ledger row by its stable request ID. Detail views
// must not scan a bounded list: older rows remain addressable after the
// ledger grows beyond the list page size.
func (s *Storage) GetRequest(ctx context.Context, id string) (Request, error) {
	var r Request
	var status, failure, meta sql.NullString
	var started, ended sql.NullTime
	err := s.db.QueryRowContext(ctx, `SELECT id,payload,raw,masked,upstream,restored,status,error,meta,started_at,ended_at,created_at FROM requests WHERE id=?`, id).
		Scan(&r.ID, &r.Payload, &r.Raw, &r.Masked, &r.Upstream, &r.Restored, &status, &failure, &meta, &started, &ended, &r.CreatedAt)
	r.Status, r.Error, r.Meta = status.String, failure.String, meta.String
	if started.Valid {
		r.StartedAt = started.Time
	}
	if ended.Valid {
		r.EndedAt = ended.Time
	}
	return r, err
}

func (s *Storage) ListEvents(ctx context.Context, requestID string, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, e := s.db.QueryContext(ctx, `SELECT id,request_id,event_type,payload,raw,masked,upstream,restored,status,error,meta,started_at,ended_at,created_at FROM events WHERE request_id=? ORDER BY created_at LIMIT ?`, requestID, limit)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var v Event
		var status, failure, meta sql.NullString
		var started, ended sql.NullTime
		if e := rows.Scan(&v.ID, &v.RequestID, &v.Type, &v.Payload, &v.Raw, &v.Masked, &v.Upstream, &v.Restored, &status, &failure, &meta, &started, &ended, &v.CreatedAt); e != nil {
			return nil, e
		}
		v.Status, v.Error, v.Meta = status.String, failure.String, meta.String
		if started.Valid {
			v.StartedAt = started.Time
		}
		if ended.Valid {
			v.EndedAt = ended.Time
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Storage) ListAllEvents(ctx context.Context, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,request_id,event_type,payload,raw,masked,upstream,restored,status,error,meta,started_at,ended_at,created_at FROM events ORDER BY created_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var event Event
		var status, failure, meta sql.NullString
		var started, ended sql.NullTime
		if err := rows.Scan(&event.ID, &event.RequestID, &event.Type, &event.Payload, &event.Raw, &event.Masked, &event.Upstream, &event.Restored, &status, &failure, &meta, &started, &ended, &event.CreatedAt); err != nil {
			return nil, err
		}
		event.Status, event.Error, event.Meta = status.String, failure.String, meta.String
		if started.Valid {
			event.StartedAt = started.Time
		}
		if ended.Valid {
			event.EndedAt = ended.Time
		}
		out = append(out, event)
	}
	return out, rows.Err()
}

// ListAuditEvents returns only events emitted by the audit pipeline. Keeping
// this filter in SQL prevents high-volume MASK/RESTORE operational events
// from crowding real audit findings out of the audit window.
func (s *Storage) ListAuditEvents(ctx context.Context, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,request_id,event_type,payload,raw,masked,upstream,restored,status,error,meta,started_at,ended_at,created_at FROM events WHERE event_type = 'AUDIT' OR SUBSTR(event_type,1,6) = 'AUDIT_' ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var event Event
		var status, failure, meta sql.NullString
		var started, ended sql.NullTime
		if err := rows.Scan(&event.ID, &event.RequestID, &event.Type, &event.Payload, &event.Raw, &event.Masked, &event.Upstream, &event.Restored, &status, &failure, &meta, &started, &ended, &event.CreatedAt); err != nil {
			return nil, err
		}
		event.Status, event.Error, event.Meta = status.String, failure.String, meta.String
		if started.Valid {
			event.StartedAt = started.Time
		}
		if ended.Valid {
			event.EndedAt = ended.Time
		}
		out = append(out, event)
	}
	return out, rows.Err()
}

func (s *Storage) GetEvent(ctx context.Context, id string) (Event, error) {
	var e Event
	var status, failure, meta sql.NullString
	var started, ended sql.NullTime
	err := s.db.QueryRowContext(ctx, `SELECT id,request_id,event_type,payload,raw,masked,upstream,restored,status,error,meta,started_at,ended_at,created_at FROM events WHERE id=?`, id).Scan(&e.ID, &e.RequestID, &e.Type, &e.Payload, &e.Raw, &e.Masked, &e.Upstream, &e.Restored, &status, &failure, &meta, &started, &ended, &e.CreatedAt)
	e.Status, e.Error, e.Meta = status.String, failure.String, meta.String
	if started.Valid {
		e.StartedAt = started.Time
	}
	if ended.Valid {
		e.EndedAt = ended.Time
	}
	return e, err
}
func (s *Storage) DeleteEvents(ctx context.Context) error {
	return s.enqueueAndWait(ctx, func(taskCtx context.Context, db *sql.DB) error {
		_, err := db.ExecContext(taskCtx, `DELETE FROM events`)
		return err
	})
}

func (s *Storage) CountRequests(ctx context.Context) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM requests`).Scan(&count)
	return count, err
}
func (s *Storage) Health(ctx context.Context) error { return s.db.PingContext(ctx) }

// Snapshot returns storage state suitable for health endpoints and metrics.
// DBStats is provided by database/sql and is safe to read concurrently with
// queries; queue length is an instantaneous backpressure signal.
func (s *Storage) Snapshot() Snapshot {
	s.errMu.RLock()
	lastWriteErr := s.lastErr != nil
	s.errMu.RUnlock()
	s.mu.RLock()
	depth := len(s.queue)
	capacity := cap(s.queue)
	s.mu.RUnlock()
	return Snapshot{Mode: s.mode, QueueDepth: depth, QueueCapacity: capacity, LastWriteErr: lastWriteErr, DB: s.db.Stats()}
}

// MetricsText exposes bounded storage telemetry in Prometheus text format.
// Error text is deliberately omitted: database driver errors can contain
// topology or credential details and are not safe on the public metrics route.
func (s *Storage) MetricsText() string {
	snapshot := s.Snapshot()
	lastErr := 0
	if snapshot.LastWriteErr {
		lastErr = 1
	}
	return fmt.Sprintf(
		"# HELP silencer_storage_queue_depth Pending database writer operations.\n"+
			"# TYPE silencer_storage_queue_depth gauge\n"+
			"silencer_storage_queue_depth %d\n"+
			"# HELP silencer_storage_queue_capacity Database writer queue capacity.\n"+
			"# TYPE silencer_storage_queue_capacity gauge\n"+
			"silencer_storage_queue_capacity %d\n"+
			"# HELP silencer_storage_last_write_error Whether a database writer error has occurred.\n"+
			"# TYPE silencer_storage_last_write_error gauge\n"+"silencer_storage_last_write_error %d\n"+
			"# HELP silencer_storage_open_connections Open database connections.\n"+"# TYPE silencer_storage_open_connections gauge\n"+"silencer_storage_open_connections %d\n"+"# HELP silencer_storage_in_use_connections Database connections currently in use.\n"+"# TYPE silencer_storage_in_use_connections gauge\n"+"silencer_storage_in_use_connections %d\n"+"# HELP silencer_storage_idle_connections Idle database connections.\n"+"# TYPE silencer_storage_idle_connections gauge\n"+"silencer_storage_idle_connections %d\n"+"# HELP silencer_storage_wait_count_total Total waits for a database connection.\n"+"# TYPE silencer_storage_wait_count_total counter\n"+"silencer_storage_wait_count_total %d\n",
		snapshot.QueueDepth, snapshot.QueueCapacity, lastErr,
		snapshot.DB.OpenConnections, snapshot.DB.InUse, snapshot.DB.Idle,
		snapshot.DB.WaitCount,
	)
}
func (s *Storage) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	// Enqueue takes the read lock while sending, so closing the queue under the
	// write lock makes the close/enqueue boundary race-free. The writer ranges
	// over the channel and drains every accepted operation before returning.
	s.closed = true
	close(s.queue)
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()
	s.wg.Wait()
	return s.db.Close()
}

// Enqueue schedules a database operation on the bounded writer queue.
func (s *Storage) Enqueue(op func(*sql.DB) error) error {
	if op == nil {
		return fmt.Errorf("nil operation")
	}
	taskCtx, release := s.taskContext(context.Background(), false)
	err := s.enqueue(writeTask{ctx: taskCtx, release: release, op: func(_ context.Context, db *sql.DB) error {
		return op(db)
	}})
	if err != nil {
		release()
	}
	return err
}

func (s *Storage) LastError() error { s.errMu.RLock(); defer s.errMu.RUnlock(); return s.lastErr }

// Stats returns requests/events within time range.
func (s *Storage) Stats(ctx context.Context, since time.Time) (requests []Request, events []Event, err error) {
	requests, err = s.listRequestsSince(ctx, since)
	if err != nil {
		return
	}
	events, err = s.listEventsSince(ctx, since)
	return
}
func (s *Storage) DashboardStats(ctx context.Context, since time.Time) ([]Request, []Event, error) {
	return s.compactStats(ctx, since)
}
func (s *Storage) listRequestsSince(ctx context.Context, since time.Time) ([]Request, error) {
	rows, e := s.db.QueryContext(ctx, `SELECT id,payload,raw,masked,upstream,restored,status,error,meta,started_at,ended_at,created_at FROM requests WHERE created_at>=? ORDER BY created_at ASC, id ASC`, since)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []Request
	for rows.Next() {
		var r Request
		var st, ed sql.NullTime
		var status, failure, meta sql.NullString
		if e := rows.Scan(&r.ID, &r.Payload, &r.Raw, &r.Masked, &r.Upstream, &r.Restored, &status, &failure, &meta, &st, &ed, &r.CreatedAt); e != nil {
			return nil, e
		}
		r.Status, r.Error, r.Meta = status.String, failure.String, meta.String
		if st.Valid {
			r.StartedAt = st.Time
		}
		if ed.Valid {
			r.EndedAt = ed.Time
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func (s *Storage) listEventsSince(ctx context.Context, since time.Time) ([]Event, error) {
	rows, e := s.db.QueryContext(ctx, `SELECT id,request_id,event_type,payload,raw,masked,upstream,restored,status,error,meta,started_at,ended_at,created_at FROM events WHERE created_at>=? ORDER BY created_at ASC, id ASC`, since)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var v Event
		var st, ed sql.NullTime
		var status, failure, meta sql.NullString
		if e := rows.Scan(&v.ID, &v.RequestID, &v.Type, &v.Payload, &v.Raw, &v.Masked, &v.Upstream, &v.Restored, &status, &failure, &meta, &st, &ed, &v.CreatedAt); e != nil {
			return nil, e
		}
		v.Status, v.Error, v.Meta = status.String, failure.String, meta.String
		if st.Valid {
			v.StartedAt = st.Time
		}
		if ed.Valid {
			v.EndedAt = ed.Time
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
