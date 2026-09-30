//go:build integration

package storage

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

const (
	testLease       = 30 * time.Second
	testBackoffBase = 5 * time.Second
	testMaxBackoff  = 60 * time.Second
)

func insertBookingWithStatus(t *testing.T, status string) uuid.UUID {
	t.Helper()
	b := &Booking{
		UnitID:        tenSeatUnitID,
		CustomerID:    seedCustomerID,
		Qty:           2,
		VisitDateTime: time.Now().Add(24 * time.Hour),
		TotalMinor:    300_000_000,
		Currency:      "IDR",
		BookingStatus: status,
	}
	if err := NewStore(testPool).InsertBooking(context.Background(), b); err != nil {
		t.Fatalf("insert booking: %v", err)
	}
	return b.BookingID
}

func exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

func claimedIDs(due []DuePayment) []uuid.UUID {
	ids := make([]uuid.UUID, len(due))
	for i, d := range due {
		ids[i] = d.BookingID
	}
	return ids
}

// Only payment_pending bookings are claimed, with every field the worker needs.
func TestClaimDuePaymentsOnlyPaymentPending(t *testing.T) {
	resetDB(t)
	store := NewStore(testPool)
	want := insertBookingWithStatus(t, "payment_pending")
	insertBookingWithStatus(t, "pending")
	insertBookingWithStatus(t, "confirmed")
	insertBookingWithStatus(t, "cancelled")

	due, err := store.ClaimDuePayments(context.Background(), 10, testLease, testBackoffBase, testMaxBackoff)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(due) != 1 || due[0].BookingID != want {
		t.Fatalf("claimed: got %v, want only %s", claimedIDs(due), want)
	}

	d := due[0]
	if d.UnitID != tenSeatUnitID || d.CustomerID != seedCustomerID ||
		d.Qty != 2 || d.TotalMinor != 300_000_000 || d.Currency != "IDR" {
		t.Errorf("fields: got %+v", d)
	}
}

// A claimed booking is invisible to other claims until its lease runs out.
func TestClaimDuePaymentsLeaseHidesBooking(t *testing.T) {
	resetDB(t)
	store := NewStore(testPool)
	ctx := context.Background()
	insertBookingWithStatus(t, "payment_pending")

	first, err := store.ClaimDuePayments(ctx, 10, testLease, testBackoffBase, testMaxBackoff)
	if err != nil || len(first) != 1 {
		t.Fatalf("first claim: got %d bookings, err %v; want 1", len(first), err)
	}

	second, err := store.ClaimDuePayments(ctx, 10, testLease, testBackoffBase, testMaxBackoff)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if len(second) != 0 {
		t.Errorf("second claim during the lease: got %v, want none", claimedIDs(second))
	}
}

// When the lease has passed, for example because the worker crashed, the
// booking is due again and another claim picks it up.
func TestClaimDuePaymentsExpiredLeaseIsDueAgain(t *testing.T) {
	resetDB(t)
	store := NewStore(testPool)
	ctx := context.Background()
	id := insertBookingWithStatus(t, "payment_pending")

	if _, err := store.ClaimDuePayments(ctx, 10, testLease, testBackoffBase, testMaxBackoff); err != nil {
		t.Fatalf("first claim: %v", err)
	}

	// Simulate the lease running out, without sleeping in the test.
	exec(t, `UPDATE bookings SET next_attempt_at = now() - interval '1 second' WHERE booking_id = $1`, id)

	again, err := store.ClaimDuePayments(ctx, 10, testLease, testBackoffBase, testMaxBackoff)
	if err != nil {
		t.Fatalf("claim after lease: %v", err)
	}
	if len(again) != 1 || again[0].BookingID != id {
		t.Errorf("after the lease: got %v, want %s", claimedIDs(again), id)
	}
}

// Claiming must not touch updated_at: escalation measures time in
// payment_pending from it, and repeated claims would hide a stuck payment.
func TestClaimDuePaymentsDoesNotTouchUpdatedAt(t *testing.T) {
	resetDB(t)
	store := NewStore(testPool)
	id := insertBookingWithStatus(t, "payment_pending")

	entered := time.Now().Add(-10 * time.Minute).Truncate(time.Microsecond)
	exec(t, `UPDATE bookings SET updated_at = $2 WHERE booking_id = $1`, id, entered)

	due, err := store.ClaimDuePayments(context.Background(), 10, testLease, testBackoffBase, testMaxBackoff)
	if err != nil || len(due) != 1 {
		t.Fatalf("claim: got %d bookings, err %v; want 1", len(due), err)
	}
	if !due[0].PendingSince.Equal(entered) {
		t.Errorf("PendingSince: got %v, want %v", due[0].PendingSince, entered)
	}

	var updatedAt time.Time
	if err := testPool.QueryRow(context.Background(),
		`SELECT updated_at FROM bookings WHERE booking_id = $1`, id).Scan(&updatedAt); err != nil {
		t.Fatalf("read updated_at: %v", err)
	}
	if !updatedAt.Equal(entered) {
		t.Errorf("updated_at changed by the claim: got %v, want %v", updatedAt, entered)
	}
}

// The longest-waiting bookings are claimed first, and never more than limit.
func TestClaimDuePaymentsOldestFirstWithinLimit(t *testing.T) {
	resetDB(t)
	store := NewStore(testPool)

	oldest := insertBookingWithStatus(t, "payment_pending")
	middle := insertBookingWithStatus(t, "payment_pending")
	newest := insertBookingWithStatus(t, "payment_pending")
	exec(t, `UPDATE bookings SET next_attempt_at = now() - interval '3 minutes' WHERE booking_id = $1`, oldest)
	exec(t, `UPDATE bookings SET next_attempt_at = now() - interval '2 minutes' WHERE booking_id = $1`, middle)
	exec(t, `UPDATE bookings SET next_attempt_at = now() - interval '1 minute'  WHERE booking_id = $1`, newest)

	due, err := store.ClaimDuePayments(context.Background(), 2, testLease, testBackoffBase, testMaxBackoff)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	got := claimedIDs(due)
	if len(got) != 2 || !containsAll(got, oldest, middle) {
		t.Errorf("claimed: got %v, want the two oldest %s and %s", got, oldest, middle)
	}
}

// Two workers claiming at the same moment never get the same booking, and
// between them every due booking is claimed.
func TestClaimDuePaymentsConcurrentClaimsAreDisjoint(t *testing.T) {
	resetDB(t)
	store := NewStore(testPool)

	const total = 20
	for i := 0; i < total; i++ {
		insertBookingWithStatus(t, "payment_pending")
	}

	start := make(chan struct{})
	results := make([][]DuePayment, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			results[w], errs[w] = store.ClaimDuePayments(context.Background(), total, testLease, testBackoffBase, testMaxBackoff)
		}(w)
	}
	close(start)
	wg.Wait()

	seen := map[uuid.UUID]int{}
	for w := 0; w < 2; w++ {
		if errs[w] != nil {
			t.Fatalf("worker %d: %v", w, errs[w])
		}
		for _, d := range results[w] {
			seen[d.BookingID]++
		}
	}
	for id, n := range seen {
		if n > 1 {
			t.Errorf("booking %s claimed %d times", id, n)
		}
	}
	if len(seen) != total {
		t.Errorf("claimed between both workers: got %d, want all %d", len(seen), total)
	}
}

func containsAll(ids []uuid.UUID, want ...uuid.UUID) bool {
	set := map[uuid.UUID]bool{}
	for _, id := range ids {
		set[id] = true
	}
	for _, w := range want {
		if !set[w] {
			return false
		}
	}
	return true
}

func paymentAttempts(t *testing.T, id uuid.UUID) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(),
		`SELECT payment_attempts FROM bookings WHERE booking_id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("read payment_attempts: %v", err)
	}
	return n
}

// secondsUntilNextAttempt measures the delay the claim scheduled, using the
// database's clock so test and database can't disagree.
func secondsUntilNextAttempt(t *testing.T, id uuid.UUID) float64 {
	t.Helper()
	var s float64
	if err := testPool.QueryRow(context.Background(),
		`SELECT EXTRACT(EPOCH FROM next_attempt_at - now()) FROM bookings WHERE booking_id = $1`, id,
	).Scan(&s); err != nil {
		t.Fatalf("read next_attempt_at: %v", err)
	}
	return s
}

// Each claim counts as one attempt.
func TestClaimDuePaymentsCountsAttempts(t *testing.T) {
	resetDB(t)
	store := NewStore(testPool)
	ctx := context.Background()
	id := insertBookingWithStatus(t, "payment_pending")

	for want := 1; want <= 2; want++ {
		if _, err := store.ClaimDuePayments(ctx, 10, testLease, testBackoffBase, testMaxBackoff); err != nil {
			t.Fatalf("claim %d: %v", want, err)
		}
		if got := paymentAttempts(t, id); got != want {
			t.Errorf("after claim %d: payment_attempts = %d, want %d", want, got, want)
		}
		exec(t, `UPDATE bookings SET next_attempt_at = now() - interval '1 second' WHERE booking_id = $1`, id)
	}
}

// The next attempt is always at least the full lease away, and at most
// lease + the capped backoff, whatever the attempt count.
func TestClaimDuePaymentsDelayStaysWithinBounds(t *testing.T) {
	const slack = 1.0 // seconds: time passing between the claim and our read
	lease := testLease.Seconds()

	tests := []struct {
		name     string
		attempts int
		maxDelay float64
	}{
		{"first attempt", 0, lease + testBackoffBase.Seconds()}, // 30 + 5×2⁰
		{"many attempts", 10, lease + testMaxBackoff.Seconds()}, // 5×2¹⁰ is capped at 60
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetDB(t)
			store := NewStore(testPool)
			id := insertBookingWithStatus(t, "payment_pending")
			exec(t, `UPDATE bookings SET payment_attempts = $2 WHERE booking_id = $1`, id, tt.attempts)

			if _, err := store.ClaimDuePayments(context.Background(), 10, testLease, testBackoffBase, testMaxBackoff); err != nil {
				t.Fatalf("claim: %v", err)
			}

			delay := secondsUntilNextAttempt(t, id)
			if delay < lease-slack || delay > tt.maxDelay {
				t.Errorf("delay: got %.1fs, want between %.0fs and %.0fs", delay, lease, tt.maxDelay)
			}
		})
	}
}

// The backoff really grows: after many attempts, delays spread well beyond
// what a first attempt could ever get. Checked over many bookings, because a
// single random draw could land low by chance.
func TestClaimDuePaymentsBackoffGrowsWithAttempts(t *testing.T) {
	resetDB(t)
	store := NewStore(testPool)

	const n = 20
	ids := make([]uuid.UUID, n)
	for i := range ids {
		ids[i] = insertBookingWithStatus(t, "payment_pending")
	}
	exec(t, `UPDATE bookings SET payment_attempts = 10 WHERE booking_status = 'payment_pending'`)

	if _, err := store.ClaimDuePayments(context.Background(), n, testLease, testBackoffBase, testMaxBackoff); err != nil {
		t.Fatalf("claim: %v", err)
	}

	firstAttemptMax := (testLease + testBackoffBase).Seconds() // 35s
	for _, id := range ids {
		if secondsUntilNextAttempt(t, id) > firstAttemptMax {
			return // at least one delay exceeds anything a first attempt could get
		}
	}
	t.Errorf("none of %d delays exceeded %.0fs: the backoff isn't growing", n, firstAttemptMax)
}
