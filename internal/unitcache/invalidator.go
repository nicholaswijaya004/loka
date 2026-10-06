package unitcache

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/nicholaswijaya004/loka/internal/booking"
	"github.com/nicholaswijaya004/loka/internal/consumer"
	"github.com/nicholaswijaya004/loka/internal/storage"
)

// ConsumerName is the invalidator's Kafka consumer group and its key in
// processed_events. Renaming it starts a new group from scratch.
const ConsumerName = "cacheinvalidator"

const eventBookingCreated = "booking.created"

// Deleter is the one cache operation the invalidator needs.
type Deleter interface {
	Delete(ctx context.Context, id uuid.UUID) error
}

// Invalidator drops a unit's cache entry whenever its seats change: taken by
// booking.created, given back by booking.cancelled. booking.confirmed changes
// nothing on the unit, so it is ignored.
type Invalidator struct {
	deleter Deleter
	logger  *slog.Logger
}

func NewInvalidator(deleter Deleter, logger *slog.Logger) *Invalidator {
	return &Invalidator{deleter: deleter, logger: logger}
}

// Handle never uses tx: the invalidator only talks to Redis. A Redis error is
// returned wrapped but not as poison, so the framework retries the event
// without committing the offset.
func (i *Invalidator) Handle(ctx context.Context, _ *storage.Store, e consumer.Event) error {
	switch e.Type {
	case eventBookingCreated, booking.EventBookingCancelled:
		// Only the fields this consumer needs; both events use these keys.
		var p struct {
			Version int       `json:"version"`
			UnitID  uuid.UUID `json:"unit_id"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return fmt.Errorf("%w: decode %s: %v", consumer.ErrPoisonMessage, e.Type, err)
		}
		if p.UnitID == uuid.Nil {
			// A v1 cancelled event predates unit_id: the TTL bounds the
			// staleness it leaves. Anything else without a unit is a
			// producer bug, and retrying can't fix it.
			if e.Type == booking.EventBookingCancelled && p.Version < 2 {
				i.logger.Info("cancelled event without unit_id, left to the TTL",
					"event_id", e.ID, "version", p.Version)
				return nil
			}
			return fmt.Errorf("%w: %s v%d without unit_id", consumer.ErrPoisonMessage, e.Type, p.Version)
		}
		if err := i.deleter.Delete(ctx, p.UnitID); err != nil {
			return fmt.Errorf("invalidate unit %s: %w", p.UnitID, err)
		}
		return nil
	default:
		return nil
	}
}
