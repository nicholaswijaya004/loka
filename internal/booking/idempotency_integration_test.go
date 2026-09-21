//go:build integration

package booking

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/nicholaswijaya004/loka/internal/storage"
)

func TestCreateIdempotentConcurrentSameKey(t *testing.T) {
	const workers = 50

	resetDB(t)
	store := storage.NewStore(testPool)
	// Idempotency sits in front of the booking work, so the strategy
	// doesn't matter here; single-statement is the default.
	svc := NewService(storeAdapter{store}, testLogger, false, "single")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	type result struct {
		booking  *storage.Booking
		replayed bool
		err      error
	}

	start := make(chan struct{})
	results := make(chan result, workers)
	var wg sync.WaitGroup

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			b, replayed, err := svc.CreateIdempotent(ctx, testKey, testHash,
				testUnitID, testCustomerID, 1, testVisit)
			results <- result{b, replayed, err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var fresh, replays, inFlight int
	var freshID uuid.UUID
	var replayIDs []uuid.UUID

	for r := range results {
		switch {
		case r.err == nil && !r.replayed:
			fresh++
			freshID = r.booking.BookingID
		case r.err == nil && r.replayed:
			replays++
			replayIDs = append(replayIDs, r.booking.BookingID)
		case errors.Is(r.err, ErrRequestInFlight):
			inFlight++
		default:
			t.Errorf("unexpected result: replayed=%v err=%v", r.replayed, r.err)
		}
	}

	t.Logf("fresh=%d replays=%d in-flight=%d", fresh, replays, inFlight)

	if fresh != 1 {
		t.Fatalf("fresh bookings: got %d, want exactly 1", fresh)
	}
	for _, id := range replayIDs {
		if id != freshID {
			t.Errorf("replay returned booking %v, want the original %v", id, freshID)
		}
	}

	var rows int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM bookings WHERE unit_id = $1`, testUnitID,
	).Scan(&rows); err != nil {
		t.Fatalf("count bookings: %v", err)
	}
	if rows != 1 {
		t.Errorf("booking rows: got %d, want 1", rows)
	}

	unit, err := store.GetInventoryUnit(ctx, testUnitID)
	if err != nil {
		t.Fatalf("read unit: %v", err)
	}
	if unit.AvailableUnits != 9 {
		t.Errorf("available units: got %d, want 9 — inventory consumed more than once", unit.AvailableUnits)
	}

	key, err := store.GetIdempotencyKey(ctx, testKey)
	if err != nil {
		t.Fatalf("read idempotency key: %v", err)
	}
	if key.State != "completed" {
		t.Errorf("key state: got %q, want %q", key.State, "completed")
	}
	if key.BookingID == nil || *key.BookingID != freshID {
		t.Errorf("key booking id: got %v, want %v", key.BookingID, freshID)
	}

	// A retry arriving after the winner finished must replay, not re-book.
	b, replayed, err := svc.CreateIdempotent(ctx, testKey, testHash,
		testUnitID, testCustomerID, 1, testVisit)
	if err != nil {
		t.Fatalf("late retry: %v", err)
	}
	if !replayed {
		t.Error("late retry: got a fresh booking, want a replay")
	}
	if b.BookingID != freshID {
		t.Errorf("late retry: got booking %v, want %v", b.BookingID, freshID)
	}
}
