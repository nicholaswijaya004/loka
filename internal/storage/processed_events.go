package storage

import (
	"context"
	"fmt"
)

func (s *Store) ClaimEvent(ctx context.Context, consumer string, eventID int64) (bool, error) {
	tag, err := s.db.Exec(ctx, `
		INSERT INTO processed_events (consumer, event_id)
		VALUES ($1, $2)
		ON CONFLICT DO NOTHING
	`, consumer, eventID)

	if err != nil {
		return false, fmt.Errorf("claim event %d for %s: %w", eventID, consumer, err)
	}

	// return false ans nil, means no error nothing went wrong
	// skip the messages
	if tag.RowsAffected() == 0 {
		return false, nil
	}

	return true, nil
}
