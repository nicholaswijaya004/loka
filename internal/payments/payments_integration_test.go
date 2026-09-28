//go:build integration

package payments

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/nicholaswijaya004/loka/internal/booking"
	"github.com/nicholaswijaya004/loka/internal/consumer"
	"github.com/nicholaswijaya004/loka/internal/storage"
	"github.com/nicholaswijaya004/loka/internal/testdb"
)

// Seed data from scripts/seed.sql.
var (
	seedUnitID     = uuid.MustParse("22222222-2222-2222-2222-222222222222")
	seedCustomerID = uuid.MustParse("11111111-1111-1111-1111-111111111111")
)

func newHandler() *Handler {
	return New(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// insertBooking creates a booking directly in the given status, so each test
// can start from exactly the state it needs.
func insertBooking(t *testing.T, status string) uuid.UUID {
	t.Helper()
	b := &storage.Booking{
		UnitID:        seedUnitID,
		CustomerID:    seedCustomerID,
		Qty:           1,
		VisitDateTime: time.Now().Add(24 * time.Hour),
		TotalMinor:    150_000_000,
		Currency:      "IDR",
		BookingStatus: status,
	}
	if err := storage.NewStore(testPool).InsertBooking(context.Background(), b); err != nil {
		t.Fatalf("insert booking: %v", err)
	}
	return b.BookingID
}

func statusOf(t *testing.T, id uuid.UUID) string {
	t.Helper()
	var status string
	if err := testPool.QueryRow(context.Background(),
		`SELECT booking_status FROM bookings WHERE booking_id = $1`, id,
	).Scan(&status); err != nil {
		t.Fatalf("read status of %s: %v", id, err)
	}
	return status
}

func createdEvent(payload string) consumer.Event {
	return consumer.Event{ID: 1, Type: eventBookingCreated, Payload: []byte(payload)}
}

func payloadFor(id uuid.UUID) string {
	return fmt.Sprintf(`{"version":1,"booking_id":%q}`, id)
}

// handle runs the handler inside a transaction, as the consumer framework does.
func handle(t *testing.T, e consumer.Event) error {
	t.Helper()
	h := newHandler()
	return storage.NewStore(testPool).WithTx(context.Background(), func(tx *storage.Store) error {
		return h.Handle(context.Background(), tx, e)
	})
}

func TestHandleStartsPaymentForPendingBooking(t *testing.T) {
	testdb.Reset(t, testPool)
	id := insertBooking(t, booking.StatusPending)

	if err := handle(t, createdEvent(payloadFor(id))); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if got := statusOf(t, id); got != booking.StatusPaymentPending {
		t.Errorf("status: got %q, want %q", got, booking.StatusPaymentPending)
	}
}

// A booking that is no longer pending has nothing left for this handler to
// do. It must return nil (processed), not an error that Run would retry forever.
func TestHandleIgnoresBookingNoLongerPending(t *testing.T) {
	for _, status := range []string{booking.StatusCancelled, booking.StatusPaymentPending} {
		t.Run(status, func(t *testing.T) {
			testdb.Reset(t, testPool)
			id := insertBooking(t, status)

			if err := handle(t, createdEvent(payloadFor(id))); err != nil {
				t.Fatalf("handle: got %v, want nil — a status conflict is not retriable", err)
			}
			if got := statusOf(t, id); got != status {
				t.Errorf("status: got %q, want it unchanged at %q", got, status)
			}
		})
	}
}

func TestHandlePoisonCases(t *testing.T) {
	tests := []struct {
		name    string
		payload string
	}{
		{"unknown booking", payloadFor(uuid.New())},
		{"malformed JSON", `{"version":1,"booking_id":`},
		{"unsupported version", fmt.Sprintf(`{"version":2,"booking_id":%q}`, uuid.New())},
		{"missing booking_id", `{"version":1}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testdb.Reset(t, testPool)

			err := handle(t, createdEvent(tt.payload))
			if !errors.Is(err, consumer.ErrPoisonMessage) {
				t.Errorf("error: got %v, want one wrapping ErrPoisonMessage", err)
			}
		})
	}
}

// Other event types, including the saga's own later events, are not this
// handler's business: ignored, and the booking is untouched.
func TestHandleIgnoresOtherEventTypes(t *testing.T) {
	for _, eventType := range []string{"booking.confirmed", "booking.cancelled", "something.new"} {
		t.Run(eventType, func(t *testing.T) {
			testdb.Reset(t, testPool)
			id := insertBooking(t, booking.StatusPending)

			e := consumer.Event{ID: 1, Type: eventType, Payload: []byte(payloadFor(id))}
			if err := handle(t, e); err != nil {
				t.Fatalf("handle: got %v, want nil", err)
			}
			if got := statusOf(t, id); got != booking.StatusPending {
				t.Errorf("status: got %q, want it untouched at %q", got, booking.StatusPending)
			}
		})
	}
}
