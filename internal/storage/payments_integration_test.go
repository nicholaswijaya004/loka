//go:build integration

package storage

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

var seedCustomerID = uuid.MustParse("11111111-1111-1111-1111-111111111111")

func insertTestBooking(t *testing.T) uuid.UUID {
	t.Helper()
	b := &Booking{
		UnitID:        tenSeatUnitID,
		CustomerID:    seedCustomerID,
		Qty:           1,
		VisitDateTime: time.Now().Add(24 * time.Hour),
		TotalMinor:    150_000_000,
		Currency:      "IDR",
		BookingStatus: "payment_pending",
	}
	if err := NewStore(testPool).InsertBooking(context.Background(), b); err != nil {
		t.Fatalf("insert booking: %v", err)
	}
	return b.BookingID
}

func strPtr(s string) *string { return &s }

type paymentRow struct {
	status      string
	amount      int64
	providerID  *string
	reason      *string
	succeededAt *time.Time
	failedAt    *time.Time
}

func readPayment(t *testing.T, id uuid.UUID) paymentRow {
	t.Helper()
	var r paymentRow
	if err := testPool.QueryRow(context.Background(), `
		SELECT payment_status, amount_minor, provider_payment_id, failure_reason, succeeded_at, failed_at
		FROM payments WHERE payment_id = $1`, id,
	).Scan(&r.status, &r.amount, &r.providerID, &r.reason, &r.succeededAt, &r.failedAt); err != nil {
		t.Fatalf("read payment %s: %v", id, err)
	}
	return r
}

func TestInsertPaymentSucceeded(t *testing.T) {
	resetDB(t)
	store := NewStore(testPool)
	bookingID := insertTestBooking(t)

	id, err := store.InsertPayment(context.Background(), Payment{
		BookingID: bookingID, AmountMinor: 150_000_000, Currency: "IDR",
		Status: PaymentSucceeded, ProviderPaymentID: strPtr("ch_123"),
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	r := readPayment(t, id)
	if r.status != PaymentSucceeded || r.amount != 150_000_000 {
		t.Errorf("row: got status %q amount %d", r.status, r.amount)
	}
	if r.providerID == nil || *r.providerID != "ch_123" {
		t.Errorf("provider_payment_id: got %v, want ch_123", r.providerID)
	}
	if r.succeededAt == nil || r.failedAt != nil {
		t.Errorf("timestamps: succeeded_at=%v failed_at=%v, want only succeeded_at", r.succeededAt, r.failedAt)
	}
}

func TestInsertPaymentFailed(t *testing.T) {
	resetDB(t)
	store := NewStore(testPool)
	bookingID := insertTestBooking(t)

	id, err := store.InsertPayment(context.Background(), Payment{
		BookingID: bookingID, AmountMinor: 150_000_000, Currency: "IDR",
		Status: PaymentFailed, ProviderPaymentID: strPtr("ch_456"),
		FailureReason: strPtr("insufficient_funds"),
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	r := readPayment(t, id)
	if r.status != PaymentFailed {
		t.Errorf("status: got %q, want %q", r.status, PaymentFailed)
	}
	if r.reason == nil || *r.reason != "insufficient_funds" {
		t.Errorf("failure_reason: got %v", r.reason)
	}
	if r.failedAt == nil || r.succeededAt != nil {
		t.Errorf("timestamps: succeeded_at=%v failed_at=%v, want only failed_at", r.succeededAt, r.failedAt)
	}
}

// The backstop: even if a bug bypassed the status transition, the database
// refuses a second successful payment for the same booking.
func TestInsertPaymentRejectsSecondSuccess(t *testing.T) {
	resetDB(t)
	store := NewStore(testPool)
	bookingID := insertTestBooking(t)
	ctx := context.Background()
	p := Payment{BookingID: bookingID, AmountMinor: 150_000_000, Currency: "IDR", Status: PaymentSucceeded}

	if _, err := store.InsertPayment(ctx, p); err != nil {
		t.Fatalf("first success: %v", err)
	}
	_, err := store.InsertPayment(ctx, p)
	if !errors.Is(err, ErrDuplicateSuccessfulPayment) {
		t.Errorf("second success: got %v, want ErrDuplicateSuccessfulPayment", err)
	}
}

// The index is partial: it only covers successes. Failed attempts are history,
// and several may exist for one booking.
func TestInsertPaymentAllowsSeveralFailures(t *testing.T) {
	resetDB(t)
	store := NewStore(testPool)
	bookingID := insertTestBooking(t)
	ctx := context.Background()
	p := Payment{BookingID: bookingID, AmountMinor: 150_000_000, Currency: "IDR",
		Status: PaymentFailed, FailureReason: strPtr("insufficient_funds")}

	for i := 0; i < 2; i++ {
		if _, err := store.InsertPayment(ctx, p); err != nil {
			t.Fatalf("failure %d: %v", i+1, err)
		}
	}
}
