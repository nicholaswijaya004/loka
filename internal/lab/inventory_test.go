package lab

import (
	"os"
	"sync"
	"sync/atomic"
	"testing"
)

func TestUnsafeInventoryOverbooks(t *testing.T) {
	if os.Getenv("LAB_RACE") == "" {
		t.Skip("demonstrates a deliberate data race; set LAB_RACE=1 to run")
	}
	const seats = 10
	const attempts = 500

	inv := NewUnsafeInventory(seats)
	var wg sync.WaitGroup
	var succeeded int64

	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := inv.Book(); err == nil {
				atomic.AddInt64(&succeeded, 1)
			}
		}()
	}
	wg.Wait()

	available, sold := inv.Stats()
	t.Logf("seats=%d succeeded=%d sold=%d available=%d overbooked=%d",
		seats, succeeded, sold, available, int(succeeded)-seats)
	t.Logf("invariant available+sold == seats? %v (%d + %d = %d)",
		available+sold == seats, available, sold, available+sold)
}

func TestSafeInventory(t *testing.T) {
	const seats = 10
	const attempts = 500

	inv := NewSafeInventory(seats)
	var wg sync.WaitGroup
	var succeeded int64

	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := inv.Book(); err == nil {
				atomic.AddInt64(&succeeded, 1)
			}
		}()
	}
	wg.Wait()

	available, sold := inv.Stats()
	t.Logf("seats=%d succeeded=%d sold=%d available=%d", seats, succeeded, sold, available)

	if succeeded != seats {
		t.Errorf("bookings: want %d, got %d (overbooked by %d)", seats, succeeded, succeeded-seats)
	}
	if sold != seats {
		t.Errorf("sold: want %d, got %d", seats, sold)
	}
	if available != 0 {
		t.Errorf("available: want 0, got %d", available)
	}
	if available+sold != seats {
		t.Errorf("invariant violated: available(%d) + sold(%d) = %d, want %d",
			available, sold, available+sold, seats)
	}
}
