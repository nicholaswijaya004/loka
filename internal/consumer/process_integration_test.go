//go:build integration

package consumer

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/nicholaswijaya004/loka/internal/storage"
	"github.com/nicholaswijaya004/loka/internal/testdb"
)

const testConsumer = "test-consumer"

var errHandlerFailed = errors.New("handler failed")

// fakeHandler counts calls and can fail on demand. When sideEffect is set, it
// writes a row through tx before returning, standing in for real work such as
// inserting a notification, so tests can check that work is rolled back too.
type fakeHandler struct {
	calls      int
	fail       bool
	sideEffect bool
}

func (f *fakeHandler) Handle(ctx context.Context, tx *storage.Store, e Event) error {
	f.calls++
	if f.sideEffect {
		// Any write through tx will do. Claiming under a different consumer
		// name gives a row we can count without needing another table.
		if _, err := tx.ClaimEvent(ctx, "side-effect", e.ID); err != nil {
			return err
		}
	}
	if f.fail {
		return errHandlerFailed
	}
	return nil
}

func newTestConsumer(h Handler) *Consumer {
	return New(testConsumer, storage.NewStore(testPool), h, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func countProcessed(t *testing.T, consumer string, eventID int64) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM processed_events WHERE consumer = $1 AND event_id = $2`,
		consumer, eventID,
	).Scan(&n); err != nil {
		t.Fatalf("count processed: %v", err)
	}
	return n
}

var testEvent = Event{ID: 7, Type: "booking.created", Key: "booking-7", Payload: []byte(`{}`)}

func TestProcessNewEventRunsHandler(t *testing.T) {
	testdb.Reset(t, testPool)
	h := &fakeHandler{}
	c := newTestConsumer(h)

	isNew, err := c.Process(context.Background(), testEvent)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if !isNew {
		t.Error("isNew: got false, want true")
	}
	if h.calls != 1 {
		t.Errorf("handler calls: got %d, want 1", h.calls)
	}
	if n := countProcessed(t, testConsumer, testEvent.ID); n != 1 {
		t.Errorf("processed rows: got %d, want 1", n)
	}
}

func TestProcessDuplicateSkipsHandler(t *testing.T) {
	testdb.Reset(t, testPool)
	h := &fakeHandler{}
	c := newTestConsumer(h)
	ctx := context.Background()

	if _, err := c.Process(ctx, testEvent); err != nil {
		t.Fatalf("first process: %v", err)
	}
	isNew, err := c.Process(ctx, testEvent)
	if err != nil {
		t.Fatalf("second process: got %v, want nil — a duplicate is not an error", err)
	}

	if isNew {
		t.Error("second process: isNew got true, want false")
	}
	if h.calls != 1 {
		t.Errorf("handler calls: got %d, want 1 — the duplicate must not run it again", h.calls)
	}
}

// A failed handler must leave no claim behind, so the redelivered event is
// processed rather than skipped.
func TestProcessHandlerFailureLeavesNoClaim(t *testing.T) {
	testdb.Reset(t, testPool)
	h := &fakeHandler{fail: true}
	c := newTestConsumer(h)
	ctx := context.Background()

	isNew, err := c.Process(ctx, testEvent)
	if !errors.Is(err, errHandlerFailed) {
		t.Fatalf("error: got %v, want %v", err, errHandlerFailed)
	}
	if isNew {
		t.Error("isNew after failure: got true, want false")
	}
	if n := countProcessed(t, testConsumer, testEvent.ID); n != 0 {
		t.Errorf("processed rows after failure: got %d, want 0 — the claim must roll back", n)
	}

	// The redelivery: handler healthy again, event must be processed.
	h.fail = false
	isNew, err = c.Process(ctx, testEvent)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if !isNew || h.calls != 2 {
		t.Errorf("retry: isNew=%v calls=%d, want true and 2", isNew, h.calls)
	}
}

// The handler's own database work must roll back with the claim. This is what
// proves the handler really runs in the same transaction.
func TestProcessHandlerWorkRollsBackWithClaim(t *testing.T) {
	testdb.Reset(t, testPool)
	h := &fakeHandler{fail: true, sideEffect: true}
	c := newTestConsumer(h)

	if _, err := c.Process(context.Background(), testEvent); !errors.Is(err, errHandlerFailed) {
		t.Fatalf("error: got %v, want %v", err, errHandlerFailed)
	}

	if n := countProcessed(t, "side-effect", testEvent.ID); n != 0 {
		t.Errorf("handler's write: got %d rows, want 0 — it must roll back with the claim", n)
	}
	if n := countProcessed(t, testConsumer, testEvent.ID); n != 0 {
		t.Errorf("claim: got %d rows, want 0", n)
	}
}
