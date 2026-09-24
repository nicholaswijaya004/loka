package notifier

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/nicholaswijaya004/loka/internal/consumer"
	"github.com/nicholaswijaya004/loka/internal/storage"
)

const (
	// ConsumerName identifies this service in processed_events. Never rename it.
	ConsumerName = "notifier"

	kindBookingConfirmation = "booking_confirmation"
	eventBookingCreated     = "booking.created"
)

// bookingCreated holds only the fields the notifier needs. Unknown fields in
// the payload are ignored, so the producer can add fields without breaking us.
type bookingCreated struct {
	Version    int       `json:"version"`
	BookingID  uuid.UUID `json:"booking_id"`
	CustomerID uuid.UUID `json:"customer_id"`
}

type Notifier struct {
	logger *slog.Logger
}

func New(logger *slog.Logger) *Notifier {
	return &Notifier{logger: logger}
}

var _ consumer.Handler = (*Notifier)(nil)

// Handle runs inside the consumer's transaction; all writes go through tx.
func (n *Notifier) Handle(ctx context.Context, tx *storage.Store, e consumer.Event) error {
	switch e.Type {
	case eventBookingCreated:
		return n.handleBookingCreated(ctx, tx, e)
	default:
		// Other event types on this topic aren't the notifier's business.
		n.logger.Debug("ignoring event", "type", e.Type, "event_id", e.ID)
		return nil
	}
}

func (n *Notifier) handleBookingCreated(ctx context.Context, tx *storage.Store, e consumer.Event) error {
	var p bookingCreated
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return fmt.Errorf("%w: event %d: decode booking.created: %w", consumer.ErrPoisonMessage, e.ID, err)
	}
	if p.Version != 1 {
		return fmt.Errorf("%w: event %d: unsupported booking.created version %d", consumer.ErrPoisonMessage, e.ID, p.Version)
	}

	inserted, err := tx.InsertNotification(ctx, storage.Notification{
		BookingID:  p.BookingID,
		CustomerID: p.CustomerID,
		Kind:       kindBookingConfirmation,
		EventID:    e.ID,
	})
	if err != nil {
		return err // database problem: retried by Run
	}
	if !inserted {
		n.logger.Info("confirmation already queued", "booking_id", p.BookingID, "event_id", e.ID)
	}
	return nil
}
