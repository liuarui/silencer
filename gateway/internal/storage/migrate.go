package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// DatabaseConfig identifies a source or destination database for the explicit
// migration command. The runtime still reads its normal SILENCER_* variables.
type DatabaseConfig struct {
	Mode       string
	SQLitePath string
	MySQLDSN   string
}

// OpenDatabase opens a standalone database connection for migration. It does
// not create tables or mutate data.
func OpenDatabase(ctx context.Context, cfg DatabaseConfig) (*sql.DB, error) {
	mode := strings.ToLower(strings.TrimSpace(cfg.Mode))
	if mode == "" {
		mode = "sqlite"
	}
	driver, dsn := "sqlite3", cfg.SQLitePath
	if mode == "mysql" {
		driver, dsn = "mysql", cfg.MySQLDSN
	} else if mode != "sqlite" {
		return nil, fmt.Errorf("unsupported database mode %q", cfg.Mode)
	}
	if dsn == "" {
		if mode == "sqlite" {
			dsn = "silencer.db"
		} else {
			return nil, fmt.Errorf("MySQL DSN is required")
		}
	}
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, err
	}
	if mode == "sqlite" {
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
	} else {
		poolSize, poolErr := positiveEnvInt("SILENCER_MYSQL_POOL_SIZE", 32)
		if poolErr != nil {
			_ = db.Close()
			return nil, poolErr
		}
		idleSize, idleErr := positiveEnvInt("SILENCER_MYSQL_MAX_IDLE_CONNS", minInt(8, poolSize))
		if idleErr != nil {
			_ = db.Close()
			return nil, idleErr
		}
		if idleSize > poolSize {
			idleSize = poolSize
		}
		db.SetMaxOpenConns(poolSize)
		db.SetMaxIdleConns(idleSize)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// CopyOptions controls a source-to-runtime copy. Existing target rows are
// updated by primary key, so rerunning a migration is idempotent.
type CopyOptions struct {
	SourceMode string
	BatchSize  int
	Verify     bool
}

type CopyReport struct {
	EntityMappingsCopied   int    `json:"entity_mappings_copied"`
	EntityMappingsVerified int    `json:"entity_mappings_verified"`
	RequestsSource         int    `json:"requests_source"`
	RequestsCopied         int    `json:"requests_copied"`
	RequestsVerified       int    `json:"requests_verified"`
	EventsSource           int    `json:"events_source"`
	EventsCopied           int    `json:"events_copied"`
	EventsVerified         int    `json:"events_verified"`
	AuditEventsSource      int    `json:"audit_events_source"`
	AuditEventsCopied      int    `json:"audit_events_copied"`
	AuditEventsVerified    int    `json:"audit_events_verified"`
	RequestsSHA256         string `json:"requests_sha256"`
	EventsSHA256           string `json:"events_sha256"`
	AuditSHA256            string `json:"audit_sha256"`
	Verified               bool   `json:"verified"`
}

type migrationRow map[string]any

// CopyFrom imports both the current Go schema and the legacy Python schema.
// Every binary/text payload is copied byte-for-byte. Runtime capture limits
// apply to new requests, but must not silently corrupt an existing database
// during migration.
func (s *Storage) CopyFrom(ctx context.Context, source *sql.DB, opts CopyOptions) (CopyReport, error) {
	if source == nil {
		return CopyReport{}, fmt.Errorf("source database is nil")
	}
	if opts.BatchSize < 1 {
		opts.BatchSize = 500
	}
	if opts.SourceMode == "" {
		opts.SourceMode = "sqlite"
	}
	var report CopyReport
	requests, err := copyTableRows(ctx, source, s.db, s.mode, opts.SourceMode, "requests", opts, &report)
	if err != nil {
		return report, err
	}
	_ = requests
	if _, err := copyTableRows(ctx, source, s.db, s.mode, opts.SourceMode, "events", opts, &report); err != nil {
		return report, err
	}
	if _, err := copyTableRows(ctx, source, s.db, s.mode, opts.SourceMode, "audit_events", opts, &report); err != nil {
		return report, err
	}
	if err := s.copyEntityMappings(ctx, source, opts, &report); err != nil {
		return report, err
	}
	if err := s.backfillEntityMappings(ctx); err != nil {
		return report, err
	}
	if opts.Verify {
		if err := s.verifyCopiedEntityMappings(ctx, source, &report); err != nil {
			return report, err
		}
	}
	report.Verified = !opts.Verify || (report.RequestsVerified == report.RequestsCopied && report.EventsVerified == report.EventsCopied && report.AuditEventsVerified == report.AuditEventsCopied && report.EntityMappingsCopied == report.EntityMappingsVerified)
	return report, nil
}

func copyTableRows(ctx context.Context, source, target *sql.DB, targetMode, sourceMode, table string, opts CopyOptions, report *CopyReport) (int, error) {
	columns, exists, err := tableColumns(ctx, source, sourceMode, table)
	if err != nil {
		return 0, err
	}
	if !exists {
		return 0, nil
	}
	order := "id"
	if table == "requests" && !hasColumn(columns, "id") {
		order = "request_id"
	}
	query := fmt.Sprintf("SELECT * FROM %s ORDER BY %s", table, order)
	rows, err := source.QueryContext(ctx, query)
	if err != nil {
		return 0, fmt.Errorf("read source %s: %w", table, err)
	}
	defer rows.Close()
	hash := sha256.New()
	count := 0
	tx, err := target.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("start target %s transaction: %w", table, err)
	}
	pendingRequests := make([]requestRecord, 0, opts.BatchSize)
	pendingEvents := make([]eventRecord, 0, opts.BatchSize)
	flush := func() error {
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit target %s transaction: %w", table, err)
		}
		if opts.Verify {
			if table == "requests" {
				for _, record := range pendingRequests {
					if verifyRequest(ctx, target, record) {
						report.RequestsVerified++
					}
				}
			} else {
				for _, record := range pendingEvents {
					if verifyEvent(ctx, target, record) {
						if table == "audit_events" {
							report.AuditEventsVerified++
						} else {
							report.EventsVerified++
						}
					}
				}
			}
		}
		pendingRequests = pendingRequests[:0]
		pendingEvents = pendingEvents[:0]
		var beginErr error
		tx, beginErr = target.BeginTx(ctx, nil)
		return beginErr
	}
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			_ = tx.Rollback()
			return count, fmt.Errorf("scan source %s: %w", table, err)
		}
		row := make(migrationRow, len(columns))
		for i, column := range columns {
			row[strings.ToLower(column)] = values[i]
		}
		if table == "requests" {
			record, ok := normalizeRequest(row)
			if !ok {
				_ = tx.Rollback()
				return count, fmt.Errorf("source requests row %d has no request id", count)
			}
			if err := upsertRequest(ctx, tx, targetMode, record); err != nil {
				_ = tx.Rollback()
				return count, err
			}
			report.RequestsSource++
			report.RequestsCopied++
			writeRequestDigest(hash, record)
			pendingRequests = append(pendingRequests, record)
			if len(pendingRequests) >= opts.BatchSize {
				if err := flush(); err != nil {
					return count, err
				}
			}
		} else {
			audit := table == "audit_events"
			record, ok := normalizeEvent(row, audit)
			if !ok {
				_ = tx.Rollback()
				return count, fmt.Errorf("source %s row %d has no event id", table, count)
			}
			if err := upsertEvent(ctx, tx, targetMode, record); err != nil {
				_ = tx.Rollback()
				return count, err
			}
			if audit {
				report.AuditEventsSource++
				report.AuditEventsCopied++
			} else {
				report.EventsSource++
				report.EventsCopied++
			}
			writeEventDigest(hash, record)
			pendingEvents = append(pendingEvents, record)
			if len(pendingEvents) >= opts.BatchSize {
				if err := flush(); err != nil {
					return count, err
				}
			}
		}
		count++
	}
	if err := rows.Err(); err != nil {
		_ = tx.Rollback()
		return count, fmt.Errorf("iterate source %s: %w", table, err)
	}
	if err := flush(); err != nil {
		return count, err
	}
	// flush starts the next batch transaction for reuse; no rows remain, so
	// close that empty transaction before returning to release the connection.
	_ = tx.Rollback()
	digest := hex.EncodeToString(hash.Sum(nil))
	if table == "requests" {
		report.RequestsSHA256 = digest
	} else if table == "audit_events" {
		report.AuditSHA256 = digest
	} else {
		report.EventsSHA256 = digest
	}
	return count, nil
}

type requestRecord struct {
	id                                       string
	payload, raw, masked, upstream, restored []byte
	status, failure, meta                    string
	started, ended, created                  time.Time
}

type eventRecord struct {
	id, requestID, typ                       string
	payload, raw, masked, upstream, restored []byte
	status, failure, meta                    string
	started, ended, created                  time.Time
}

func normalizeRequest(row migrationRow) (requestRecord, bool) {
	id := text(row["id"])
	if id == "" {
		id = text(row["request_id"])
	}
	if id == "" {
		return requestRecord{}, false
	}
	record := requestRecord{id: id, payload: migrationBytesValue(row["payload"]), raw: migrationBytesValue(row["raw"]), masked: migrationBytesValue(row["masked"]), upstream: migrationBytesValue(row["upstream"]), restored: migrationBytesValue(row["restored"]), status: text(row["status"]), failure: text(row["error"]), meta: text(row["meta"])}
	// The legacy request ledger used `upstream` for the upstream URL and
	// `upstream_response` for body bytes. Prefer the explicit response columns
	// whenever they exist, even when the URL is non-empty.
	if value, exists := row["upstream_response"]; exists {
		record.upstream = migrationBytesValue(value)
	}
	if value, exists := row["restored_response"]; exists {
		record.restored = migrationBytesValue(value)
	}
	if record.meta == "" {
		meta := map[string]any{}
		for _, key := range []string{"client", "upstream", "model", "path", "body_encoding", "truncated", "body_unavailable"} {
			if value, ok := row[key]; ok && value != nil {
				meta[key] = value
			}
		}
		if data, err := json.Marshal(meta); err == nil && len(meta) > 0 {
			record.meta = string(data)
		}
	}
	record.started = timeValue(row["started_at"])
	if record.started.IsZero() {
		record.started = timeValue(row["started"])
	}
	record.ended = timeValue(row["ended_at"])
	if record.ended.IsZero() {
		record.ended = timeValue(row["ended"])
	}
	record.created = timeValue(row["created_at"])
	if record.created.IsZero() {
		record.created = record.started
	}
	if record.created.IsZero() {
		record.created = time.Unix(0, 0).UTC()
	}
	if record.payload == nil {
		record.payload = []byte{}
	}
	return record, true
}

func normalizeEvent(row migrationRow, legacyAudit bool) (eventRecord, bool) {
	rawID := text(row["id"])
	if rawID == "" {
		return eventRecord{}, false
	}
	id := rawID
	if legacyAudit {
		id = "legacy-audit-" + rawID
	} else if _, legacy := row["event_type"]; !legacy && row["request_id"] == nil && row["ts"] != nil {
		id = "legacy-event-" + rawID
	}
	record := eventRecord{id: id, requestID: text(row["request_id"]), typ: text(row["event_type"]), payload: migrationBytesValue(row["payload"]), raw: migrationBytesValue(row["raw"]), masked: migrationBytesValue(row["masked"]), upstream: migrationBytesValue(row["upstream"]), restored: migrationBytesValue(row["restored"]), status: text(row["status"]), failure: text(row["error"]), meta: text(row["meta"])}
	if legacyAudit {
		record.payload = migrationBytesValue(row["evidence"])
		record.typ = text(row["signal_type"])
		if record.typ == "" {
			record.typ = "AUDIT"
		} else {
			record.typ = "AUDIT_" + record.typ
		}
	}
	if record.requestID == "" {
		record.requestID = text(row["sid"])
	}
	if record.requestID == "" {
		record.requestID = "legacy"
		if legacyAudit {
			record.requestID = "legacy-audit"
		}
	}
	if record.typ == "" {
		record.typ = text(row["type"])
	}
	if record.typ == "" {
		record.typ = "UNKNOWN"
	}
	if record.meta == "" {
		meta := map[string]any{}
		for _, key := range []string{"host", "method", "path", "count", "restored", "http_status", "ingress", "evidence", "severity", "signal_type", "probe_id"} {
			if value, ok := row[key]; ok && value != nil {
				meta[key] = value
			}
		}
		if data, err := json.Marshal(meta); err == nil && len(meta) > 0 {
			record.meta = string(data)
		}
	}
	record.started = timeValue(row["started_at"])
	record.ended = timeValue(row["ended_at"])
	record.created = timeValue(row["created_at"])
	if record.created.IsZero() {
		record.created = timeValue(row["ts"])
	}
	if record.created.IsZero() {
		record.created = time.Unix(0, 0).UTC()
	}
	if record.payload == nil {
		record.payload = []byte{}
	}
	return record, true
}

type sqlExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func upsertRequest(ctx context.Context, db sqlExecer, mode string, r requestRecord) error {
	query := `INSERT INTO requests(id,payload,raw,masked,upstream,restored,status,error,meta,started_at,ended_at,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`
	if mode == "mysql" {
		query += ` ON DUPLICATE KEY UPDATE payload=VALUES(payload),raw=VALUES(raw),masked=VALUES(masked),upstream=VALUES(upstream),restored=VALUES(restored),status=VALUES(status),error=VALUES(error),meta=VALUES(meta),started_at=VALUES(started_at),ended_at=VALUES(ended_at),created_at=VALUES(created_at)`
	} else {
		query += ` ON CONFLICT(id) DO UPDATE SET payload=excluded.payload,raw=excluded.raw,masked=excluded.masked,upstream=excluded.upstream,restored=excluded.restored,status=excluded.status,error=excluded.error,meta=excluded.meta,started_at=excluded.started_at,ended_at=excluded.ended_at,created_at=excluded.created_at`
	}
	_, err := db.ExecContext(ctx, query, r.id, r.payload, r.raw, r.masked, r.upstream, r.restored, r.status, r.failure, r.meta, nullableTime(r.started), nullableTime(r.ended), r.created)
	return err
}

func upsertEvent(ctx context.Context, db sqlExecer, mode string, e eventRecord) error {
	query := `INSERT INTO events(id,request_id,event_type,payload,raw,masked,upstream,restored,status,error,meta,started_at,ended_at,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`
	if mode == "mysql" {
		query += ` ON DUPLICATE KEY UPDATE request_id=VALUES(request_id),event_type=VALUES(event_type),payload=VALUES(payload),raw=VALUES(raw),masked=VALUES(masked),upstream=VALUES(upstream),restored=VALUES(restored),status=VALUES(status),error=VALUES(error),meta=VALUES(meta),started_at=VALUES(started_at),ended_at=VALUES(ended_at),created_at=VALUES(created_at)`
	} else {
		query += ` ON CONFLICT(id) DO UPDATE SET request_id=excluded.request_id,event_type=excluded.event_type,payload=excluded.payload,raw=excluded.raw,masked=excluded.masked,upstream=excluded.upstream,restored=excluded.restored,status=excluded.status,error=excluded.error,meta=excluded.meta,started_at=excluded.started_at,ended_at=excluded.ended_at,created_at=excluded.created_at`
	}
	_, err := db.ExecContext(ctx, query, e.id, e.requestID, e.typ, e.payload, e.raw, e.masked, e.upstream, e.restored, e.status, e.failure, e.meta, nullableTime(e.started), nullableTime(e.ended), e.created)
	return err
}

func verifyRequest(ctx context.Context, db *sql.DB, r requestRecord) bool {
	var got requestRecord
	err := db.QueryRowContext(ctx, `SELECT id,payload,raw,masked,upstream,restored,status,error,meta,started_at,ended_at,created_at FROM requests WHERE id=?`, r.id).Scan(&got.id, &got.payload, &got.raw, &got.masked, &got.upstream, &got.restored, &got.status, &got.failure, &got.meta, nullableScanner(&got.started), nullableScanner(&got.ended), &got.created)
	return err == nil && equalRequest(r, got)
}

func verifyEvent(ctx context.Context, db *sql.DB, e eventRecord) bool {
	var got eventRecord
	err := db.QueryRowContext(ctx, `SELECT id,request_id,event_type,payload,raw,masked,upstream,restored,status,error,meta,started_at,ended_at,created_at FROM events WHERE id=?`, e.id).Scan(&got.id, &got.requestID, &got.typ, &got.payload, &got.raw, &got.masked, &got.upstream, &got.restored, &got.status, &got.failure, &got.meta, nullableScanner(&got.started), nullableScanner(&got.ended), &got.created)
	return err == nil && equalEvent(e, got)
}

// nullableScanner keeps verification compatible with NULL terminal timestamps.
func nullableScanner(dest *time.Time) any { return &nullableTimeValue{dest: dest} }

type nullableTimeValue struct{ dest *time.Time }

func (v *nullableTimeValue) Scan(src any) error {
	t := timeValue(src)
	if t.IsZero() {
		*v.dest = time.Time{}
	} else {
		*v.dest = t
	}
	return nil
}

func equalRequest(a, b requestRecord) bool {
	return a.id == b.id && string(a.payload) == string(b.payload) && string(a.raw) == string(b.raw) && string(a.masked) == string(b.masked) && string(a.upstream) == string(b.upstream) && string(a.restored) == string(b.restored) && a.status == b.status && a.failure == b.failure && a.meta == b.meta && sameTime(a.started, b.started) && sameTime(a.ended, b.ended) && sameTime(a.created, b.created)
}
func equalEvent(a, b eventRecord) bool {
	return a.id == b.id && a.requestID == b.requestID && a.typ == b.typ && string(a.payload) == string(b.payload) && string(a.raw) == string(b.raw) && string(a.masked) == string(b.masked) && string(a.upstream) == string(b.upstream) && string(a.restored) == string(b.restored) && a.status == b.status && a.failure == b.failure && a.meta == b.meta && sameTime(a.started, b.started) && sameTime(a.ended, b.ended) && sameTime(a.created, b.created)
}
func sameTime(a, b time.Time) bool {
	if a.IsZero() || b.IsZero() {
		return a.IsZero() == b.IsZero()
	}
	return a.Equal(b)
}

func writeRequestDigest(h interface{ Write([]byte) (int, error) }, r requestRecord) {
	writeFields(h, r.id, string(r.payload), string(r.raw), string(r.masked), string(r.upstream), string(r.restored), r.status, r.failure, r.meta, r.started.UTC().String(), r.ended.UTC().String(), r.created.UTC().String())
}
func writeEventDigest(h interface{ Write([]byte) (int, error) }, e eventRecord) {
	writeFields(h, e.id, e.requestID, e.typ, string(e.payload), string(e.raw), string(e.masked), string(e.upstream), string(e.restored), e.status, e.failure, e.meta, e.started.UTC().String(), e.ended.UTC().String(), e.created.UTC().String())
}
func writeFields(h interface{ Write([]byte) (int, error) }, fields ...string) {
	for _, field := range fields {
		_, _ = h.Write([]byte(strconv.Itoa(len(field)) + ":" + field + "\n"))
	}
}

func tableColumns(ctx context.Context, db *sql.DB, mode, table string) ([]string, bool, error) {
	var query string
	if mode == "mysql" {
		query = `SELECT column_name FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name=? ORDER BY ordinal_position`
	} else {
		query = "PRAGMA table_info(" + table + ")"
	}
	rows, err := db.QueryContext(ctx, query, func() []any {
		if mode == "mysql" {
			return []any{table}
		}
		return nil
	}()...)
	if err != nil {
		if mode != "mysql" && strings.Contains(strings.ToLower(err.Error()), "no such table") {
			return nil, false, nil
		}
		return nil, false, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		if mode == "mysql" {
			var name string
			if err := rows.Scan(&name); err != nil {
				return nil, false, err
			}
			out = append(out, name)
		} else {
			var cid int
			var name, typ string
			var notNull, pk int
			var defaultValue any
			if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
				return nil, false, err
			}
			out = append(out, name)
		}
	}
	return out, len(out) > 0, rows.Err()
}
func hasColumn(columns []string, wanted string) bool {
	for _, column := range columns {
		if strings.EqualFold(column, wanted) {
			return true
		}
	}
	return false
}
func text(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case []byte:
		return string(v)
	default:
		return fmt.Sprint(v)
	}
}
func bytesValue(value any) []byte {
	switch v := value.(type) {
	case nil:
		return nil
	case []byte:
		return append([]byte(nil), v...)
	case string:
		return []byte(v)
	default:
		return []byte(fmt.Sprint(v))
	}
}

func migrationBytesValue(value any) []byte {
	return bytesValue(value)
}
func timeValue(value any) time.Time {
	switch v := value.(type) {
	case nil:
		return time.Time{}
	case time.Time:
		return v.UTC()
	case []byte:
		return timeValue(string(v))
	case string:
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			return t.UTC()
		}
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return unixFloat(f)
		}
	case float64:
		return unixFloat(v)
	case float32:
		return unixFloat(float64(v))
	case int64:
		return unixFloat(float64(v))
	case int:
		return unixFloat(float64(v))
	case uint64:
		return unixFloat(float64(v))
	}
	return time.Time{}
}
func unixFloat(value float64) time.Time {
	if value == 0 {
		return time.Time{}
	}
	if value > 1e12 {
		value /= 1000
	}
	sec := int64(value)
	nsec := int64((value - float64(sec)) * 1e9)
	return time.Unix(sec, nsec).UTC()
}
