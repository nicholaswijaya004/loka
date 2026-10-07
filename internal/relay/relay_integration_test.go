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

// fakePublisher records each Publish call and fails the events in failOn.
// With shortResults it breaks the contract and returns one result too few.
// The mutex matters only for the Run test, where the relay runs in a goroutine.
type fakePublisher struct {
	mu           sync.Mutex
	failOn       map[int64]bool
	shortResults bool
	calls        [][]int64 // the event ids of each call
	published    []int64
}

var _ Publisher = (*fakePublisher)(nil)

func (f *fakePublisher) Publish(_ context.Context, msgs []Message) []error {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make([]int64, len(msgs))
	errs := make([]error, len(msgs))
	for i, m := range msgs {
		ids[i] = m.Event.ID
		if f.failOn[m.Event.ID] {
			errs[i] = errInjectedPublish
			continue
		}
		f.published = append(f.published, m.Event.ID)
	}
	f.calls = append(f.calls, ids)
	if f.shortResults {
		return errs[:len(errs)-1]
	}
	return errs
}

func (f *fakePublisher) snapshot() (calls [][]int64, published []int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]int64(nil), f.calls...), append([]int64(nil), f.published...)
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

// A batch goes to the publisher in one call, in id order, and every event
// it acknowledged is marked.
func TestRunOncePublishesTheBatchInOneCall(t *testing.T) {
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
	calls, _ := pub.snapshot()
	if len(calls) != 1 || !equalIDs(calls[0], ids) {
		t.Errorf("publish calls: got %v, want one call with %v", calls, ids)
	}
	for _, id := range ids {
		if s := stateOf(t, id); !s.published || s.attempts != 0 {
			t.Errorf("event %d: got %+v, want published on the first attempt", id, s)
		}
	}
}

// One failed event doesn't hold back the others: every acknowledged event is
// marked, the failure is recorded on its own event, and the transaction still
// commits. (Per-booking order is not kept when a batch partly fails; today's
// consumers don't depend on it.)
func TestRunOnceMarksEverySuccessWhenOneFails(t *testing.T) {
	testdb.Reset(t, testPool)
	ids := insertEvents(t, 3)
	first, failing, after := ids[0], ids[1], ids[2]
	pub := &fakePublisher{failOn: map[int64]bool{failing: true}}

	n, err := newRelay(pub, 10, time.Second).RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: a publish failure must not fail the batch, got %v", err)
	}
	if n != 2 {
		t.Errorf("published count: got %d, want 2", n)
	}
	if calls, _ := pub.snapshot(); len(calls) != 1 || len(calls[0]) != 3 {
		t.Errorf("publish calls: got %v, want one call with all 3 events", calls)
	}

	for _, id := range []int64{first, after} {
		if s := stateOf(t, id); !s.published || s.attempts != 0 {
			t.Errorf("event %d: got %+v, want published", id, s)
		}
	}
	s := stateOf(t, failing)
	if s.published || s.attempts != 1 || s.lastErr == nil || *s.lastErr != errInjectedPublish.Error() {
		t.Errorf("failed event: got %+v (err %v), want unpublished, 1 attempt, error recorded", s, s.lastErr)
	}
}

// The next run sends only the event that failed, not the ones already marked,
// and keeps its attempt history.
func TestRunOnceRetriesOnlyTheFailedEvent(t *testing.T) {
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
	if n != 1 {
		t.Errorf("second run published: got %d, want 1", n)
	}
	if calls, _ := pub.snapshot(); len(calls) != 2 || !equalIDs(calls[1], []int64{ids[1]}) {
		t.Errorf("publish calls: got %v, want a second call with only %d", calls, ids[1])
	}
	if s := stateOf(t, ids[1]); !s.published || s.attempts != 1 {
		t.Errorf("retried event: got %+v, want published with 1 recorded failure", s)
	}
}

// Kafka down: every event fails, each failure is recorded, nothing is marked,
// and RunOnce itself succeeds so Run keeps polling.
func TestRunOnceRecordsEveryFailureWhenAllFail(t *testing.T) {
	testdb.Reset(t, testPool)
	ids := insertEvents(t, 3)
	failOn := map[int64]bool{}
	for _, id := range ids {
		failOn[id] = true
	}
	pub := &fakePublisher{failOn: failOn}

	n, err := newRelay(pub, 10, time.Second).RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 0 {
		t.Errorf("published count: got %d, want 0", n)
	}
	for _, id := range ids {
		if s := stateOf(t, id); s.published || s.attempts != 1 || s.lastErr == nil {
			t.Errorf("event %d: got %+v, want unpublished with 1 recorded failure", id, s)
		}
	}
}

// A publisher that returns the wrong number of results breaks the contract:
// the relay can't tell which events made it, so it marks and records nothing
// and returns an error. The events are sent again on the next poll.
func TestRunOnceRejectsAWrongNumberOfResults(t *testing.T) {
	testdb.Reset(t, testPool)
	ids := insertEvents(t, 3)
	pub := &fakePublisher{shortResults: true}

	n, err := newRelay(pub, 10, time.Second).RunOnce(context.Background())
	if err == nil {
		t.Fatal("RunOnce: got nil, want an error for 2 results to 3 messages")
	}
	if n != 0 {
		t.Errorf("published count: got %d, want 0", n)
	}
	for _, id := range ids {
		if s := stateOf(t, id); s.published || s.attempts != 0 {
			t.Errorf("event %d: got %+v, want untouched", id, s)
		}
	}
}

func TestRunOnceWithNothingToDo(t *testing.T) {
	testdb.Reset(t, testPool)
	pub := &fakePublisher{}

	n, err := newRelay(pub, 10, time.Second).RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if calls, _ := pub.snapshot(); n != 0 || len(calls) != 0 {
		t.Errorf("empty outbox: got n=%d calls=%v, want 0 and none", n, calls)
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
	if calls, _ := pub.snapshot(); len(calls) != 1 || len(calls[0]) != 2 {
		t.Errorf("publish calls: got %v, want one call with 2 events", calls)
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

// A failure to read the outbox is an error, not an empty batch. Here a row
// Go can't decode (trace_context is a JSON array, not an object) fails the
// fetch without aborting the transaction, so only RunOnce itself can report
// it. Swallowed, the relay would sleep as if idle while nothing gets out.
func TestRunOnceReportsAFetchFailure(t *testing.T) {
	testdb.Reset(t, testPool)
	ids := insertEvents(t, 1)
	if _, err := testPool.Exec(context.Background(),
		`UPDATE outbox_events SET trace_context = '[1, 2]' WHERE id = $1`, ids[0]); err != nil {
		t.Fatalf("corrupt trace_context: %v", err)
	}
	pub := &fakePublisher{}

	n, err := newRelay(pub, 10, time.Second).RunOnce(context.Background())
	if err == nil {
		t.Fatal("RunOnce with an undecodable row: got nil, want the fetch error")
	}
	if calls, _ := pub.snapshot(); n != 0 || len(calls) != 0 {
		t.Errorf("got n=%d calls=%v, want 0 and none", n, calls)
	}
}
