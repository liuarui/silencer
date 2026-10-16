package storage

import (
	"context"
	"time"
)

// Only mapping metadata is needed to warm the bounded entity registry.
func (s *Storage) RecentMappingMetadata(ctx context.Context, since time.Time, limit int) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT meta,created_at FROM events WHERE event_type='MASK' AND created_at>=? AND meta LIKE ? AND meta LIKE ? AND meta NOT LIKE ? ORDER BY created_at DESC,id DESC LIMIT ?`, since, `%"mapping_mode":"entity-v1"%`, `%"original":%`, `%"log_expired":true%`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []Event
	for rows.Next() {
		var event Event
		if err := rows.Scan(&event.Meta, &event.CreatedAt); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}
