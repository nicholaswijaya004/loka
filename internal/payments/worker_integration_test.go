//go:build integration

package payments

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/nicholaswijaya004/loka/internal/booking"
	"github.com/nicholaswijaya004/loka/internal/storage"
	"github.com/nicholaswijaya004/loka/internal/testdb"
)

// fakeProvider succeeds every charge after a short delay, except bookings in
// slow: those block until release is closed or the caller gives up, like a
// lost response. It records how many charges ran at once.
type fakeProvider struct {
	slow    map[uuid.UUID]bool
	release chan struct{}

	mu       sync.Mutex
	inFlight int
	maxSeen  int
}

func newFakeProvider(slow ...uuid.UUID) *fakeProvider {
	p := &fakeProvider{slow: map[uuid.UUID]bool{}, release: make(chan struct{})}
	for _, id := range slow {
		p.slow[id] = true
	}
	return p
}

func (p *fakeProvider) Charge(ctx context.Context, req ChargeRequest) (*ChargeResult, error) {
	p.mu.Lock()
	p.inFlight++
	p.maxSeen = max(p.maxSeen, p.inFlight)
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.inFlight--
		p.mu.Unlock()
	}()

	if p.slow[req.BookingID] {
		select {
		case <-p.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	} else {
		select {
		case <-time.After(20 * time.Millisecond): // long enough for charges to overlap
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &ChargeResult{Outcome: ChargeSucceeded, ChargeID: "ch-" + req.BookingID.String()}, nil
}

func (p *fakeProvider) stats() (inFlight, maxSeen int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.inFlight, p.maxSeen
}

// startWorker runs a worker until the returned stop is called. stop returns
// only once Run has returned.
func startWorker(t *testing.T, provider Provider, maxInFlight int) (stop func()) {
	t.Helper()
	w := NewWorker(storage.NewStore(testPool), provider, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WorkerConfig{
			MaxInFlight:   maxInFlight,
			Interval:      50 * time.Millisecond,
			Lease:         30 * time.Second,
			EscalateAfter: time.Hour,
			BackoffBase:   time.Second,
			MaxBackoff:    time.Second,
		})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	return func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("run: %v", err)
		}
	}
}

func eventually(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not true after %s", what, timeout)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func allConfirmed(t *testing.T, ids []uuid.UUID) func() bool {
	return func() bool {
		for _, id := range ids {
			if statusOf(t, id) != booking.StatusConfirmed {
				return false
			}
		}
		return true
	}
}

// A charge that hangs (a lost response waiting for its timeout) holds only
// its own slot. The other bookings keep flowing through the free ones.
//
// With a batch that waits for all its charges, the hung charge would block
// every booking claimed after it until it finished.
func TestWorkerSlowChargeHoldsOnlyItsOwnSlot(t *testing.T) {
	testdb.Reset(t, testPool)

	hung := insertBooking(t, booking.StatusPaymentPending)
	// Oldest first: make sure the hung booking is in the very first claim.
	if _, err := testPool.Exec(context.Background(),
		`UPDATE bookings SET next_attempt_at = now() - interval '1 minute' WHERE booking_id = $1`, hung); err != nil {
		t.Fatalf("age hung booking: %v", err)
	}
	var others []uuid.UUID
	for range 6 {
		others = append(others, insertBooking(t, booking.StatusPaymentPending))
	}

	provider := newFakeProvider(hung)
	stop := startWorker(t, provider, 2)
	defer stop()

	eventually(t, 5*time.Second, "the other bookings confirmed while one charge hangs",
		allConfirmed(t, others))
	if s := statusOf(t, hung); s != booking.StatusPaymentPending {
		t.Fatalf("hung booking: got %q, want payment_pending until its charge answers", s)
	}

	close(provider.release)
	eventually(t, 5*time.Second, "the hung booking confirmed once its charge answers",
		allConfirmed(t, []uuid.UUID{hung}))
}

// The worker never runs more charges at once than MaxInFlight, but does use
// them: all bookings are charged, in parallel.
func TestWorkerKeepsAtMostMaxInFlightCharges(t *testing.T) {
	testdb.Reset(t, testPool)

	var ids []uuid.UUID
	for range 20 {
		ids = append(ids, insertBooking(t, booking.StatusPaymentPending))
	}

	provider := newFakeProvider()
	stop := startWorker(t, provider, 4)
	defer stop()

	eventually(t, 10*time.Second, "all bookings confirmed", allConfirmed(t, ids))
	if _, maxSeen := provider.stats(); maxSeen > 4 || maxSeen < 2 {
		t.Errorf("most charges at once: got %d, want 2..4", maxSeen)
	}
}

// On shutdown, Run returns only after the charges in flight have finished or
// given up, so main can close the pool without pulling it from under them.
func TestWorkerRunWaitsForChargesInFlight(t *testing.T) {
	testdb.Reset(t, testPool)

	hung := insertBooking(t, booking.StatusPaymentPending)
	provider := newFakeProvider(hung)
	stop := startWorker(t, provider, 2)

	eventually(t, 5*time.Second, "the hung charge in flight", func() bool {
		inFlight, _ := provider.stats()
		return inFlight == 1
	})
	stop()

	if inFlight, _ := provider.stats(); inFlight != 0 {
		t.Errorf("charges still in flight after Run returned: %d", inFlight)
	}
	if s := statusOf(t, hung); s != booking.StatusPaymentPending {
		t.Errorf("hung booking: got %q, want payment_pending: an unanswered charge is never decided", s)
	}
}
