package storage

import (
	"context"
	"database/sql"
	"time"
)

func (s *Storage) Done() <-chan struct{} { return s.writeCtx.Done() }

func (s *Storage) PendingBodyCleanup(ctx context.Context, limit int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM requests WHERE meta LIKE ? ORDER BY created_at LIMIT ?`, `%"body_cleanup_pending":true%`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Storage) CompleteBodyCleanup(ctx context.Context, id string) error {
	return s.enqueueAndWait(ctx, func(ctx context.Context, db *sql.DB) error {
		_, err := db.ExecContext(ctx, `UPDATE requests SET meta=REPLACE(meta,?,?) WHERE id=?`, `"body_cleanup_pending":true`, `"body_cleanup_pending":false`, id)
		return err
	})
}

// Remove detail data in small transactions while retaining accounting rows.
// In-flight requests are never expired. Transforms retain only safe counters.
func (s *Storage) ExpireLogDetails(ctx context.Context, before time.Time, limit int, requestMeta func(Request) string, eventMeta func(Event) string, active func(string) bool) ([]string, int, error) {
	var requests []Request
	for _, running := range []bool{false, true} {
		predicate := "status<>'running'"
		if running {
			predicate = "status='running'"
		}
		var cursorTime time.Time
		cursorID := ""
		accepted := 0
		for accepted < limit {
			query := `SELECT id,created_at FROM requests WHERE created_at<? AND ` + predicate + ` AND (meta IS NULL OR meta NOT LIKE ?)`
			args := []any{before, `%"log_expired":true%`}
			if cursorID != "" {
				query += ` AND (created_at>? OR (created_at=? AND id>?))`
				args = append(args, cursorTime, cursorTime, cursorID)
			}
			query += ` ORDER BY created_at,id LIMIT ?`
			args = append(args, limit)
			rows, err := s.db.QueryContext(ctx, query, args...)
			if err != nil {
				return nil, 0, err
			}
			type candidate struct {
				id      string
				created time.Time
			}
			var pending []candidate
			for rows.Next() {
				var c candidate
				if err = rows.Scan(&c.id, &c.created); err != nil {
					break
				}
				pending = append(pending, c)
			}
			if err == nil {
				err = rows.Err()
			}
			rows.Close()
			if err != nil {
				return nil, 0, err
			}
			for _, c := range pending {
				cursorTime, cursorID = c.created, c.id
				// The lease covers terminal database updates and event inserts too.
				if active(c.id) {
					continue
				}
				r, e := s.GetRequest(ctx, c.id)
				if e != nil {
					return nil, 0, e
				}
				r.Meta = requestMeta(r)
				requests = append(requests, r)
				accepted++
				if accepted == limit {
					break
				}
			}
			if len(pending) < limit {
				break
			}
		}
	}

	var events []Event
	var cursorTime time.Time
	cursorID := ""
	for len(events) < limit {
		query := `SELECT id,request_id,event_type,masked,meta,created_at FROM events WHERE created_at<? AND (meta IS NULL OR meta NOT LIKE ?)`
		args := []any{before, `%"log_expired":true%`}
		if cursorID != "" {
			query += ` AND (created_at>? OR (created_at=? AND id>?))`
			args = append(args, cursorTime, cursorTime, cursorID)
		}
		query += ` ORDER BY created_at,id LIMIT ?`
		args = append(args, limit)
		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, 0, err
		}
		var pending []Event
		for rows.Next() {
			var e Event
			var meta sql.NullString
			if err = rows.Scan(&e.ID, &e.RequestID, &e.Type, &e.Masked, &meta, &e.CreatedAt); err != nil {
				break
			}
			e.Meta = meta.String
			pending = append(pending, e)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return nil, 0, err
		}
		for _, e := range pending {
			cursorTime, cursorID = e.CreatedAt, e.ID
			if e.RequestID != "" && active(e.RequestID) {
				continue
			}
			e.Meta = eventMeta(e)
			events = append(events, e)
			if len(events) == limit {
				break
			}
		}
		if len(pending) < limit {
			break
		}
	}

	if len(requests)+len(events) == 0 {
		return nil, 0, nil
	}
	err := s.enqueueAndWait(ctx, func(ctx context.Context, db *sql.DB) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		for _, r := range requests {
			if _, err = tx.ExecContext(ctx, `UPDATE requests SET payload=?,raw=NULL,masked=NULL,upstream=NULL,restored=NULL,meta=? WHERE id=?`, []byte{}, r.Meta, r.ID); err != nil {
				return err
			}
		}
		for _, e := range events {
			if _, err = tx.ExecContext(ctx, `UPDATE events SET payload=?,raw=NULL,masked=NULL,upstream=NULL,restored=NULL,meta=? WHERE id=?`, []byte{}, e.Meta, e.ID); err != nil {
				return err
			}
		}
		return tx.Commit()
	})
	if err != nil {
		return nil, 0, err
	}
	ids := make([]string, 0, len(requests))
	for _, r := range requests {
		ids = append(ids, r.ID)
	}
	return ids, len(requests) + len(events), nil
}

// Repair earlier releases which marked rows expired but left legacy payloads.
// Preserve metadata verbatim: counters must not be recalculated from cleared text.
func (s *Storage) RepairExpiredLogDetails(ctx context.Context, limit int) error {
	return s.enqueueAndWait(ctx, func(ctx context.Context, db *sql.DB) error {
		for _, table := range []string{"requests", "events"} {
			rows, err := db.QueryContext(ctx, `SELECT id FROM `+table+` WHERE meta LIKE ? AND (LENGTH(payload)>0 OR LENGTH(raw)>0 OR LENGTH(masked)>0 OR LENGTH(upstream)>0 OR LENGTH(restored)>0) LIMIT ?`, `%"log_expired":true%`, limit)
			if err != nil {
				return err
			}
			var ids []string
			for rows.Next() {
				var id string
				if err = rows.Scan(&id); err != nil {
					break
				}
				ids = append(ids, id)
			}
			if err == nil {
				err = rows.Err()
			}
			rows.Close()
			if err != nil {
				return err
			}
			for _, id := range ids {
				if _, err = db.ExecContext(ctx, `UPDATE `+table+` SET payload=?,raw=NULL,masked=NULL,upstream=NULL,restored=NULL WHERE id=? AND meta LIKE ?`, []byte{}, id, `%"log_expired":true%`); err != nil {
					return err
				}
			}
		}
		return nil
	})
}
func (s *Storage) RequestExists(ctx context.Context, id string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM requests WHERE id=?`, id).Scan(&n)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}
