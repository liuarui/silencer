package storage

import (
	"context"
	"database/sql"
	"strings"
)

// ListLogSummaries keeps the existing bounded, timestamp-tie window without
// transferring the four ledger bodies. Full-text queries use the full rows.
// Legacy MASK rows without complete accounting still load their masked text.
func (s *Storage) ListLogSummaries(ctx context.Context, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 100
	}
	return s.logSummaries(ctx, `SELECT id,request_id,event_type,payload,status,error,meta,started_at,ended_at,created_at FROM events ORDER BY created_at DESC,id DESC LIMIT ?`, limit)
}

// Match legacy meta.sid before limiting so unrelated empty request IDs cannot
// displace the requested phases. Invalid historical metadata is treated as {}.
func (s *Storage) RelatedLogSummaries(ctx context.Context, requestID string, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 100
	}
	sid := `JSON_EXTRACT(CASE WHEN JSON_VALID(meta) THEN meta ELSE '{}' END,'$.sid')`
	if s.mode == "mysql" {
		sid = `JSON_UNQUOTE(` + sid + `)`
	}
	return s.logSummaries(ctx, `SELECT id,request_id,event_type,payload,status,error,meta,started_at,ended_at,created_at FROM events WHERE request_id=? OR (request_id='' AND (id=? OR `+sid+`=?)) ORDER BY created_at DESC,id DESC LIMIT ?`, requestID, requestID, requestID, limit)
}

func (s *Storage) logSummaries(ctx context.Context, query string, args ...any) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	var events []Event
	for rows.Next() {
		var e Event
		var status, failure, meta sql.NullString
		var started, ended sql.NullTime
		if err = rows.Scan(&e.ID, &e.RequestID, &e.Type, &e.Payload, &status, &failure, &meta, &started, &ended, &e.CreatedAt); err != nil {
			break
		}
		e.Status, e.Error, e.Meta = status.String, failure.String, meta.String
		e.StatsMeta = parseStatsMeta(e.Meta)
		e.StartedAt, e.EndedAt = started.Time, ended.Time
		events = append(events, e)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, err
	}
	if err = s.hydrateLegacyMaskEvents(ctx, events); err != nil {
		return nil, err
	}
	return events, nil
}

func (s *Storage) hydrateLegacyMaskEvents(ctx context.Context, events []Event) error {
	var ids []any
	index := map[string]int{}
	for i := range events {
		e := &events[i]
		if e.StatsMeta == nil {
			e.StatsMeta = parseStatsMeta(e.Meta)
		}
		if strings.Contains(strings.ToUpper(e.Type), "MASK") && (e.StatsMeta["items_complete"] != true || e.StatsMeta["count"] == nil) && e.StatsMeta["log_expired"] != true {
			ids = append(ids, e.ID)
			index[e.ID] = i
		}
	}
	for start := 0; start < len(ids); start += 200 {
		end := start + 200
		if end > len(ids) {
			end = len(ids)
		}
		query := `SELECT id,masked FROM events WHERE id IN (` + strings.TrimSuffix(strings.Repeat("?,", end-start), ",") + `)`
		rows, err := s.db.QueryContext(ctx, query, ids[start:end]...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			var masked []byte
			if err = rows.Scan(&id, &masked); err != nil {
				break
			}
			events[index[id]].Masked = masked
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
