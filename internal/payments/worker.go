package payments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nicholaswijaya004/loka/internal/booking"
	"github.com/nicholaswijaya004/loka/internal/storage"
)

const (
	aggregateBooking      = "booking"
	eventBookingConfirmed = "booking.confirmed"
	outcomeEventVersion   = 1
)

type bookingConfirmedPayload struct {
	Version     int       `json:"version"`
	BookingID   uuid.UUID `json:"booking_id"`
	CustomerID  uuid.UUID `json:"customer_id"`
	TotalMinor  int64     `json:"total_minor"`
	Currency    string    `json:"currency"`
	ChargeID    string    `json:"charge_id"`
	ConfirmedAt time.Time `json:"confirmed_at"`
}

// WorkerConfig holds the worker's numbers. MaxInFlight is how many charges
// run at once. Each booking is charged as soon as it is claimed, so the
// provider's timeout must be shorter than Lease. After a failed attempt, the
// next try is Lease plus a jittered backoff that grows from BackoffBase up to
// MaxBackoff; Lease + MaxBackoff must stay well below EscalateAfter, or
// escalation is delayed.
type WorkerConfig struct {
	MaxInFlight   int
	Interval      time.Duration
	Lease         time.Duration
	EscalateAfter time.Duration
	BackoffBase   time.Duration
	MaxBackoff    time.Duration
}

// Worker charges payment_pending bookings and records each outcome. It is
// component 2 of ADR-003.
type Worker struct {
	store    *storage.Store
	provider Provider
	logger   *slog.Logger
	cfg      WorkerConfig
}

func NewWorker(store *storage.Store, provider Provider, logger *slog.Logger, cfg WorkerConfig) *Worker {
	return &Worker{store: store, provider: provider, logger: logger, cfg: cfg}
}

// processOne charges one booking and acts on the answer. It never cancels a
// booking whose outcome is unknown: those stay payment_pending, and the lease
// makes a later poll try again with the same idempotency key.
func (w *Worker) processOne(ctx context.Context, d storage.DuePayment) {
	log := w.logger.With("booking_id", d.BookingID)

	res, err := w.provider.Charge(ctx, ChargeRequest{
		IdempotencyKey: d.BookingID.String(),
		BookingID:      d.BookingID,
		AmountMinor:    d.TotalMinor,
		Currency:       d.Currency,
	})
	if err != nil {
		if ctx.Err() != nil {
			return // shutting down; the lease hands this booking to the next run
		}
		waiting := time.Since(d.PendingSince)
		switch {
		case IsPermanent(err):
			// Retrying can't help. Escalate now; never cancel.
			log.Error("payment needs review: escalate",
				"attempts", d.Attempts, "waiting", waiting, "error", err)
		case waiting > w.cfg.EscalateAfter:
			log.Error("payment stuck: escalate",
				"attempts", d.Attempts, "waiting", waiting, "error", err)
		default:
			log.Warn("charge got no answer, will retry",
				"attempts", d.Attempts, "waiting", waiting, "error", err)
		}
		return
	}

	err = w.applyOutcome(ctx, d, res)
	switch {
	case err == nil:
		log.Info("payment outcome recorded", "outcome", res.Outcome, "charge_id", res.ChargeID)
	case errors.Is(err, storage.ErrStatusConflict):
		// Another worker recorded this outcome first. Correct, not a failure.
		log.Info("payment outcome already recorded")
	default:
		// The charge is known but recording it failed. The lease brings it
		// back, the same key replays the same result, and we try again.
		log.Error("record payment outcome", "outcome", res.Outcome, "error", err)
	}
}

// applyOutcome records a known outcome in one transaction. The transition
// comes first: if another worker already moved the booking, it fails fast
// with ErrStatusConflict and nothing else is written.
func (w *Worker) applyOutcome(ctx context.Context, d storage.DuePayment, res *ChargeResult) error {
	return w.store.WithTx(ctx, func(tx *storage.Store) error {
		switch res.Outcome {
		case ChargeSucceeded:
			return confirm(ctx, tx, d, res)
		case ChargeDeclined:
			return cancel(ctx, tx, d, res)
		default:
			return fmt.Errorf("%w: outcome %q", ErrUnexpectedProviderResponse, res.Outcome)
		}
	})
}

func confirm(ctx context.Context, tx *storage.Store, d storage.DuePayment, res *ChargeResult) error {
	if err := booking.Transition(ctx, tx, d.BookingID,
		booking.StatusPaymentPending, booking.StatusConfirmed, nil); err != nil {
		return err
	}

	chargeID := res.ChargeID
	if _, err := tx.InsertPayment(ctx, storage.Payment{
		BookingID:         d.BookingID,
		AmountMinor:       d.TotalMinor,
		Currency:          d.Currency,
		Status:            storage.PaymentSucceeded,
		ProviderPaymentID: &chargeID,
	}); err != nil {
		return err
	}

	return insertEvent(ctx, tx, d.BookingID, eventBookingConfirmed, bookingConfirmedPayload{
		Version:     outcomeEventVersion,
		BookingID:   d.BookingID,
		CustomerID:  d.CustomerID,
		TotalMinor:  d.TotalMinor,
		Currency:    d.Currency,
		ChargeID:    res.ChargeID,
		ConfirmedAt: time.Now().UTC(),
	})
}

func cancel(ctx context.Context, tx *storage.Store, d storage.DuePayment, res *ChargeResult) error {
	reason := res.DeclineReason
	if reason == "" {
		reason = "payment_declined"
	}

	if err := booking.Transition(ctx, tx, d.BookingID,
		booking.StatusPaymentPending, booking.StatusCancelled, &reason); err != nil {
		return err
	}

	// Compensation: give the seats back. Tied to the transition above, so it
	// happens exactly once per booking.
	if err := tx.ReleaseSeats(ctx, d.UnitID, d.Qty); err != nil {
		return err
	}

	var providerID *string
	if res.ChargeID != "" {
		providerID = &res.ChargeID
	}
	if _, err := tx.InsertPayment(ctx, storage.Payment{
		BookingID:         d.BookingID,
		AmountMinor:       d.TotalMinor,
		Currency:          d.Currency,
		Status:            storage.PaymentFailed,
		ProviderPaymentID: providerID,
		FailureReason:     &reason,
	}); err != nil {
		return err
	}

	return insertEvent(ctx, tx, d.BookingID, booking.EventBookingCancelled, booking.CancelledPayload{
		Version:     booking.CancelledEventVersion,
		BookingID:   d.BookingID,
		CustomerID:  d.CustomerID,
		Reason:      reason,
		CancelledAt: time.Now().UTC(),
	})
}

func insertEvent(ctx context.Context, tx *storage.Store, bookingID uuid.UUID, eventType string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal %s payload: %w", eventType, err)
	}
	return tx.InsertOutboxEvent(ctx, &storage.Outbox{
		AggregateType: aggregateBooking,
		AggregateID:   bookingID,
		EventType:     eventType,
		Payload:       b,
	})
}

// Run keeps up to MaxInFlight charges going until ctx is cancelled. Each
// charge holds one slot, and a freed slot is refilled at once, so a charge
// that hangs until its timeout (a lost response) holds only its own slot.
// Charging a batch and waiting for all of it would let that one charge stall
// every booking behind it.
//
// It claims only as many bookings as there are free slots, so a claimed
// booking starts charging immediately and its lease isn't spent in a queue.
// On shutdown it waits for the charges in flight to finish or give up.
func (w *Worker) Run(ctx context.Context) error {
	slots := make(chan struct{}, w.cfg.MaxInFlight) // one token per running charge
	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		free, ok := acquireSlots(ctx, slots)
		if !ok {
			return nil
		}

		due, err := w.store.ClaimDuePayments(ctx, free, w.cfg.Lease, w.cfg.BackoffBase, w.cfg.MaxBackoff)
		if err != nil && ctx.Err() == nil {
			w.logger.Error("claim due payments", "error", err)
		}
		for _, d := range due {
			wg.Add(1)
			go func() {
				defer func() {
					<-slots
					wg.Done()
				}()
				w.processOne(ctx, d)
			}()
		}
		// Give back the slots nothing was claimed for.
		for range free - len(due) {
			<-slots
		}

		// Every free slot was filled, so more may be due: claim again as soon
		// as a slot frees. Otherwise nothing more is due right now.
		if err == nil && len(due) == free {
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(w.cfg.Interval):
		}
	}
}

// acquireSlots waits until one slot is free, then takes every other slot
// that is free right now, and returns how many it holds. It reports false if
// ctx ends first.
func acquireSlots(ctx context.Context, slots chan struct{}) (int, bool) {
	select {
	case slots <- struct{}{}:
	case <-ctx.Done():
		return 0, false
	}
	n := 1
	for n < cap(slots) {
		select {
		case slots <- struct{}{}:
			n++
		default:
			return n, true
		}
	}
	return n, true
}
