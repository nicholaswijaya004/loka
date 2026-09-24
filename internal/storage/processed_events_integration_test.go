//go:build integration

package storage

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestClaimEventFirstClaimIsNew(t *testing.T) {
	resetDB(t)
	store := NewStore(testPool)

	isNew, err := store.ClaimEvent(context.Background(), "notifier", 1)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !isNew {
		t.Error("first claim: got false, want true")
	}
}

func TestClaimEventSecondClaimIsDuplicate(t *testing.T) {
	resetDB(t)
	store := NewStore(testPool)
	ctx := context.Background()

	if _, err := store.ClaimEvent(ctx, "notifier", 1); err != nil {
		t.Fatalf("first claim: %v", err)
	}

	isNew, err := store.ClaimEvent(ctx, "notifier", 1)
	if err != nil {
		t.Fatalf("second claim: got error %v, want nil — a duplicate is not an error", err)
	}
	if isNew {
		t.Error("second claim: got true, want false (duplicate)")
	}
}

// Each consumer tracks its own progress: one consumer processing an event
// must not make another consumer skip it.
func TestClaimEventIsPerConsumer(t *testing.T) {
	resetDB(t)
	store := NewStore(testPool)
	ctx := context.Background()

	if _, err := store.ClaimEvent(ctx, "notifier", 1); err != nil {
		t.Fatalf("notifier claim: %v", err)
	}

	isNew, err := store.ClaimEvent(ctx, "voucher", 1)
	if err != nil {
		t.Fatalf("voucher claim: %v", err)
	}
	if !isNew {
		t.Error("another consumer claiming the same event: got false, want true")
	}
}

func TestClaimEventDifferentEventsAreIndependent(t *testing.T) {
	resetDB(t)
	store := NewStore(testPool)
	ctx := context.Background()

	for _, id := range []int64{1, 2, 3} {
		isNew, err := store.ClaimEvent(ctx, "notifier", id)
		if err != nil {
			t.Fatalf("claim %d: %v", id, err)
		}
		if !isNew {
			t.Errorf("claim %d: got false, want true", id)
		}
	}
}

// A claim made in a transaction that rolls back must disappear with it, so the
// event is processed again on redelivery. This is what makes a crash between
// claiming and finishing the work safe.
func TestClaimEventRolledBackClaimIsForgotten(t *testing.T) {
	resetDB(t)
	store := NewStore(testPool)
	ctx := context.Background()
	errWorkFailed := errors.New("handler failed")

	err := store.WithTx(ctx, func(tx *Store) error {
		isNew, err := tx.ClaimEvent(ctx, "notifier", 1)
		if err != nil {
			return err
		}
		if !isNew {
			t.Error("claim inside tx: got false, want true")
		}
		return errWorkFailed // the handler's work failed, so roll everything back
	})
	if !errors.Is(err, errWorkFailed) {
		t.Fatalf("tx: got %v, want %v", err, errWorkFailed)
	}

	isNew, err := store.ClaimEvent(ctx, "notifier", 1)
	if err != nil {
		t.Fatalf("claim after rollback: %v", err)
	}
	if !isNew {
		t.Error("claim after rollback: got false, want true — the rolled-back claim must not count")
	}
}

// Many deliveries of the same event racing each other: exactly one may win.
func TestClaimEventConcurrentClaimsHaveOneWinner(t *testing.T) {
	resetDB(t)
	store := NewStore(testPool)
	ctx := context.Background()

	const workers = 20
	start := make(chan struct{})
	results := make(chan bool, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			err := store.WithTx(ctx, func(tx *Store) error {
				isNew, err := tx.ClaimEvent(ctx, "notifier", 1)
				if err != nil {
					return err
				}
				results <- isNew
				return nil
			})
			if err != nil {
				errs <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		t.Errorf("claim: %v", err)
	}

	winners := 0
	for isNew := range results {
		if isNew {
			winners++
		}
	}
	if winners != 1 {
		t.Errorf("winners: got %d, want exactly 1", winners)
	}
}
