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
