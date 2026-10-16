package storage

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// ListRequestSummaries returns the most recent retained ledger rows without
// transferring any request or response bodies. Detail reads remain separate.
func (s *Storage) ListRequestSummaries(ctx context.Context, limit int) ([]Request, error) {
	return s.ListRequestSummariesBefore(ctx, limit, time.Time{}, "")
}

// Seek pagination lets the gateway apply current configuration before limiting
// matches, without loading bodies or missing failures behind newer good rows.
func (s *Storage) ListRequestSummariesBefore(ctx context.Context, limit int, before time.Time, beforeID string) ([]Request, error) {
	if limit <= 0 {
		limit = 100
	}
	query := `SELECT id,payload,status,error,meta,started_at,ended_at,created_at,
 COALESCE(LENGTH(raw),0)+COALESCE(LENGTH(masked),0)+COALESCE(LENGTH(upstream),0)+COALESCE(LENGTH(restored),0)>0
 FROM requests WHERE (meta IS NULL OR meta NOT LIKE ?)`
	args := []any{`%"log_expired":true%`}
	if !before.IsZero() {
		query += ` AND (created_at < ? OR (created_at = ? AND id < ?))`
		args = append(args, before, before, beforeID)
	}
	query += ` ORDER BY created_at DESC,id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Request
	for rows.Next() {
		var r Request
		var status, failure, meta sql.NullString
		var started, ended sql.NullTime
		var available bool
		if err = rows.Scan(&r.ID, &r.Payload, &status, &failure, &meta, &started, &ended, &r.CreatedAt, &available); err != nil {
			return nil, err
		}
		r.Status, r.Error, r.Meta = status.String, failure.String, meta.String
		r.StatsMeta = parseStatsMeta(r.Meta)
		r.SummaryBodyAvailable = &available
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

// Event summaries supply audit/phase outcomes only, never plaintext bodies.
func (s *Storage) RequestEventSummaries(ctx context.Context, ids []string) ([]Event, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	query := `SELECT id,request_id,event_type,status,error,meta,created_at FROM events WHERE request_id IN (` + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + `)`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var item Event
		var status, failure, meta sql.NullString
		if err := rows.Scan(&item.ID, &item.RequestID, &item.Type, &status, &failure, &meta, &item.CreatedAt); err != nil {
			return nil, err
		}
		item.Status, item.Error, item.Meta = status.String, failure.String, meta.String
		item.StatsMeta = parseStatsMeta(item.Meta)
		out = append(out, item)
	}
	return out, rows.Err()
}

// Old rows without either counters or a RESTORE phase need the same narrow
// response fallback used by dashboard statistics. Bodies stay out of API lists.
func (s *Storage) HydrateLegacyResponseSummaries(ctx context.Context, items []Request, events []Event) error {
	hasRestore := make(map[string]bool)
	for _, event := range events {
		if strings.EqualFold(event.Type, "RESTORE") {
			hasRestore[event.RequestID] = true
		}
	}
	var ids []any
	index := make(map[string]int)
	for i, item := range items {
		meta := item.StatsMeta
		if meta == nil {
			meta = parseStatsMeta(item.Meta)
		}
		_, counted := meta["restore_count"]
		_, unresolved := meta["restore_unresolved"]
		if counted || unresolved || hasRestore[item.ID] || meta["log_expired"] == true {
			continue
		}
		ids = append(ids, item.ID)
		index[item.ID] = i
	}
	if len(ids) == 0 {
		return nil
	}
	query := `SELECT id,upstream,restored FROM requests WHERE id IN (` + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + `)`
	rows, err := s.db.QueryContext(ctx, query, ids...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var upstream, restored []byte
		if err := rows.Scan(&id, &upstream, &restored); err != nil {
			return err
		}
		item := &items[index[id]]
		item.Upstream, item.Restored = upstream, restored
	}
	return rows.Err()
}

// RequestSearchBodies amortizes full-text candidate lookups in bounded batches.
// Full captures remain streamed from files by the caller, not buffered here.
func (s *Storage) RequestSearchBodies(ctx context.Context, ids []string) ([]Request, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	query := `SELECT id,raw,masked,upstream,restored,meta FROM requests WHERE id IN (` + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + `)`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Request
	for rows.Next() {
		var item Request
		var meta sql.NullString
		if err = rows.Scan(&item.ID, &item.Raw, &item.Masked, &item.Upstream, &item.Restored, &meta); err != nil {
			return nil, err
		}
		item.Meta = meta.String
		item.StatsMeta = parseStatsMeta(item.Meta)
		out = append(out, item)
	}
	return out, rows.Err()
}
