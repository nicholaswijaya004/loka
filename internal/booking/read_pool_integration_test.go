//go:build integration

package booking

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nicholaswijaya004/loka/internal/storage"
)

// TestGetIsolatedFromExhaustedWritePool reproduces what a stall does to the
// API: every write connection is held (by POSTs frozen mid-statement), and a
// GET arrives. With a reader on its own pool the GET is served; without one it
// queues for a write connection until its deadline.
func TestGetIsolatedFromExhaustedWritePool(t *testing.T) {
	resetDB(t)
	ctx := context.Background()

	writePool := newSizedPool(t, 2)
	readPool := newSizedPool(t, 2)
	writeStore := storeAdapter{storage.NewStore(writePool)}

	b, err := NewService(writeStore, testLogger, false, "single").
		Create(ctx, testUnitID, testCustomerID, 1, testVisit)
	if err != nil {
		t.Fatalf("create booking: %v", err)
	}

	// Hold every write connection. Registered after the pool's Close, so the
	// cleanups release them before the pool closes (Close waits for them).
	for i := int32(0); i < writePool.Config().MaxConns; i++ {
		conn, err := writePool.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquire write conn %d: %v", i, err)
		}
		t.Cleanup(conn.Release)
	}
	if got, want := writePool.Stat().AcquiredConns(), writePool.Config().MaxConns; got != want {
		t.Fatalf("write pool acquired conns: got %d, want %d (pool not exhausted)", got, want)
	}

	t.Run("with a reader, Get is served from the read pool", func(t *testing.T) {
		svc := NewService(writeStore, testLogger, false, "single",
			WithReader(storage.NewStore(readPool)))

		getCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		got, err := svc.Get(getCtx, b.BookingID)
		if err != nil {
			t.Fatalf("Get with the write pool exhausted: %v", err)
		}
		if got.BookingID != b.BookingID {
			t.Errorf("booking id: got %v, want %v", got.BookingID, b.BookingID)
		}
	})

	// Control: proves the exhausted write pool really blocks, so the subtest
	// above can't pass by accident (e.g. if the pool had a free connection).
	t.Run("without a reader, Get waits for a write connection", func(t *testing.T) {
		svc := NewService(writeStore, testLogger, false, "single")

		getCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		defer cancel()
		_, err := svc.Get(getCtx, b.BookingID)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Get error: got %v, want context.DeadlineExceeded", err)
		}
	})
}

// newSizedPool opens a pool to the test database with at most size connections.
func newSizedPool(t *testing.T, size int32) *pgxpool.Pool {
	t.Helper()
	cfg := testPool.Config() // a copy; changing it doesn't touch testPool
	cfg.MaxConns = size
	cfg.MinConns = 0
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}
