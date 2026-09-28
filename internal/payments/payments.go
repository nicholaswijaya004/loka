package payments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/nicholaswijaya004/loka/internal/booking"
	"github.com/nicholaswijaya004/loka/internal/consumer"
	"github.com/nicholaswijaya004/loka/internal/storage"
)

const (
	// ConsumerName identifies this service in processed_events. Never rename it.
	ConsumerName = "payments"

	eventBookingCreated = "booking.created"
)

type bookingCreated struct {
	Version   int       `json:"version"`
	BookingID uuid.UUID `json:"booking_id"`
}

type Handler struct {
	logger *slog.Logger
}

func New(logger *slog.Logger) *Handler {
	return &Handler{logger: logger}
}

var _ consumer.Handler = (*Handler)(nil)

// Handle runs inside the consumer's transaction; all writes go through tx.
func (h *Handler) Handle(ctx context.Context, tx *storage.Store, e consumer.Event) error {
	switch e.Type {
	case eventBookingCreated:
		return h.handleBookingCreated(ctx, tx, e)
	default:
		// Other event types on this topic aren't the handler's business.
		h.logger.Debug("ignoring event", "type", e.Type, "event_id", e.ID)
		return nil
	}
}

func (h *Handler) handleBookingCreated(ctx context.Context, tx *storage.Store, e consumer.Event) error {
	var p bookingCreated
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return fmt.Errorf("%w: event %d: decode booking.created: %w", consumer.ErrPoisonMessage, e.ID, err)
	}
	if p.Version != 1 {
		return fmt.Errorf("%w: event %d: unsupported booking.created version %d", consumer.ErrPoisonMessage, e.ID, p.Version)
	}
	if p.BookingID == uuid.Nil {
		return fmt.Errorf("%w: event %d: booking_id missing", consumer.ErrPoisonMessage, e.ID)
	}

	err := booking.Transition(ctx, tx, p.BookingID, booking.StatusPending, booking.StatusPaymentPending, nil)
	switch {
	case err == nil:
		return nil

	case errors.Is(err, storage.ErrStatusConflict):
		// No longer pending: expired first, or its payment already started.
		// Nothing will make it pending again, so retrying can't help.
		h.logger.Info("booking not pending, payment not started",
			"booking_id", p.BookingID, "event_id", e.ID)
		return nil

	case errors.Is(err, storage.ErrBookingNotFound):
		// The event and the booking are written in one transaction, so this
		// means data was lost or the payload is wrong. Retrying can't fix it.
		return fmt.Errorf("%w: event %d: booking %s not found", consumer.ErrPoisonMessage, e.ID, p.BookingID)

	case errors.Is(err, booking.ErrIllegalTransition):
		// pending → payment_pending is always allowed, so this is a code bug.
		return fmt.Errorf("%w: event %d: %w", consumer.ErrPoisonMessage, e.ID, err)

	default:
		return err // transient (e.g. database down): Run retries this event
	}
}
