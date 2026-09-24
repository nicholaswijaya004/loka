package storage

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

type Notification struct {
	BookingID  uuid.UUID
	CustomerID uuid.UUID
	Kind       string
	EventID    int64
}

// InsertNotification queues a notification. It returns false, with no error,
// if one of the same kind already exists for the booking.
func (s *Store) InsertNotification(ctx context.Context, n Notification) (bool, error) {
	tag, err := s.db.Exec(ctx, `
		INSERT INTO notifications (booking_id, customer_id, kind, event_id)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (booking_id, kind) DO NOTHING
	`, n.BookingID, n.CustomerID, n.Kind, n.EventID)
	if err != nil {
		return false, fmt.Errorf("insert notification for booking %s: %w", n.BookingID, err)
	}
	return tag.RowsAffected() == 1, nil
}
