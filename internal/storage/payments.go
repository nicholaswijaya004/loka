package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	PaymentSucceeded = "succeeded"
	PaymentFailed    = "failed"
)

// DuePayment is a payment_pending booking the worker has claimed.
type DuePayment struct {
	BookingID    uuid.UUID
	UnitID       uuid.UUID
	CustomerID   uuid.UUID
	Qty          int
	TotalMinor   int64
	Currency     string
	PendingSince time.Time // updated_at: when it entered payment_pending; drives escalation
	Attempts     int
	TraceContext map[string]string
}

// Payment is one payment outcome to record. Timeouts are not recorded: an
// unknown outcome is not a payment fact.
type Payment struct {
	BookingID         uuid.UUID
	AmountMinor       int64
	Currency          string
	Status            string  // PaymentSucceeded or PaymentFailed
	ProviderPaymentID *string // the provider's charge id, for reconciliation
	FailureReason     *string // set for PaymentFailed
}

// ClaimDuePayments claims up to limit payment_pending bookings whose next
// attempt is due, and pushes their next_attempt_at forward by its lease plus a jittered,
// exponential backoff, capped at maxBackoff, and it also counts the attempt, so no
// other worker picks them up while this one is charging. If the worker dies,
// the lease simply runs out and the booking becomes due again.
func (s *Store) ClaimDuePayments(ctx context.Context, limit int, lease time.Duration, backoffBase time.Duration, maxBackoff time.Duration) ([]DuePayment, error) {
	rows, err := s.db.Query(ctx, `
		WITH due AS (
			SELECT booking_id
			FROM bookings
			WHERE booking_status = 'payment_pending'
			  AND next_attempt_at <= now()
			ORDER BY next_attempt_at
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
		UPDATE bookings b
		SET payment_attempts = b.payment_attempts + 1,
			next_attempt_at  = now() + (
				$2
				+ random() * least($4::float8, $3::float8 * 2 ^ b.payment_attempts)
			) * interval '1 second'
		FROM due
		WHERE b.booking_id = due.booking_id
		RETURNING b.booking_id, b.unit_id, b.customer_id, b.qty, b.total_minor, b.currency, b.updated_at, b.payment_attempts, b.trace_context
	`, limit, int(lease.Seconds()), int(backoffBase.Seconds()), int(maxBackoff.Seconds()))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			return nil, fmt.Errorf("claim due payments: %w (position %d, detail %q)",
				err, pgErr.Position, pgErr.Detail)
		}
		return nil, fmt.Errorf("claim due payments: %w", err)
	}
	defer rows.Close()

	var due []DuePayment
	for rows.Next() {
		var d DuePayment
		if err := rows.Scan(&d.BookingID, &d.UnitID, &d.CustomerID, &d.Qty, &d.TotalMinor, &d.Currency, &d.PendingSince, &d.Attempts, &d.TraceContext); err != nil {
			return nil, fmt.Errorf("scan due payment: %w", err)
		}
		due = append(due, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read due payments: %w", err)
	}
	return due, nil
}

// InsertPayment records a payment outcome and returns its payment_id. It must
// run in the same transaction as the booking's status transition, after it:
// the transition is what guarantees one outcome per booking. The unique index
// on successful payments is a backstop, reported as ErrDuplicateSuccessfulPayment.
func (s *Store) InsertPayment(ctx context.Context, p Payment) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.db.QueryRow(ctx, `
		INSERT INTO payments (booking_id, amount_minor, currency, payment_status,
		                      provider_payment_id, failure_reason, succeeded_at, failed_at)
		VALUES ($1, $2, $3, $4, $5, $6,
		        CASE WHEN $4::text = 'succeeded' THEN now() END,
		        CASE WHEN $4::text = 'failed'    THEN now() END)
		RETURNING payment_id
	`, p.BookingID, p.AmountMinor, p.Currency, p.Status, p.ProviderPaymentID, p.FailureReason,
	).Scan(&id)

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" &&
		pgErr.ConstraintName == "idx_payments_one_success_per_booking" {
		return uuid.Nil, fmt.Errorf("%w: booking %s", ErrDuplicateSuccessfulPayment, p.BookingID)
	}
	if isSerializationFailure(err) {
		return uuid.Nil, ErrSerializationFailure
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("insert payment for booking %s: %w", p.BookingID, err)
	}
	return id, nil
}
