//go:build integration

package relay

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/nicholaswijaya004/loka/internal/storage"
	"github.com/nicholaswijaya004/loka/internal/testdb"
)

var testLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

var errInjectedPublish = errors.New("injected publish failure")

// fakePublisher records what it was asked to publish and fails on chosen ids.
// The mutex matters only for the Run test, where the relay runs in a goroutine.
type fakePublisher struct {
	mu        sync.Mutex
	failOn    map[int64]bool
	calls     int
	published []int64
}

var _ Publisher = (*fakePublisher)(nil)

func (f *fakePublisher) Publish(_ context.Context, e storage.Outbox) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.failOn[e.ID] {
		return errInjectedPublish
	}
	f.published = append(f.published, e.ID)
	return nil
}

func (f *fakePublisher) snapshot() (calls int, published []int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, append([]int64(nil), f.published...)
}

func insertEvents(t *testing.T, n int) []int64 {
	t.Helper()
	store := storage.NewStore(testPool)
	ids := make([]int64, n)
	for i := range ids {
		e := &storage.Outbox{
			AggregateType: "booking",
			AggregateID:   uuid.New(),
			EventType:     "booking.created",
			Payload:       []byte(`{"test":true}`),
		}
		if err := store.InsertOutboxEvent(context.Background(), e); err != nil {
			t.Fatalf("insert event: %v", err)
		}
		ids[i] = e.ID
	}
	return ids
}

type eventState struct {
	attempts  int
	lastErr   *string
	published bool
}

func stateOf(t *testing.T, id int64) eventState {
	t.Helper()
	var s eventState
	if err := testPool.QueryRow(context.Background(), `
		SELECT attempt_number, error, published_at IS NOT NULL
		FROM outbox_events WHERE id = $1`, id,
	).Scan(&s.attempts, &s.lastErr, &s.published); err != nil {
		t.Fatalf("read event %d: %v", id, err)
	}
	return s
}

func newRelay(pub Publisher, batchSize int, interval time.Duration) *Relay {
	return New(storage.NewStore(testPool), pub, batchSize, interval, testLogger)
}

func equalIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestRunOncePublishesAndMarksEverything(t *testing.T) {
	testdb.Reset(t, testPool)
	ids := insertEvents(t, 3)
	pub := &fakePublisher{}

	n, err := newRelay(pub, 10, time.Second).RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if n != 3 {
		t.Errorf("published count: got %d, want 3", n)
	}
	if _, got := pub.snapshot(); !equalIDs(got, ids) {
		t.Errorf("publish order: got %v, want %v", got, ids)
	}
	for _, id := range ids {
		if s := stateOf(t, id); !s.published || s.attempts != 0 {
			t.Errorf("event %d: got %+v, want published on the first attempt", id, s)
		}
	}
}

// A failure is recorded, the transaction still commits, and the batch stops
// at the failed event so a later event for the same booking can't overtake it.
func TestRunOnceRecordsFailureAndStopsBatch(t *testing.T) {
	testdb.Reset(t, testPool)
	ids := insertEvents(t, 3)
	first, failing, after := ids[0], ids[1], ids[2]
	pub := &fakePublisher{failOn: map[int64]bool{failing: true}}

	n, err := newRelay(pub, 10, time.Second).RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: a publish failure must not fail the batch, got %v", err)
	}
	if n != 1 {
		t.Errorf("published count: got %d, want 1", n)
	}

	calls, published := pub.snapshot()
	if calls != 2 {
		t.Errorf("publish calls: got %d, want 2 — the batch must stop at the failure", calls)
	}
	if !equalIDs(published, []int64{first}) {
		t.Errorf("published: got %v, want [%d]", published, first)
	}

	if s := stateOf(t, first); !s.published {
		t.Errorf("event before the failure: got %+v, want published", s)
	}
	s := stateOf(t, failing)
	if s.published || s.attempts != 1 || s.lastErr == nil || *s.lastErr != errInjectedPublish.Error() {
		t.Errorf("failed event: got %+v (err %v), want unpublished, 1 attempt, error recorded", s, s.lastErr)
	}
	if s := stateOf(t, after); s.published || s.attempts != 0 {
		t.Errorf("event after the failure: got %+v, want untouched", s)
	}
}

// Once the cause is fixed, the next run publishes the failed event and
// everything behind it, in order, keeping the attempt history.
func TestRunOnceRetriesFailedEventNextTime(t *testing.T) {
	testdb.Reset(t, testPool)
	ids := insertEvents(t, 3)
	pub := &fakePublisher{failOn: map[int64]bool{ids[1]: true}}
	r := newRelay(pub, 10, time.Second)

	if _, err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("first run: %v", err)
	}

	pub.mu.Lock()
	pub.failOn = nil
	pub.mu.Unlock()

	n, err := r.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if n != 2 {
		t.Errorf("second run published: got %d, want 2", n)
	}
	if _, got := pub.snapshot(); !equalIDs(got, ids) {
		t.Errorf("overall publish order: got %v, want %v", got, ids)
	}
	if s := stateOf(t, ids[1]); !s.published || s.attempts != 1 {
		t.Errorf("retried event: got %+v, want published with 1 recorded failure", s)
	}
}

func TestRunOnceWithNothingToDo(t *testing.T) {
	testdb.Reset(t, testPool)
	pub := &fakePublisher{}

	n, err := newRelay(pub, 10, time.Second).RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if calls, _ := pub.snapshot(); n != 0 || calls != 0 {
		t.Errorf("empty outbox: got n=%d calls=%d, want 0 and 0", n, calls)
	}
}

func TestRunOnceRespectsBatchSize(t *testing.T) {
	testdb.Reset(t, testPool)
	insertEvents(t, 5)
	pub := &fakePublisher{}

	n, err := newRelay(pub, 2, time.Second).RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 2 {
		t.Errorf("published: got %d, want 2 (the batch size)", n)
	}
}

// Run drains a backlog without sleeping between full batches, then stops
// promptly on cancel even in the middle of a long sleep.
func TestRunDrainsBacklogAndStopsOnCancel(t *testing.T) {
	testdb.Reset(t, testPool)
	ids := insertEvents(t, 5)
	pub := &fakePublisher{}

	// A one-hour interval: if Run slept after a full batch, this test would
	// never see all 5 published.
	r := newRelay(pub, 2, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	deadline := time.After(10 * time.Second)
	for {
		if _, got := pub.snapshot(); len(got) == len(ids) {
			break
		}
		select {
		case <-deadline:
			_, got := pub.snapshot()
			t.Fatalf("backlog not drained: published %v, want all of %v", got, ids)
		case <-time.After(20 * time.Millisecond):
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run after cancel: got %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop within 2s of cancel — the sleep ignores ctx")
	}
}
