//go:build integration

package booking

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/nicholaswijaya004/loka/internal/storage"
)

const testExpireAfter = 15 * time.Minute

var fiftySeatUnitID = uuid.MustParse("33333333-3333-3333-3333-333333333333")

func newTestExpirer(batchSize int) *Expirer {
	return NewExpirer(storage.NewStore(testPool), testLogger, ExpirerConfig{
		After:     testExpireAfter,
		BatchSize: batchSize,
		Interval:  time.Second,
	})
}

// createHeldBooking books through the real service, so the seats are really
// held and a booking.created event exists, as in production.
func createHeldBooking(t *testing.T, unitID uuid.UUID, qty int) uuid.UUID {
	t.Helper()
	svc := NewService(storeAdapter{storage.NewStore(testPool)}, testLogger, false, "single")
	b, err := svc.Create(context.Background(), unitID, testCustomerID, qty, testVisit)
	if err != nil {
		t.Fatalf("create booking: %v", err)
	}
	return b.BookingID
}

func execSQL(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// makeOld moves a booking's created_at back by age, so it can be past the
// deadline without the test waiting for it.
func makeOld(t *testing.T, id uuid.UUID, age time.Duration) {
	t.Helper()
	execSQL(t, `UPDATE bookings SET created_at = now() - $2 * interval '1 second' WHERE booking_id = $1`,
		id, int(age.Seconds()))
}

func availableUnits(t *testing.T, unitID uuid.UUID) int {
	t.Helper()
	return countRows(t, `SELECT available_units FROM inventory_units WHERE unit_id = $1`, unitID)
}

func expiredEvents(t *testing.T, id uuid.UUID) int {
	t.Helper()
	return countRows(t, `
		SELECT count(*) FROM outbox_events
		WHERE aggregate_type = 'booking' AND aggregate_id = $1
		  AND event_type = 'booking.cancelled'
		  AND payload->>'booking_id' = $1::text
		  AND payload->>'customer_id' = $2::text
		  AND payload->>'reason' = 'expired'`,
		id, testCustomerID)
}

func bookingStatus(t *testing.T, id uuid.UUID) string {
	t.Helper()
	var s string
	if err := testPool.QueryRow(context.Background(),
		`SELECT booking_status FROM bookings WHERE booking_id = $1`, id).Scan(&s); err != nil {
		t.Fatalf("read status: %v", err)
	}
	return s
}

// Expired bookings give their seats back, summed per unit, on every unit in
// the batch.
func TestExpirerReleasesSeatsOnEveryUnit(t *testing.T) {
	resetDB(t)
	var ids []uuid.UUID
	for _, qty := range []int{1, 2, 3} {
		ids = append(ids, createHeldBooking(t, testUnitID, qty))
	}
	ids = append(ids, createHeldBooking(t, fiftySeatUnitID, 4))
	for _, id := range ids {
		makeOld(t, id, 20*time.Minute)
	}
	if a, b := availableUnits(t, testUnitID), availableUnits(t, fiftySeatUnitID); a != 4 || b != 46 {
		t.Fatalf("before expiry: got %d and %d available, want 4 and 46", a, b)
	}

	n, err := newTestExpirer(50).RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if n != 4 {
		t.Errorf("expired: got %d, want 4", n)
	}
	if a := availableUnits(t, testUnitID); a != 10 {
		t.Errorf("ten-seat unit: got %d available, want 10", a)
	}
	if b := availableUnits(t, fiftySeatUnitID); b != 50 {
		t.Errorf("fifty-seat unit: got %d available, want 50", b)
	}
}

// Every expired booking gets exactly one booking.cancelled event with reason
// "expired", and no payment row: no charge was ever attempted.
func TestExpirerWritesOneEventAndNoPayment(t *testing.T) {
	resetDB(t)
	ids := []uuid.UUID{createHeldBooking(t, testUnitID, 1), createHeldBooking(t, testUnitID, 1)}
	for _, id := range ids {
		makeOld(t, id, 20*time.Minute)
	}

	if _, err := newTestExpirer(50).RunOnce(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	for _, id := range ids {
		if s := bookingStatus(t, id); s != StatusCancelled {
			t.Errorf("booking %s: got %q, want cancelled", id, s)
		}
		if n := expiredEvents(t, id); n != 1 {
			t.Errorf("booking %s: got %d expired events, want 1", id, n)
		}
	}
	if n := countRows(t, `SELECT count(*) FROM payments`); n != 0 {
		t.Errorf("payments: got %d, want 0 — an expired booking was never charged", n)
	}
}

// A pending booking inside the deadline, and an old one whose charge is in
// flight, keep their status and their seats.
func TestExpirerLeavesFreshAndChargingBookingsAlone(t *testing.T) {
	resetDB(t)
	fresh := createHeldBooking(t, testUnitID, 1)
	makeOld(t, fresh, 10*time.Minute)
	charging := createHeldBooking(t, testUnitID, 2)
	makeOld(t, charging, 20*time.Minute)
	execSQL(t, `UPDATE bookings SET booking_status = 'payment_pending' WHERE booking_id = $1`, charging)

	n, err := newTestExpirer(50).RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if n != 0 {
		t.Errorf("expired: got %d, want 0", n)
	}
	if s := bookingStatus(t, fresh); s != StatusPending {
		t.Errorf("fresh booking: got %q, want pending", s)
	}
	if s := bookingStatus(t, charging); s != StatusPaymentPending {
		t.Errorf("charging booking: got %q, want payment_pending", s)
	}
	if a := availableUnits(t, testUnitID); a != 7 {
		t.Errorf("available units: got %d, want 7 — both bookings still hold their seats", a)
	}
	if n := countRows(t, `SELECT count(*) FROM outbox_events WHERE event_type = 'booking.cancelled'`); n != 0 {
		t.Errorf("cancelled events: got %d, want 0", n)
	}
}

// Running again after a batch changes nothing: no second release, no second
// event.
func TestExpirerSecondRunChangesNothing(t *testing.T) {
	resetDB(t)
	id := createHeldBooking(t, testUnitID, 3)
	makeOld(t, id, 20*time.Minute)
	expirer := newTestExpirer(50)

	if n, err := expirer.RunOnce(context.Background()); err != nil || n != 1 {
		t.Fatalf("first run: got %d, err %v; want 1", n, err)
	}
	n, err := expirer.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if n != 0 {
		t.Errorf("second run expired %d, want 0", n)
	}
	if a := availableUnits(t, testUnitID); a != 10 {
		t.Errorf("available units: got %d, want 10", a)
	}
	if n := expiredEvents(t, id); n != 1 {
		t.Errorf("expired events: got %d, want 1", n)
	}
}

// If any write in the batch fails, none of it happens: the booking stays
// pending and keeps its seats. This fails if the transition, the release and
// the event are not in one transaction.
func TestExpirerWritesNothingWhenEventFails(t *testing.T) {
	resetDB(t)
	id := createHeldBooking(t, testUnitID, 2)
	makeOld(t, id, 20*time.Minute)

	// Make the outbox reject booking.cancelled, so the last write of the batch fails.
	execSQL(t, `ALTER TABLE outbox_events ADD CONSTRAINT test_reject_cancelled
	            CHECK (event_type <> 'booking.cancelled') NOT VALID`)
	t.Cleanup(func() {
		execSQL(t, `ALTER TABLE outbox_events DROP CONSTRAINT IF EXISTS test_reject_cancelled`)
	})

	if _, err := newTestExpirer(50).RunOnce(context.Background()); err == nil {
		t.Fatal("run: got nil, want the injected outbox error")
	}
	if s := bookingStatus(t, id); s != StatusPending {
		t.Errorf("status: got %q, want pending — the transition must roll back", s)
	}
	if a := availableUnits(t, testUnitID); a != 8 {
		t.Errorf("available units: got %d, want 8 — the release must roll back", a)
	}
}

// Two expirers running at once, with small batches spanning two units,
// release every seat exactly once and never fail. A deadlock (inconsistent
// unit lock order) or a double release (ErrOverRelease) shows up as an error.
func TestExpirerConcurrentRunsReleaseEachSeatOnce(t *testing.T) {
	resetDB(t)
	const perUnit = 10
	var ids []uuid.UUID
	for i := 0; i < perUnit; i++ {
		ids = append(ids, createHeldBooking(t, testUnitID, 1), createHeldBooking(t, fiftySeatUnitID, 1))
	}
	for _, id := range ids {
		makeOld(t, id, 20*time.Minute)
	}

	start := make(chan struct{})
	expired := make([]int, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			expirer := newTestExpirer(3)
			<-start
			for {
				n, err := expirer.RunOnce(context.Background())
				if err != nil {
					errs[w] = err
					return
				}
				if n == 0 {
					return
				}
				expired[w] += n
			}
		}(w)
	}
	close(start)
	wg.Wait()

	for w, err := range errs {
		if err != nil {
			t.Errorf("expirer %d: %v", w, err)
		}
	}
	if total := expired[0] + expired[1]; total != len(ids) {
		t.Errorf("expired between both: got %d, want %d", total, len(ids))
	}
	if a := availableUnits(t, testUnitID); a != 10 {
		t.Errorf("ten-seat unit: got %d available, want 10", a)
	}
	if b := availableUnits(t, fiftySeatUnitID); b != 50 {
		t.Errorf("fifty-seat unit: got %d available, want 50", b)
	}
	if n := countRows(t, `SELECT count(*) FROM outbox_events WHERE event_type = 'booking.cancelled'`); n != len(ids) {
		t.Errorf("cancelled events: got %d, want %d", n, len(ids))
	}
}
