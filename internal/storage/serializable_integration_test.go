//go:build integration

package storage

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

var fiftySeatUnitID = uuid.MustParse("33333333-3333-3333-3333-333333333333")

type skewTx struct {
	name          string
	readID        uuid.UUID
	writeID       uuid.UUID
	readErr       chan error
	updateErr     chan error
	proceedUpdate chan struct{}
	proceedCommit chan struct{}
	done          chan skewResult
}

type skewResult struct {
	err        error
	fnReturned bool // fn finished cleanly, so any error came from COMMIT
}

func newSkewTx(name string, readID, writeID uuid.UUID) *skewTx {
	return &skewTx{
		name: name, readID: readID, writeID: writeID,
		readErr:       make(chan error, 1),
		updateErr:     make(chan error, 1),
		proceedUpdate: make(chan struct{}),
		proceedCommit: make(chan struct{}),
		done:          make(chan skewResult, 1),
	}
}

func (s *skewTx) start(ctx context.Context, store *Store) {
	go func() {
		fnReturned := false
		err := store.WithSerializableTx(ctx, func(tx *Store) error {
			_, err := tx.GetInventoryUnit(ctx, s.readID)
			s.readErr <- err
			if err != nil {
				return err
			}
			if err := waitFor(ctx, s.proceedUpdate); err != nil {
				return err
			}
			err = tx.DecrementAvailability(ctx, s.writeID, 1)
			s.updateErr <- err
			if err != nil {
				return err
			}
			if err := waitFor(ctx, s.proceedCommit); err != nil {
				return err
			}
			fnReturned = true
			return nil
		})
		s.done <- skewResult{err: err, fnReturned: fnReturned}
	}()
}

func waitFor(ctx context.Context, ch <-chan struct{}) error {
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func recv[T any](t *testing.T, ctx context.Context, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-ctx.Done():
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

func unitAvailable(t *testing.T, s *Store, id uuid.UUID) int {
	t.Helper()
	unit, err := s.GetInventoryUnit(context.Background(), id)
	if err != nil {
		t.Fatalf("read unit %s: %v", id, err)
	}
	return unit.AvailableUnits
}

func TestSerializableConcurrentUpdateBlocksThenAborts(t *testing.T) {
	resetDB(t)
	store := NewStore(testPool)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	t1Updated := make(chan struct{})
	commitT1 := make(chan struct{})
	releaseT1 := sync.OnceFunc(func() { close(commitT1) })
	defer releaseT1() // never leave T1 hanging, even if the test fails early

	t1Done := make(chan error, 1)
	t2PID := make(chan int, 1)
	t2Done := make(chan error, 1)

	// T1: update the row, then hold the transaction open until told to commit.
	go func() {
		t1Done <- store.WithSerializableTx(ctx, func(tx *Store) error {
			if err := tx.DecrementAvailability(ctx, tenSeatUnitID, 1); err != nil {
				return err
			}
			close(t1Updated)
			<-commitT1
			return nil
		})
	}()

	select {
	case <-t1Updated:
	case err := <-t1Done:
		t.Fatalf("T1 finished before updating: %v", err)
	case <-ctx.Done():
		t.Fatal("timed out waiting for T1 to update")
	}

	// T2: take its snapshot while T1 is still uncommitted, then update the same row.
	go func() {
		t2Done <- store.WithSerializableTx(ctx, func(tx *Store) error {
			var pid int
			if err := tx.db.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				return err
			}
			t2PID <- pid
			return tx.DecrementAvailability(ctx, tenSeatUnitID, 1)
		})
	}()

	var pid int
	select {
	case pid = <-t2PID:
	case err := <-t2Done:
		t.Fatalf("T2 finished before reporting its pid: %v", err)
	case <-ctx.Done():
		t.Fatal("timed out waiting for T2 to start")
	}

	waitUntilBlockedOnLock(t, ctx, pid)

	select {
	case err := <-t2Done:
		t.Fatalf("T2 finished while T1 still held the row: %v", err)
	default:
	}

	releaseT1()

	if err := <-t1Done; err != nil {
		t.Fatalf("T1: unexpected error: %v", err)
	}

	var err error
	select {
	case err = <-t2Done:
	case <-ctx.Done():
		t.Fatal("T2 never returned after T1 committed")
	}
	if !errors.Is(err, ErrSerializationFailure) {
		t.Errorf("T2 error: got %v, want %v", err, ErrSerializationFailure)
	}

	if got := availableUnits(t, store); got != 9 {
		t.Errorf("available units: got %d, want 9 — only T1's write may land", got)
	}
}

// waitUntilBlockedOnLock polls until the given backend is waiting on a lock.
func waitUntilBlockedOnLock(t *testing.T, ctx context.Context, pid int) {
	t.Helper()
	for {
		var waitType *string
		err := testPool.QueryRow(ctx,
			`SELECT wait_event_type FROM pg_stat_activity WHERE pid = $1`, pid,
		).Scan(&waitType)
		if err != nil {
			t.Fatalf("poll pg_stat_activity: %v", err)
		}
		if waitType != nil && *waitType == "Lock" {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("backend %d never blocked on a lock", pid)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestSerializableWriteSkew(t *testing.T) {
	tests := []struct {
		name           string
		t1CommitsFirst bool
	}{
		{"T1 commits first", true},
		{"T2 commits first", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetDB(t)
			store := NewStore(testPool)

			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel() // also unblocks any goroutine still waiting if the test fails

			// T1 reads A and writes B. T2 reads B and writes A.
			t1 := newSkewTx("T1", tenSeatUnitID, fiftySeatUnitID)
			t2 := newSkewTx("T2", fiftySeatUnitID, tenSeatUnitID)

			// Step 1: both read. Each read takes the snapshot and records
			// which rows the transaction depends on.
			t1.start(ctx, store)
			if err := recv(t, ctx, t1.readErr, "T1 read"); err != nil {
				t.Fatalf("T1 read: %v", err)
			}
			t2.start(ctx, store)
			if err := recv(t, ctx, t2.readErr, "T2 read"); err != nil {
				t.Fatalf("T2 read: %v", err)
			}

			// Step 2: each writes the row the other read. Different rows,
			// so no row lock is involved and nobody waits.
			close(t1.proceedUpdate)
			if err := recv(t, ctx, t1.updateErr, "T1 update"); err != nil {
				t.Logf("T1 failed at UPDATE: %v", err)
			}
			close(t2.proceedUpdate)
			if err := recv(t, ctx, t2.updateErr, "T2 update"); err != nil {
				t.Logf("T2 failed at UPDATE: %v", err)
			}

			// Step 3: commit in the chosen order.
			first, second := t1, t2
			if !tt.t1CommitsFirst {
				first, second = t2, t1
			}
			close(first.proceedCommit)
			firstRes := recv(t, ctx, first.done, first.name+" result")
			close(second.proceedCommit)
			secondRes := recv(t, ctx, second.done, second.name+" result")

			for _, r := range []struct {
				tx  *skewTx
				res skewResult
			}{{first, firstRes}, {second, secondRes}} {
				switch {
				case r.res.err == nil:
					t.Logf("%s committed", r.tx.name)
				case r.res.fnReturned:
					t.Logf("%s failed at COMMIT: %v", r.tx.name, r.res.err)
				default:
					t.Logf("%s failed inside the transaction: %v", r.tx.name, r.res.err)
				}
			}

			if firstRes.err != nil {
				t.Errorf("%s committed first: got %v, want nil", first.name, firstRes.err)
			}
			if !errors.Is(secondRes.err, ErrSerializationFailure) {
				t.Errorf("%s: got %v, want %v", second.name, secondRes.err, ErrSerializationFailure)
			}

			// Only the winner's write may land.
			wantA, wantB := 10, 49 // T1 won: it wrote B
			if !tt.t1CommitsFirst {
				wantA, wantB = 9, 50 // T2 won: it wrote A
			}
			if got := unitAvailable(t, store, tenSeatUnitID); got != wantA {
				t.Errorf("unit A: got %d, want %d", got, wantA)
			}
			if got := unitAvailable(t, store, fiftySeatUnitID); got != wantB {
				t.Errorf("unit B: got %d, want %d", got, wantB)
			}
		})
	}
}
