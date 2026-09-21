//go:build integration

package storage

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

var (
	singleSeatUnitID = uuid.MustParse("44444444-4444-4444-4444-444444444444")
	tenSeatUnitID    = uuid.MustParse("22222222-2222-2222-2222-222222222222")
)

func TestSeedData(t *testing.T) {
	resetDB(t)
	var n int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM inventory_units`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("inventory units: got %d, want 3", n)
	}
}

func TestDecrementAvailabilityRejectsOverbooking(t *testing.T) {
	resetDB(t)
	store := NewStore(testPool)

	err := store.DecrementAvailability(context.Background(), singleSeatUnitID, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	err = store.DecrementAvailability(context.Background(), singleSeatUnitID, 1)
	if !errors.Is(err, ErrSoldOut) {
		t.Fatalf("error: got %v, want %v", err, ErrSoldOut)
	}

	unit, err := store.GetInventoryUnit(context.Background(), singleSeatUnitID)
	if err != nil {
		t.Fatalf("read back unit: %v", err)
	}
	if unit.AvailableUnits != 0 {
		t.Errorf("available units: got %d, want 0", unit.AvailableUnits)
	}
}

func TestDecrementAvailabilityUnknownUnit(t *testing.T) {
	resetDB(t)
	store := NewStore(testPool)

	err := store.DecrementAvailability(context.Background(), uuid.New(), 1)
	if !errors.Is(err, ErrUnitNotFound) {
		t.Errorf("error: got %v, want %v", err, ErrUnitNotFound)
	}
}

func TestGetInventoryUnitUnknownUnit(t *testing.T) {
	resetDB(t)
	store := NewStore(testPool)

	unit, err := store.GetInventoryUnit(context.Background(), uuid.New())
	if !errors.Is(err, ErrUnitNotFound) {
		t.Errorf("error: got %v, want %v", err, ErrUnitNotFound)
	}
	if unit != nil {
		t.Errorf("unit: got %+v, want nil", unit)
	}
}

// Zero rows affected has two meanings for the optimistic decrement: the version
// moved on, or the unit doesn't exist. These two tests pin that the method
// tells them apart.
func TestDecrementAvailabilityOptimisticStaleVersion(t *testing.T) {
	resetDB(t)
	store := NewStore(testPool)
	ctx := context.Background()

	unit, err := store.GetInventoryUnit(ctx, tenSeatUnitID)
	if err != nil {
		t.Fatalf("read unit: %v", err)
	}
	staleVersion := unit.Version

	if err := store.DecrementAvailabilityOptimistic(ctx, tenSeatUnitID, 1, staleVersion); err != nil {
		t.Fatalf("first decrement: %v", err)
	}

	err = store.DecrementAvailabilityOptimistic(ctx, tenSeatUnitID, 1, staleVersion)
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("error: got %v, want %v", err, ErrVersionConflict)
	}

	after, err := store.GetInventoryUnit(ctx, tenSeatUnitID)
	if err != nil {
		t.Fatalf("read back unit: %v", err)
	}
	if after.AvailableUnits != 9 {
		t.Errorf("available units: got %d, want 9 — the stale write must not land", after.AvailableUnits)
	}
	if after.Version != staleVersion+1 {
		t.Errorf("version: got %d, want %d", after.Version, staleVersion+1)
	}
}

func TestDecrementAvailabilityOptimisticUnknownUnit(t *testing.T) {
	resetDB(t)
	store := NewStore(testPool)

	err := store.DecrementAvailabilityOptimistic(context.Background(), uuid.New(), 1, 0)
	if !errors.Is(err, ErrUnitNotFound) {
		t.Errorf("error: got %v, want %v — a missing unit must not look like a conflict", err, ErrUnitNotFound)
	}
}
