package booking

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/nicholaswijaya004/loka/internal/storage"
)

const (
	EventBookingCancelled = "booking.cancelled"
	CancelledEventVersion = 1
	ReasonExpired         = "expired"
)

// CancelledPayload is the booking.cancelled event. The payment worker (on a
// decline) and the expirer both write it, so every consumer sees one schema.
type CancelledPayload struct {
	Version     int       `json:"version"`
	BookingID   uuid.UUID `json:"booking_id"`
	CustomerID  uuid.UUID `json:"customer_id"`
	Reason      string    `json:"reason"`
	CancelledAt time.Time `json:"cancelled_at"`
}

// ExpirerConfig holds the expirer's numbers. After is the deadline: a booking
// still pending that long after it was created is cancelled and its seats
// released. It must stay far above the normal time from insert to
// payment_pending, and above a routine restart of the relay or consumer.
type ExpirerConfig struct {
	After     time.Duration
	BatchSize int
	Interval  time.Duration
}

// Expirer cancels pending bookings past the deadline and releases their
// seats: the saga's timeout path (ADR-003). It only ever touches pending
// bookings; a payment_pending booking may have a charge in flight, and only
// a known outcome can end it.
type Expirer struct {
	store  *storage.Store
	logger *slog.Logger
	cfg    ExpirerConfig
}

func NewExpirer(store *storage.Store, logger *slog.Logger, cfg ExpirerConfig) *Expirer {
	return &Expirer{store: store, logger: logger, cfg: cfg}
}

// RunOnce expires one batch and returns how many bookings it expired. The
// transitions, the seat releases and the events commit together or not at
// all: a transition without its release would leak seats, and a release
// without its transition would give them back twice on the next run.
func (e *Expirer) RunOnce(ctx context.Context) (int, error) {
	// The pair is the same for every booking in the batch, so check it once.
	// Each row is guarded by the WHERE booking_status = 'pending' in the query.
	if err := checkTransition(StatusPending, StatusCancelled); err != nil {
		return 0, err
	}

	var expired []storage.ExpiredBooking
	err := e.store.WithTx(ctx, func(tx *storage.Store) error {
		var err error
		expired, err = tx.ExpirePendingBookings(ctx, e.cfg.After, e.cfg.BatchSize)
		if err != nil || len(expired) == 0 {
			return err
		}
		if err := releaseSeats(ctx, tx, expired); err != nil {
			return err
		}
		cancelledAt := time.Now().UTC()
		for _, b := range expired {
			if err := insertCancelledEvent(ctx, tx, b, cancelledAt); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("expire pending bookings: %w", err)
	}

	if len(expired) > 0 {
		e.logger.Info("expired pending bookings", "count", len(expired), "after", e.cfg.After)
	}
	return len(expired), nil
}

// releaseSeats gives each unit back the seats of its expired bookings: one
// UPDATE per unit, in unit_id order. A fixed order means two expirers lock
// the same units in the same sequence, so they can't deadlock. Map order
// alone would differ from run to run.
func releaseSeats(ctx context.Context, tx *storage.Store, expired []storage.ExpiredBooking) error {
	perUnit := make(map[uuid.UUID]int)
	for _, b := range expired {
		perUnit[b.UnitID] += b.Qty
	}

	units := slices.Collect(maps.Keys(perUnit))
	slices.SortFunc(units, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })

	for _, unitID := range units {
		if err := tx.ReleaseSeats(ctx, unitID, perUnit[unitID]); err != nil {
			return err
		}
	}
	return nil
}

func insertCancelledEvent(ctx context.Context, tx *storage.Store, b storage.ExpiredBooking, cancelledAt time.Time) error {
	payload, err := json.Marshal(CancelledPayload{
		Version:     CancelledEventVersion,
		BookingID:   b.BookingID,
		CustomerID:  b.CustomerID,
		Reason:      ReasonExpired,
		CancelledAt: cancelledAt,
	})
	if err != nil {
		return fmt.Errorf("marshal %s payload: %w", EventBookingCancelled, err)
	}
	return tx.InsertOutboxEvent(ctx, &storage.Outbox{
		AggregateType: bookingAggregate,
		AggregateID:   b.BookingID,
		EventType:     EventBookingCancelled,
		Payload:       payload,
	})
}

// Run polls until ctx is cancelled. A full batch means more may be waiting,
// so it runs again immediately; otherwise it sleeps for the interval.
func (e *Expirer) Run(ctx context.Context) error {
	for {
		n, err := e.RunOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			e.logger.Error("expiry batch failed", "error", err)
		}
		if err == nil && n == e.cfg.BatchSize {
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(e.cfg.Interval):
		}
	}
}
