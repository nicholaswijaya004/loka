//go:build integration

package payments

import (
	"context"
	"testing"
	"time"

	"github.com/nicholaswijaya004/loka/internal/booking"
	"github.com/nicholaswijaya004/loka/internal/storage"
	"github.com/nicholaswijaya004/loka/internal/testdb"
)

// declineProvider declines every charge.
type declineProvider struct{}

func (declineProvider) Charge(context.Context, ChargeRequest) (*ChargeResult, error) {
	return &ChargeResult{Outcome: ChargeDeclined, DeclineReason: "insufficient_funds"}, nil
}

// A declined booking's event says which unit got seats back and how many,
// read from the JSON keys as the cache invalidator will read them.
func TestWorkerDeclineEventCarriesUnitAndQty(t *testing.T) {
	testdb.Reset(t, testPool)
	ctx := context.Background()

	// Qty 3, not insertBooking's 1, so a hard-coded quantity can't pass.
	b := &storage.Booking{
		UnitID:        seedUnitID,
		CustomerID:    seedCustomerID,
		Qty:           3,
		VisitDateTime: time.Now().Add(24 * time.Hour),
		TotalMinor:    450_000_000,
		Currency:      "IDR",
		BookingStatus: booking.StatusPaymentPending,
	}
	if err := storage.NewStore(testPool).InsertBooking(ctx, b); err != nil {
		t.Fatalf("insert booking: %v", err)
	}
	// The seats are held, as they would be after a real booking.
	if _, err := testPool.Exec(ctx,
		`UPDATE inventory_units SET available_units = available_units - 3 WHERE unit_id = $1`, seedUnitID); err != nil {
		t.Fatalf("hold seats: %v", err)
	}

	stop := startWorker(t, declineProvider{}, 1)
	eventually(t, 5*time.Second, "the booking cancelled by the decline", func() bool {
		return statusOf(t, b.BookingID) == booking.StatusCancelled
	})
	stop()

	var version, qty int
	var unitID, reason string
	if err := testPool.QueryRow(ctx, `
		SELECT (payload->>'version')::int,
		       coalesce(payload->>'unit_id', ''),
		       coalesce((payload->>'qty')::int, 0),
		       payload->>'reason'
		FROM outbox_events
		WHERE aggregate_id = $1 AND event_type = 'booking.cancelled'`, b.BookingID,
	).Scan(&version, &unitID, &qty, &reason); err != nil {
		t.Fatalf("read cancelled event: %v", err)
	}
	if version != 2 {
		t.Errorf("version: got %d, want 2", version)
	}
	if unitID != seedUnitID.String() || qty != 3 {
		t.Errorf("unit_id %q qty %d, want %s and 3", unitID, qty, seedUnitID)
	}
	if reason != "insufficient_funds" {
		t.Errorf("reason: got %q, want insufficient_funds", reason)
	}
}
