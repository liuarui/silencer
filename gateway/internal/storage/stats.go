package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"
)

// Statistics read metadata, not the four potentially very large ledger bodies.
// Only rows without complete accounting fall back to the required body columns.
func (s *Storage) compactStats(ctx context.Context, since time.Time) ([]Request, []Event, error) {
	return s.compactReadStats(ctx, since, true, false)
}

// RequestStats omits the unused event history for request-only aggregates.
func (s *Storage) RequestStats(ctx context.Context, since time.Time) ([]Request, error) {
	requests, _, err := s.compactReadStats(ctx, since, false, true)
	return requests, err
}

// HistoryStats preserves legacy request-based restoration times across buckets.
func (s *Storage) HistoryStats(ctx context.Context, since time.Time) ([]Request, []Event, error) {
	return s.compactReadStats(ctx, since, true, true)
}

func (s *Storage) compactReadStats(ctx context.Context, since time.Time, includeEvents, requestRestoreFallback bool) ([]Request, []Event, error) {
	var events []Event
	restoreEvents := map[string]bool{}
	if includeEvents {
		rows, err := s.db.QueryContext(ctx, `SELECT id,request_id,event_type,status,error,meta,created_at FROM events WHERE created_at>=? ORDER BY created_at,id`, since)
		if err != nil {
			return nil, nil, err
		}
		for rows.Next() {
			var e Event
			var status, failure, meta sql.NullString
			if err = rows.Scan(&e.ID, &e.RequestID, &e.Type, &status, &failure, &meta, &e.CreatedAt); err != nil {
				break
			}
			e.Status, e.Error, e.Meta = status.String, failure.String, meta.String
			e.StatsMeta = parseStatsMeta(e.Meta)
			if strings.Contains(strings.ToUpper(e.Type), "RESTORE") && e.RequestID != "" {
				restoreEvents[e.RequestID] = true
			}
			events = append(events, e)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return nil, nil, err
		}
		if err = s.hydrateLegacyMaskEvents(ctx, events); err != nil {
			return nil, nil, err
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,status,error,meta,created_at FROM requests WHERE created_at>=? ORDER BY created_at,id`, since)
	if err != nil {
		return nil, nil, err
	}
	var requests []Request
	index := map[string]int{}
	var maskIDs, responseIDs []string
	for rows.Next() {
		var r Request
		var status, failure, meta sql.NullString
		if err = rows.Scan(&r.ID, &status, &failure, &meta, &r.CreatedAt); err != nil {
			break
		}
		r.Status, r.Error, r.Meta = status.String, failure.String, meta.String
		r.StatsMeta = parseStatsMeta(r.Meta)
		_, counted := r.StatsMeta["mask_token_count"]
		items, _ := r.StatsMeta["items"].([]any)
		if r.StatsMeta["items_complete"] != true || (!counted && len(items) == 0 && r.StatsMeta["log_expired"] != true) {
			maskIDs = append(maskIDs, r.ID)
		}
		if _, complete := r.StatsMeta["restore_count"]; !complete && (requestRestoreFallback || !restoreEvents[r.ID]) {
			responseIDs = append(responseIDs, r.ID)
		}
		index[r.ID] = len(requests)
		requests = append(requests, r)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, nil, err
	}
	for _, group := range []struct {
		ids       []string
		responses bool
	}{{maskIDs, false}, {responseIDs, true}} {
		for offset := 0; offset < len(group.ids); offset += 200 {
			end := offset + 200
			if end > len(group.ids) {
				end = len(group.ids)
			}
			args := make([]any, end-offset)
			for i, id := range group.ids[offset:end] {
				args[i] = id
			}
			columns := "masked"
			if group.responses {
				columns = "upstream,restored"
			}
			query := "SELECT id," + columns + " FROM requests WHERE id IN (" + strings.TrimSuffix(strings.Repeat("?,", len(args)), ",") + ")"
			rows, err = s.db.QueryContext(ctx, query, args...)
			if err != nil {
				return nil, nil, err
			}
			for rows.Next() {
				var id string
				var a, b []byte
				if group.responses {
					err = rows.Scan(&id, &a, &b)
				} else {
					err = rows.Scan(&id, &a)
				}
				if err != nil {
					break
				}
				r := &requests[index[id]]
				if group.responses {
					r.Upstream, r.Restored = a, b
				} else {
					r.Masked = a
				}
			}
			if err == nil {
				err = rows.Err()
			}
			rows.Close()
			if err != nil {
				return nil, nil, err
			}
		}
	}
	return requests, events, nil
}

func parseStatsMeta(text string) map[string]any {
	var meta map[string]any
	_ = json.Unmarshal([]byte(text), &meta)
	if meta == nil {
		meta = map[string]any{}
	}
	return meta
}

type RestoreBodySummary struct {
	ID, RequestID, Type, Meta string
	Bytes                     int
}

// RestoreBodyStats returns lengths for the redacted restore list; the endpoint
// never uses the content and must not transfer every retained response body.
func (s *Storage) RestoreBodyStats(ctx context.Context, since time.Time) ([]RestoreBodySummary, []RestoreBodySummary, error) {
	requests := []RestoreBodySummary{}
	rows, err := s.db.QueryContext(ctx, `SELECT id,LENGTH(restored) FROM requests WHERE created_at>=? AND LENGTH(restored)>0 ORDER BY created_at,id`, since)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var item RestoreBodySummary
		if err = rows.Scan(&item.ID, &item.Bytes); err != nil {
			break
		}
		requests = append(requests, item)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, nil, err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT id,request_id,event_type,meta,LENGTH(restored) FROM events WHERE created_at>=? AND LENGTH(restored)>0 ORDER BY created_at,id`, since)
	if err != nil {
		return nil, nil, err
	}
	var events []RestoreBodySummary
	for rows.Next() {
		var item RestoreBodySummary
		var meta sql.NullString
		if err = rows.Scan(&item.ID, &item.RequestID, &item.Type, &meta, &item.Bytes); err != nil {
			break
		}
		item.Meta = meta.String
		events = append(events, item)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	return requests, events, err
}
