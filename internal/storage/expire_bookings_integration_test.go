//go:build integration

package storage

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

const testExpireAfter = 15 * time.Minute

// makeOld moves a booking's created_at back by age, so it can be past the
// deadline without the test waiting for it.
func makeOld(t *testing.T, id uuid.UUID, age time.Duration) {
	t.Helper()
	exec(t, `UPDATE bookings SET created_at = now() - $2 * interval '1 second' WHERE booking_id = $1`,
		id, int(age.Seconds()))
}

func expiredIDs(expired []ExpiredBooking) []uuid.UUID {
	ids := make([]uuid.UUID, len(expired))
	for i, e := range expired {
		ids[i] = e.BookingID
	}
	return ids
}

type bookingState struct {
	status      string
	reason      *string
	cancelledAt *time.Time
}

func readBookingState(t *testing.T, id uuid.UUID) bookingState {
	t.Helper()
	var s bookingState
	if err := testPool.QueryRow(context.Background(),
		`SELECT booking_status, failure_reason, cancelled_at FROM bookings WHERE booking_id = $1`, id,
	).Scan(&s.status, &s.reason, &s.cancelledAt); err != nil {
		t.Fatalf("read booking %s: %v", id, err)
	}
	return s
}

// Only pending bookings past the deadline expire. A pending booking inside
// the deadline, and old bookings in every other status, are left alone:
// expiry must never touch a booking whose charge may be in flight.
func TestExpirePendingBookingsOnlyOldPending(t *testing.T) {
	resetDB(t)
	store := NewStore(testPool)

	old := insertBookingWithStatus(t, "pending")
	makeOld(t, old, 20*time.Minute)
	fresh := insertBookingWithStatus(t, "pending")
	makeOld(t, fresh, 10*time.Minute)

	others := map[uuid.UUID]string{}
	for _, status := range []string{"payment_pending", "confirmed", "cancelled"} {
		id := insertBookingWithStatus(t, status)
		makeOld(t, id, 20*time.Minute)
		others[id] = status
	}

	expired, err := store.ExpirePendingBookings(context.Background(), testExpireAfter, 10)
	if err != nil {
		t.Fatalf("expire: %v", err)
	}
	if len(expired) != 1 || expired[0].BookingID != old {
		t.Fatalf("expired: got %v, want only %s", expiredIDs(expired), old)
	}
	if e := expired[0]; e.UnitID != tenSeatUnitID || e.CustomerID != seedCustomerID || e.Qty != 2 {
		t.Errorf("fields: got %+v", e)
	}

	got := readBookingState(t, old)
	if got.status != "cancelled" {
		t.Errorf("expired booking status: got %q, want cancelled", got.status)
	}
	if got.reason == nil || *got.reason != "expired" {
		t.Errorf("failure_reason: got %v, want \"expired\"", got.reason)
	}
	if got.cancelledAt == nil {
		t.Error("cancelled_at: got NULL, want it set")
	}

	if s := readBookingState(t, fresh).status; s != "pending" {
		t.Errorf("booking inside the deadline: got %q, want pending", s)
	}
	for id, want := range others {
		if s := readBookingState(t, id).status; s != want {
			t.Errorf("old %s booking: got %q, want it unchanged", want, s)
		}
	}
}

// The oldest bookings expire first, and never more than limit per call.
func TestExpirePendingBookingsOldestFirstWithinLimit(t *testing.T) {
	resetDB(t)
	store := NewStore(testPool)

	oldest := insertBookingWithStatus(t, "pending")
	middle := insertBookingWithStatus(t, "pending")
	newest := insertBookingWithStatus(t, "pending")
	makeOld(t, oldest, 30*time.Minute)
	makeOld(t, middle, 25*time.Minute)
	makeOld(t, newest, 20*time.Minute)

	expired, err := store.ExpirePendingBookings(context.Background(), testExpireAfter, 2)
	if err != nil {
		t.Fatalf("expire: %v", err)
	}
	got := expiredIDs(expired)
	if len(got) != 2 || !containsAll(got, oldest, middle) {
		t.Errorf("expired: got %v, want the two oldest %s and %s", got, oldest, middle)
	}
	if s := readBookingState(t, newest).status; s != "pending" {
		t.Errorf("booking beyond the limit: got %q, want pending until the next call", s)
	}
}

// A booking expires once. The second call finds nothing, so a retried or
// repeated batch can never release the same seats twice.
func TestExpirePendingBookingsSecondCallFindsNothing(t *testing.T) {
	resetDB(t)
	store := NewStore(testPool)
	ctx := context.Background()

	id := insertBookingWithStatus(t, "pending")
	makeOld(t, id, 20*time.Minute)

	if first, err := store.ExpirePendingBookings(ctx, testExpireAfter, 10); err != nil || len(first) != 1 {
		t.Fatalf("first call: got %d bookings, err %v; want 1", len(first), err)
	}
	second, err := store.ExpirePendingBookings(ctx, testExpireAfter, 10)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if len(second) != 0 {
		t.Errorf("second call: got %v, want nothing", expiredIDs(second))
	}
}

// Two expirers running at the same moment never expire the same booking,
// and between them every expired booking is found.
func TestExpirePendingBookingsConcurrentCallsAreDisjoint(t *testing.T) {
	resetDB(t)
	store := NewStore(testPool)

	const total = 20
	for i := 0; i < total; i++ {
		makeOld(t, insertBookingWithStatus(t, "pending"), 20*time.Minute)
	}

	start := make(chan struct{})
	results := make([][]ExpiredBooking, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			results[w], errs[w] = store.ExpirePendingBookings(context.Background(), testExpireAfter, total)
		}(w)
	}
	close(start)
	wg.Wait()

	seen := map[uuid.UUID]int{}
	for w := 0; w < 2; w++ {
		if errs[w] != nil {
			t.Fatalf("expirer %d: %v", w, errs[w])
		}
		for _, e := range results[w] {
			seen[e.BookingID]++
		}
	}
	for id, n := range seen {
		if n > 1 {
			t.Errorf("booking %s expired %d times", id, n)
		}
	}
	if len(seen) != total {
		t.Errorf("expired between both: got %d, want all %d", len(seen), total)
	}
}
