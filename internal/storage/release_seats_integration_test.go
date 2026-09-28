//go:build integration

package storage

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func readUnit(t *testing.T, s *Store, id uuid.UUID) *InventoryUnit {
	t.Helper()
	u, err := s.GetInventoryUnit(context.Background(), id)
	if err != nil {
		t.Fatalf("read unit %s: %v", id, err)
	}
	return u
}

func TestReleaseSeatsGivesSeatsBack(t *testing.T) {
	resetDB(t)
	store := NewStore(testPool)
	ctx := context.Background()

	if err := store.DecrementAvailability(ctx, tenSeatUnitID, 3); err != nil {
		t.Fatalf("decrement: %v", err)
	}
	before := readUnit(t, store, tenSeatUnitID)

	if err := store.ReleaseSeats(ctx, tenSeatUnitID, 3); err != nil {
		t.Fatalf("release: %v", err)
	}

	after := readUnit(t, store, tenSeatUnitID)
	if after.AvailableUnits != 10 {
		t.Errorf("available units: got %d, want 10", after.AvailableUnits)
	}
	if after.Version != before.Version+1 {
		t.Errorf("version: got %d, want %d — a release must bump it", after.Version, before.Version+1)
	}
}

// Releasing seats nobody held is a bug. It must be reported as an
// over-release, never as sold out, and must change nothing.
func TestReleaseSeatsRejectsOverRelease(t *testing.T) {
	resetDB(t)
	store := NewStore(testPool)
	before := readUnit(t, store, tenSeatUnitID) // seeded full: 10 of 10

	err := store.ReleaseSeats(context.Background(), tenSeatUnitID, 1)

	if !errors.Is(err, ErrOverRelease) {
		t.Errorf("error: got %v, want ErrOverRelease", err)
	}
	if errors.Is(err, ErrSoldOut) {
		t.Error("an over-release was reported as sold out")
	}
	after := readUnit(t, store, tenSeatUnitID)
	if after.AvailableUnits != before.AvailableUnits || after.Version != before.Version {
		t.Errorf("unit changed: got %d available (v%d), want %d (v%d)",
			after.AvailableUnits, after.Version, before.AvailableUnits, before.Version)
	}
}

func TestReleaseSeatsUnknownUnit(t *testing.T) {
	resetDB(t)
	store := NewStore(testPool)

	err := store.ReleaseSeats(context.Background(), uuid.New(), 1)
	if !errors.Is(err, ErrUnitNotFound) {
		t.Errorf("error: got %v, want ErrUnitNotFound", err)
	}
}

func TestReleaseSeatsRejectsNonPositiveQty(t *testing.T) {
	resetDB(t)
	store := NewStore(testPool)

	for _, qty := range []int{0, -1} {
		if err := store.ReleaseSeats(context.Background(), tenSeatUnitID, qty); err == nil {
			t.Errorf("qty %d: got nil, want an error", qty)
		}
	}
	if u := readUnit(t, store, tenSeatUnitID); u.AvailableUnits != 10 {
		t.Errorf("available units: got %d, want 10 — invalid qty must change nothing", u.AvailableUnits)
	}
}
