//go:build integration

package booking

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// cancelledWire reads a booking's booking.cancelled event as a consumer in
// another service would: straight from the JSON keys, not through
// CancelledPayload, so a wrong tag can't hide behind a round trip.
func cancelledWire(t *testing.T, bookingID uuid.UUID) (version int, unitID string, qty int) {
	t.Helper()
	if err := testPool.QueryRow(context.Background(), `
		SELECT (payload->>'version')::int,
		       coalesce(payload->>'unit_id', ''),
		       coalesce((payload->>'qty')::int, 0)
		FROM outbox_events
		WHERE aggregate_id = $1 AND event_type = 'booking.cancelled'`, bookingID,
	).Scan(&version, &unitID, &qty); err != nil {
		t.Fatalf("read cancelled event of %s: %v", bookingID, err)
	}
	return version, unitID, qty
}

// An expired booking's event says which unit got seats back and how many,
// so the cache invalidator can drop that unit's entry. Two bookings on two
// units with different quantities: each event must carry its own booking's.
func TestExpirerCancelledEventCarriesUnitAndQty(t *testing.T) {
	resetDB(t)
	want := map[uuid.UUID]struct {
		unit uuid.UUID
		qty  int
	}{}
	for _, b := range []struct {
		unit uuid.UUID
		qty  int
	}{{testUnitID, 2}, {fiftySeatUnitID, 4}} {
		id := createHeldBooking(t, b.unit, b.qty)
		makeOld(t, id, 20*time.Minute)
		want[id] = b
	}

	if _, err := newTestExpirer(50).RunOnce(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	for id, w := range want {
		version, unitID, qty := cancelledWire(t, id)
		if version != 2 {
			t.Errorf("booking %s: version %d, want 2", id, version)
		}
		if unitID != w.unit.String() || qty != w.qty {
			t.Errorf("booking %s: unit_id %q qty %d, want %s and %d", id, unitID, qty, w.unit, w.qty)
		}
	}
}
