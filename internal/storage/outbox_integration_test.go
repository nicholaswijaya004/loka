//go:build integration

package storage

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// insertTestEvent writes one unpublished outbox event and returns its id.
func insertTestEvent(t *testing.T, s *Store) int64 {
	t.Helper()
	e := &Outbox{
		AggregateType: "booking",
		AggregateID:   uuid.New(),
		EventType:     "booking.created",
		Payload:       []byte(`{"test":true}`),
	}
	if err := s.InsertOutboxEvent(context.Background(), e); err != nil {
		t.Fatalf("insert outbox event: %v", err)
	}
	return e.ID
}

func isPublished(t *testing.T, id int64) bool {
	t.Helper()
	var published bool
	if err := testPool.QueryRow(context.Background(),
		`SELECT published_at IS NOT NULL FROM outbox_events WHERE id = $1`, id,
	).Scan(&published); err != nil {
		t.Fatalf("read event %d: %v", id, err)
	}
	return published
}

func ids(events []Outbox) []int64 {
	out := make([]int64, len(events))
	for i, e := range events {
		out[i] = e.ID
	}
	return out
}

func TestFetchUnpublishedReturnsOnlyUnpublishedOldestFirst(t *testing.T) {
	resetDB(t)
	store := NewStore(testPool)
	ctx := context.Background()

	first := insertTestEvent(t, store)
	middle := insertTestEvent(t, store)
	last := insertTestEvent(t, store)

	if err := store.MarkOutboxEventsPublished(ctx, []int64{middle}); err != nil {
		t.Fatalf("mark: %v", err)
	}

	var got []Outbox
	err := store.WithTx(ctx, func(tx *Store) error {
		var err error
		got, err = tx.FetchUnpublishedOutboxEvents(ctx, 10)
		return err
	})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}

	want := []int64{first, last}
	if g := ids(got); len(g) != 2 || g[0] != want[0] || g[1] != want[1] {
		t.Errorf("fetched ids: got %v, want %v (unpublished only, in id order)", g, want)
	}
}

func TestFetchUnpublishedRespectsLimit(t *testing.T) {
	resetDB(t)
	store := NewStore(testPool)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		insertTestEvent(t, store)
	}

	var got []Outbox
	err := store.WithTx(ctx, func(tx *Store) error {
		var err error
		got, err = tx.FetchUnpublishedOutboxEvents(ctx, 3)
		return err
	})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("fetched: got %d events, want 3", len(got))
	}
}

func TestMarkOutboxEventsPublished(t *testing.T) {
	resetDB(t)
	store := NewStore(testPool)
	ctx := context.Background()

	a := insertTestEvent(t, store)
	b := insertTestEvent(t, store)
	c := insertTestEvent(t, store)

	if err := store.MarkOutboxEventsPublished(ctx, []int64{a, c}); err != nil {
		t.Fatalf("mark: %v", err)
	}

	if !isPublished(t, a) || !isPublished(t, c) {
		t.Error("marked events should be published")
	}
	if isPublished(t, b) {
		t.Error("unmarked event b was published")
	}

	// Marking an already-published event updates 0 rows, which must be an error.
	if err := store.MarkOutboxEventsPublished(ctx, []int64{a}); err == nil {
		t.Error("re-marking a published event: got nil, want a row-count error")
	}

	// An empty batch is a no-op, not an error.
	if err := store.MarkOutboxEventsPublished(ctx, nil); err != nil {
		t.Errorf("empty batch: got %v, want nil", err)
	}
}

func TestRecordOutboxFailure(t *testing.T) {
	resetDB(t)
	store := NewStore(testPool)
	ctx := context.Background()

	id := insertTestEvent(t, store)

	if err := store.RecordOutboxFailure(ctx, id, "broker unavailable"); err != nil {
		t.Fatalf("first failure: %v", err)
	}
	if err := store.RecordOutboxFailure(ctx, id, "timeout"); err != nil {
		t.Fatalf("second failure: %v", err)
	}

	var attempts int
	var lastErr *string
	if err := testPool.QueryRow(ctx,
		`SELECT attempt_number, error FROM outbox_events WHERE id = $1`, id,
	).Scan(&attempts, &lastErr); err != nil {
		t.Fatalf("read event: %v", err)
	}
	if attempts != 2 {
		t.Errorf("attempt_number: got %d, want 2", attempts)
	}
	if lastErr == nil || *lastErr != "timeout" {
		t.Errorf("error: got %v, want the latest message %q", lastErr, "timeout")
	}
	if isPublished(t, id) {
		t.Error("a failed event must stay unpublished so it is retried")
	}

	if err := store.RecordOutboxFailure(ctx, 999999, "x"); err == nil {
		t.Error("unknown event: got nil, want a not-found error")
	}
}

// TestFetchUnpublishedSkipsLockedRows proves two relays never take the same
// events, and that the second one skips locked rows instead of waiting.
func TestFetchUnpublishedSkipsLockedRows(t *testing.T) {
	resetDB(t)
	store := NewStore(testPool)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	for i := 0; i < 4; i++ {
		insertTestEvent(t, store)
	}

	firstIDs := make(chan []int64, 1)
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	defer releaseOnce()
	firstDone := make(chan error, 1)

	// Relay 1: fetch 2 events and hold the transaction (and its locks) open.
	go func() {
		firstDone <- store.WithTx(ctx, func(tx *Store) error {
			events, err := tx.FetchUnpublishedOutboxEvents(ctx, 2)
			if err != nil {
				return err
			}
			firstIDs <- ids(events)
			return waitFor(ctx, release)
		})
	}()

	locked := recv(t, ctx, firstIDs, "relay 1 fetch")
	if len(locked) != 2 {
		t.Fatalf("relay 1: got %d events, want 2", len(locked))
	}

	// Relay 2: must return promptly with only the rows relay 1 didn't lock.
	// A short deadline turns "waited on the lock" into a clear failure.
	fetchCtx, fetchCancel := context.WithTimeout(ctx, 2*time.Second)
	defer fetchCancel()

	var second []Outbox
	start := time.Now()
	err := store.WithTx(fetchCtx, func(tx *Store) error {
		var err error
		second, err = tx.FetchUnpublishedOutboxEvents(fetchCtx, 10)
		return err
	})
	if err != nil {
		t.Fatalf("relay 2 fetch (did it wait on relay 1's locks?): %v", err)
	}
	t.Logf("relay 2 fetched %v in %v", ids(second), time.Since(start))

	if len(second) != 2 {
		t.Errorf("relay 2: got %d events, want the 2 unlocked ones", len(second))
	}
	lockedSet := map[int64]bool{locked[0]: true, locked[1]: true}
	for _, id := range ids(second) {
		if lockedSet[id] {
			t.Errorf("relay 2 fetched event %d, which relay 1 had locked", id)
		}
	}

	// Once relay 1 commits without marking anything, its rows are free again.
	releaseOnce()
	if err := recv(t, ctx, firstDone, "relay 1 commit"); err != nil {
		t.Fatalf("relay 1: %v", err)
	}

	var after []Outbox
	if err := store.WithTx(ctx, func(tx *Store) error {
		var err error
		after, err = tx.FetchUnpublishedOutboxEvents(ctx, 10)
		return err
	}); err != nil {
		t.Fatalf("fetch after release: %v", err)
	}
	if len(after) != 4 {
		t.Errorf("after release: got %d events, want all 4 (still unpublished)", len(after))
	}
}
