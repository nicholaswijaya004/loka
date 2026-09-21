//go:build integration

package storage

import (
	"context"
	"errors"
	"testing"
)

// errFromFn stands in for any failure inside the transaction body.
var errFromFn = errors.New("fn failed")

// txFuncs lists both transaction helpers so every test runs against each.
var txFuncs = []struct {
	name string
	run  func(s *Store, ctx context.Context, fn func(*Store) error) error
}{
	{"WithTx", (*Store).WithTx},
	{"WithSerializableTx", (*Store).WithSerializableTx},
}

func availableUnits(t *testing.T, s *Store) int {
	t.Helper()
	unit, err := s.GetInventoryUnit(context.Background(), tenSeatUnitID)
	if err != nil {
		t.Fatalf("read unit: %v", err)
	}
	return unit.AvailableUnits
}

func TestTxRollsBackOnError(t *testing.T) {
	for _, tx := range txFuncs {
		t.Run(tx.name, func(t *testing.T) {
			resetDB(t)
			store := NewStore(testPool)

			err := tx.run(store, context.Background(), func(txStore *Store) error {
				if err := txStore.DecrementAvailability(context.Background(), tenSeatUnitID, 1); err != nil {
					t.Fatalf("decrement: %v", err)
				}
				return errFromFn
			})

			if !errors.Is(err, errFromFn) {
				t.Errorf("error: got %v, want %v — fn's error must come back unchanged", err, errFromFn)
			}
			if got := availableUnits(t, store); got != 10 {
				t.Errorf("available units: got %d, want 10 — the decrement must roll back", got)
			}
		})
	}
}

func TestTxCommitsOnSuccess(t *testing.T) {
	for _, tx := range txFuncs {
		t.Run(tx.name, func(t *testing.T) {
			resetDB(t)
			store := NewStore(testPool)

			err := tx.run(store, context.Background(), func(txStore *Store) error {
				return txStore.DecrementAvailability(context.Background(), tenSeatUnitID, 1)
			})

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := availableUnits(t, store); got != 9 {
				t.Errorf("available units: got %d, want 9", got)
			}
		})
	}
}

// If fn were handed a pool-backed store instead of a tx-bound one, the
// decrement would be visible from outside immediately and this would fail.
func TestTxWorkIsInvisibleUntilCommit(t *testing.T) {
	for _, tx := range txFuncs {
		t.Run(tx.name, func(t *testing.T) {
			resetDB(t)
			store := NewStore(testPool)
			outside := NewStore(testPool)

			err := tx.run(store, context.Background(), func(txStore *Store) error {
				if err := txStore.DecrementAvailability(context.Background(), tenSeatUnitID, 1); err != nil {
					return err
				}
				if got := availableUnits(t, txStore); got != 9 {
					t.Errorf("inside tx: got %d, want 9 — tx must see its own write", got)
				}
				if got := availableUnits(t, outside); got != 10 {
					t.Errorf("outside tx: got %d, want 10 — uncommitted write leaked", got)
				}
				return nil
			})

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := availableUnits(t, outside); got != 9 {
				t.Errorf("after commit: got %d, want 9", got)
			}
		})
	}
}

func TestTxRejectsNesting(t *testing.T) {
	for _, tx := range txFuncs {
		t.Run(tx.name, func(t *testing.T) {
			resetDB(t)
			store := NewStore(testPool)
			innerRan := false

			err := tx.run(store, context.Background(), func(txStore *Store) error {
				return tx.run(txStore, context.Background(), func(*Store) error {
					innerRan = true
					return nil
				})
			})

			if err == nil {
				t.Error("error: got nil, want a nested-transaction error")
			}
			if innerRan {
				t.Error("inner fn ran — nesting must be refused before fn is called")
			}
		})
	}
}
