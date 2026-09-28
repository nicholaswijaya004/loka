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
	eventBookingCancelled = "booking.cancelled"
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

type bookingCancelledPayload struct {
	Version     int       `json:"version"`
	BookingID   uuid.UUID `json:"booking_id"`
	CustomerID  uuid.UUID `json:"customer_id"`
	Reason      string    `json:"reason"`
	CancelledAt time.Time `json:"cancelled_at"`
}

// WorkerConfig holds the worker's numbers. Timeout (in the Provider) must be
// shorter than Lease, and a batch is charged in parallel so that it finishes
// within one timeout, well inside its lease.
type WorkerConfig struct {
	BatchSize     int
	Interval      time.Duration
	Lease         time.Duration
	EscalateAfter time.Duration
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

// RunOnce claims one batch and processes it. It returns how many bookings
// were claimed, so Run knows whether more may be waiting. Per-booking
// problems are handled and logged here; the only error returned is failing
// to claim at all.
func (w *Worker) RunOnce(ctx context.Context) (int, error) {
	due, err := w.store.ClaimDuePayments(ctx, w.cfg.BatchSize, w.cfg.Lease)
	if err != nil {
		return 0, fmt.Errorf("claim due payments: %w", err)
	}

	var wg sync.WaitGroup
	for _, d := range due {
		wg.Add(1)
		go func(d storage.DuePayment) {
			defer wg.Done()
			w.processOne(ctx, d)
		}(d)
	}
	wg.Wait()

	return len(due), nil
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
			log.Error("payment needs review: escalate", "waiting", waiting, "error", err)
		case waiting > w.cfg.EscalateAfter:
			log.Error("payment stuck: escalate", "waiting", waiting, "error", err)
		default:
			log.Warn("charge got no answer, will retry", "waiting", waiting, "error", err)
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

	return insertEvent(ctx, tx, d.BookingID, eventBookingCancelled, bookingCancelledPayload{
		Version:     outcomeEventVersion,
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

// Run polls until ctx is cancelled. A full batch means more may be waiting,
// so it polls again immediately; otherwise it sleeps for the interval.
func (w *Worker) Run(ctx context.Context) error {
	for {
		n, err := w.RunOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			w.logger.Error("payment worker batch failed", "error", err)
		}
		if err == nil && n == w.cfg.BatchSize {
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(w.cfg.Interval):
		}
	}
}
