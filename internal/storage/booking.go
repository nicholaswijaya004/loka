package storage

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type Booking struct {
	BookingID     uuid.UUID
	UnitID        uuid.UUID
	CustomerID    uuid.UUID
	Qty           int
	VisitDateTime time.Time
	TotalMinor    int64
	Currency      string
	BookingStatus string
	FailureReason *string
	ConfirmedAt   *time.Time
	CancelledAt   *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
	TraceContext  map[string]string
}

type ExpiredBooking struct {
	BookingID    uuid.UUID
	UnitID       uuid.UUID
	CustomerID   uuid.UUID
	Qty          int
	TraceContext map[string]string
}

var failAfterDecrement = func() float64 {
	f, err := strconv.ParseFloat(os.Getenv("FAIL_AFTER_DECREMENT"), 64)
	if err != nil {
		return 0
	}
	return f
}()

func (s *Store) InsertBooking(ctx context.Context, b *Booking) error {
	if failAfterDecrement > 0 && rand.Float64() < failAfterDecrement {
		return fmt.Errorf("injected failure: insert booking after decrement")
	}

	err := s.db.QueryRow(ctx, `
		INSERT INTO bookings (unit_id, customer_id, qty, visit_date_time,
		                      total_minor, currency, booking_status, trace_context)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING booking_id, created_at, updated_at
	`, b.UnitID, b.CustomerID, b.Qty, b.VisitDateTime,
		b.TotalMinor, b.Currency, b.BookingStatus, b.TraceContext,
	).Scan(&b.BookingID, &b.CreatedAt, &b.UpdatedAt)
	if isSerializationFailure(err) {
		return ErrSerializationFailure
	}
	if err != nil {
		return fmt.Errorf("failed to insert booking: %w", err)
	}
	return nil
}

func (s *Store) GetBooking(ctx context.Context, id uuid.UUID) (*Booking, error) {
	var b Booking
	err := s.db.QueryRow(ctx, `
		SELECT booking_id, unit_id, customer_id, qty, visit_date_time,
			   total_minor, currency, booking_status, failure_reason,
			   confirmed_at, cancelled_at, created_at, updated_at, trace_context
		FROM bookings
		WHERE booking_id = $1
	`, id).Scan(&b.BookingID, &b.UnitID, &b.CustomerID, &b.Qty, &b.VisitDateTime,
		&b.TotalMinor, &b.Currency, &b.BookingStatus, &b.FailureReason,
		&b.ConfirmedAt, &b.CancelledAt, &b.CreatedAt, &b.UpdatedAt, &b.TraceContext,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrBookingNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get booking: %w", err)
	}
	return &b, nil
}

func (s *Store) TransitionBookingStatus(ctx context.Context, id uuid.UUID, from, to string, failureReason *string) error {
	tag, err := s.db.Exec(ctx, `
		UPDATE bookings
		SET booking_status = $3,
		    updated_at     = now(),
		    confirmed_at   = CASE WHEN $3::text = 'confirmed' THEN now() ELSE confirmed_at END,
		    cancelled_at   = CASE WHEN $3::text = 'cancelled' THEN now() ELSE cancelled_at END,
		    failure_reason = COALESCE($4::text, failure_reason)
		WHERE booking_id = $1 AND booking_status = $2
	`, id, from, to, failureReason)
	if err != nil {
		return fmt.Errorf("transition booking %s %s→%s: %w", id, from, to, err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}

	// Zero rows: either the booking is in another status, or it doesn't exist.
	var exists bool
	if err := s.db.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM bookings WHERE booking_id = $1)`, id,
	).Scan(&exists); err != nil {
		return fmt.Errorf("transition booking %s: check existence: %w", id, err)
	}
	if !exists {
		return ErrBookingNotFound
	}
	return fmt.Errorf("%w: booking %s is not %s", ErrStatusConflict, id, from)
}

// ExpirePendingBookings cancels up to limit pending bookings created more than
// olderThan ago, oldest first, with reason "expired". It does not release
// seats or write events: run it in the same transaction as both.
//
// The deadline is checked against Postgres's clock, the one that wrote
// created_at. SKIP LOCKED lets concurrent expirers take disjoint batches.
func (s *Store) ExpirePendingBookings(ctx context.Context, olderThan time.Duration, limit int) ([]ExpiredBooking, error) {
	rows, err := s.db.Query(ctx, `
		WITH expired AS (
			SELECT booking_id
			FROM bookings
			WHERE booking_status = 'pending'
			  AND created_at < now() - $1 * interval '1 second'
			ORDER BY created_at
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		)
		UPDATE bookings b
		SET booking_status = 'cancelled',
		    cancelled_at   = now(),
		    updated_at     = now(),
		    failure_reason = 'expired'
		FROM expired
		WHERE b.booking_id = expired.booking_id
		  AND b.booking_status = 'pending'
		RETURNING b.booking_id, b.unit_id, b.customer_id, b.qty, b.trace_context
	`, int(olderThan.Seconds()), limit)
	if err != nil {
		return nil, fmt.Errorf("expire pending bookings: %w", err)
	}
	defer rows.Close()

	var expired []ExpiredBooking
	for rows.Next() {
		var e ExpiredBooking
		if err := rows.Scan(&e.BookingID, &e.UnitID, &e.CustomerID, &e.Qty, &e.TraceContext); err != nil {
			return nil, fmt.Errorf("scan expired booking: %w", err)
		}
		expired = append(expired, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read expired bookings: %w", err)
	}
	return expired, nil
}
