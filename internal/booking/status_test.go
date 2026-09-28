package booking

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/nicholaswijaya004/loka/internal/storage"
)

type fakeTransitioner struct {
	err      error
	called   bool
	from, to string
}

func (f *fakeTransitioner) TransitionBookingStatus(_ context.Context, _ uuid.UUID, from, to string, _ *string) error {
	f.called = true
	f.from, f.to = from, to
	return f.err
}

func TestCheckTransition(t *testing.T) {
	tests := []struct {
		from, to string
		allowed  bool
	}{
		{StatusPending, StatusPaymentPending, true},
		{StatusPending, StatusCancelled, true},
		{StatusPaymentPending, StatusConfirmed, true},
		{StatusPaymentPending, StatusCancelled, true},
		{StatusPending, StatusConfirmed, false},      // must pay first
		{StatusConfirmed, StatusCancelled, false},    // refunds are a separate flow
		{StatusCancelled, StatusConfirmed, false},    // final state
		{StatusCancelled, StatusPending, false},      // final state
		{StatusPaymentPending, StatusPending, false}, // can't undo a started charge
	}
	for _, tt := range tests {
		t.Run(tt.from+"→"+tt.to, func(t *testing.T) {
			err := checkTransition(tt.from, tt.to)
			if tt.allowed && err != nil {
				t.Errorf("got %v, want allowed", err)
			}
			if !tt.allowed && !errors.Is(err, ErrIllegalTransition) {
				t.Errorf("got %v, want ErrIllegalTransition", err)
			}
		})
	}
}

func TestTransition(t *testing.T) {
	id := uuid.New()

	t.Run("illegal move is rejected before touching the store", func(t *testing.T) {
		fake := &fakeTransitioner{}

		err := Transition(context.Background(), fake, id, StatusCancelled, StatusConfirmed, nil)

		if !errors.Is(err, ErrIllegalTransition) {
			t.Errorf("error: got %v, want ErrIllegalTransition", err)
		}
		if fake.called {
			t.Error("store was called for an illegal move; the check must come first")
		}
	})

	t.Run("a lost race is passed through unchanged", func(t *testing.T) {
		fake := &fakeTransitioner{err: storage.ErrStatusConflict}

		err := Transition(context.Background(), fake, id, StatusPaymentPending, StatusConfirmed, nil)

		if !errors.Is(err, storage.ErrStatusConflict) {
			t.Errorf("error: got %v, want storage.ErrStatusConflict", err)
		}
		if errors.Is(err, ErrIllegalTransition) {
			t.Error("a race was reported as an illegal transition")
		}
	})

	t.Run("a legal move reaches the store with the same states", func(t *testing.T) {
		fake := &fakeTransitioner{}

		err := Transition(context.Background(), fake, id, StatusPending, StatusPaymentPending, nil)

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !fake.called {
			t.Fatal("store was not called for a legal move")
		}
		if fake.from != StatusPending || fake.to != StatusPaymentPending {
			t.Errorf("store got %s → %s, want %s → %s", fake.from, fake.to, StatusPending, StatusPaymentPending)
		}
	})
}
