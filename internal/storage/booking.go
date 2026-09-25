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
		                      total_minor, currency, booking_status)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING booking_id, created_at, updated_at
	`, b.UnitID, b.CustomerID, b.Qty, b.VisitDateTime,
		b.TotalMinor, b.Currency, b.BookingStatus,
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
			   confirmed_at, cancelled_at, created_at, updated_at
		FROM bookings
		WHERE booking_id = $1
	`, id).Scan(&b.BookingID, &b.UnitID, &b.CustomerID, &b.Qty, &b.VisitDateTime,
		&b.TotalMinor, &b.Currency, &b.BookingStatus, &b.FailureReason,
		&b.ConfirmedAt, &b.CancelledAt, &b.CreatedAt, &b.UpdatedAt,
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
