//go:build integration

package booking

import (
	"context"
	"errors"
	"testing"

	"github.com/nicholaswijaya004/loka/internal/storage"
)

var errInjectedOutbox = errors.New("injected outbox failure")

// failOutboxStore behaves like the real store, except InsertOutboxEvent always
// fails. It also wraps the transaction-bound store it hands to fn, so the
// failure happens inside the transaction, after InsertBooking has run.
type failOutboxStore struct{ storeAdapter }

func (s failOutboxStore) InsertOutboxEvent(context.Context, *storage.Outbox) error {
	return errInjectedOutbox
}

func (s failOutboxStore) WithTx(ctx context.Context, fn func(Store) error) error {
	return s.storeAdapter.WithTx(ctx, func(tx Store) error {
		return fn(failOutboxStore{tx.(storeAdapter)})
	})
}

func (s failOutboxStore) WithSerializableTx(ctx context.Context, fn func(Store) error) error {
	return s.storeAdapter.WithSerializableTx(ctx, func(tx Store) error {
		return fn(failOutboxStore{tx.(storeAdapter)})
	})
}

func countRows(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

var allStrategies = []string{"single", "forupdate", "optimistic", "serializable"}

func TestCreateWritesBookingAndEventTogether(t *testing.T) {
	for _, strategy := range allStrategies {
		t.Run(strategy, func(t *testing.T) {
			resetDB(t)
			svc := NewService(storeAdapter{storage.NewStore(testPool)}, testLogger, false, strategy)

			b, err := svc.Create(context.Background(), testUnitID, testCustomerID, 1, testVisit)
			if err != nil {
				t.Fatalf("create: %v", err)
			}

			if n := countRows(t, `SELECT count(*) FROM bookings`); n != 1 {
				t.Errorf("bookings: got %d, want 1", n)
			}
			if n := countRows(t, `
				SELECT count(*) FROM outbox_events
				WHERE aggregate_type = $1 AND aggregate_id = $2 AND event_type = $3
				  AND published_at IS NULL
				  AND payload->>'booking_id' = $2::text`,
				bookingAggregate, b.BookingID, eventBookingCreated); n != 1 {
				t.Errorf("matching unpublished booking.created events: got %d, want 1", n)
			}
		})
	}
}

// If the event can't be written, the booking must not exist either. This is the
// test that fails if any strategy passes s.store instead of tx to
// insertBookingWithEvent: the booking would then commit on its own connection
// while the transaction rolls back.
func TestCreateWritesNeitherWhenEventFails(t *testing.T) {
	for _, strategy := range allStrategies {
		t.Run(strategy, func(t *testing.T) {
			resetDB(t)
			store := failOutboxStore{storeAdapter{storage.NewStore(testPool)}}
			svc := NewService(store, testLogger, false, strategy)

			_, err := svc.Create(context.Background(), testUnitID, testCustomerID, 1, testVisit)
			if !errors.Is(err, errInjectedOutbox) {
				t.Fatalf("error: got %v, want %v", err, errInjectedOutbox)
			}

			if n := countRows(t, `SELECT count(*) FROM bookings`); n != 0 {
				t.Errorf("bookings: got %d, want 0 — booking committed without its event", n)
			}
			if n := countRows(t, `SELECT count(*) FROM outbox_events`); n != 0 {
				t.Errorf("outbox events: got %d, want 0", n)
			}
			if n := countRows(t, `SELECT available_units FROM inventory_units WHERE unit_id = $1`, testUnitID); n != 10 {
				t.Errorf("available units: got %d, want 10 — the decrement must roll back", n)
			}
		})
	}
}
