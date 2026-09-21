//go:build integration

package booking

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nicholaswijaya004/loka/internal/storage"
)

// storeAdapter mirrors the one in cmd/api: it rewraps the tx-bound
// *storage.Store so fn receives a booking.Store.
type storeAdapter struct {
	*storage.Store
}

func (a storeAdapter) WithTx(ctx context.Context, fn func(Store) error) error {
	return a.Store.WithTx(ctx, func(tx *storage.Store) error {
		return fn(storeAdapter{tx})
	})
}

func (a storeAdapter) WithSerializableTx(ctx context.Context, fn func(Store) error) error {
	return a.Store.WithSerializableTx(ctx, func(tx *storage.Store) error {
		return fn(storeAdapter{tx})
	})
}

func TestCreateUnderContentionAllStrategies(t *testing.T) {
	const (
		workers = 50
		seats   = 10 // testUnitID is the 10-seat unit
	)

	for _, strategy := range []string{"single", "forupdate", "optimistic", "serializable"} {
		t.Run(strategy, func(t *testing.T) {
			resetDB(t)
			store := storage.NewStore(testPool)
			svc := NewService(storeAdapter{store}, testLogger, false, strategy)

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			start := make(chan struct{})
			errs := make(chan error, workers)
			var wg sync.WaitGroup

			for i := 0; i < workers; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start // every goroutine waits here, then all go at once
					_, err := svc.Create(ctx, testUnitID, testCustomerID, 1, testVisit)
					errs <- err
				}()
			}
			close(start)
			wg.Wait()
			close(errs)

			var booked, soldOut int
			for err := range errs {
				switch {
				case err == nil:
					booked++
				case errors.Is(err, ErrSoldOut):
					soldOut++
				default:
					t.Errorf("unexpected error: %v", err)
				}
			}

			if booked != seats {
				t.Errorf("booked: got %d, want %d", booked, seats)
			}
			if soldOut != workers-seats {
				t.Errorf("sold out: got %d, want %d", soldOut, workers-seats)
			}

			unit, err := store.GetInventoryUnit(ctx, testUnitID)
			if err != nil {
				t.Fatalf("read unit: %v", err)
			}
			if unit.AvailableUnits != 0 {
				t.Errorf("available units: got %d, want 0", unit.AvailableUnits)
			}

			var bookedSeats int
			if err := testPool.QueryRow(ctx,
				`SELECT COALESCE(sum(qty), 0) FROM bookings WHERE unit_id = $1`, testUnitID,
			).Scan(&bookedSeats); err != nil {
				t.Fatalf("sum bookings: %v", err)
			}
			if bookedSeats+unit.AvailableUnits != seats {
				t.Errorf("invariant: %d booked + %d available != %d", bookedSeats, unit.AvailableUnits, seats)
			}

			t.Logf("retries: %d", svc.Retries())
		})
	}
}
