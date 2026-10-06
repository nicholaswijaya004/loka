//go:build integration

package unitcache

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/nicholaswijaya004/loka/internal/booking"
	"github.com/nicholaswijaya004/loka/internal/storage"
)

var testClient *redis.Client

func TestMain(m *testing.M) {
	ctx := context.Background()
	container, err := tcredis.Run(ctx, "redis:7.4-alpine")
	if err != nil {
		fmt.Fprintf(os.Stderr, "start redis: %v\n", err)
		os.Exit(1)
	}
	url, err := container.ConnectionString(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "redis url: %v\n", err)
		os.Exit(1)
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse redis url: %v\n", err)
		os.Exit(1)
	}
	testClient = redis.NewClient(opts)

	code := m.Run()
	_ = testClient.Close()
	if err := testcontainers.TerminateContainer(container); err != nil {
		fmt.Fprintf(os.Stderr, "terminate redis: %v\n", err)
	}
	os.Exit(code)
}

func testUnit() *storage.InventoryUnit {
	desc := "Sea view"
	return &storage.InventoryUnit{
		UnitID:         uuid.New(),
		Name:           "Deluxe Cabin",
		Description:    &desc,
		AvailableUnits: 4,
		TotalUnits:     10,
		Currency:       "IDR",
		PriceMinor:     150_000_000,
		MinBook:        1,
		Version:        3,
		CreatedAt:      time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC),
		UpdatedAt:      time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC),
	}
}

func TestRedisMissThenRoundTrip(t *testing.T) {
	ctx := context.Background()
	c := New(testClient, 30*time.Second)
	u := testUnit()

	if _, err := c.Get(ctx, u.UnitID); !errors.Is(err, booking.ErrCacheMiss) {
		t.Fatalf("empty cache: got %v, want booking.ErrCacheMiss", err)
	}
	if err := c.Set(ctx, u); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, err := c.Get(ctx, u.UnitID)
	if err != nil {
		t.Fatalf("get after set: %v", err)
	}
	if got.UnitID != u.UnitID || got.AvailableUnits != u.AvailableUnits ||
		got.PriceMinor != u.PriceMinor || got.Currency != u.Currency ||
		*got.Description != *u.Description || !got.UpdatedAt.Equal(u.UpdatedAt) {
		t.Errorf("round trip: got %+v, want %+v", got, u)
	}
}

func TestRedisSetAppliesTTL(t *testing.T) {
	ctx := context.Background()
	c := New(testClient, 30*time.Second)
	u := testUnit()

	if err := c.Set(ctx, u); err != nil {
		t.Fatalf("set: %v", err)
	}
	ttl, err := testClient.TTL(ctx, Key(u.UnitID)).Result()
	if err != nil {
		t.Fatalf("ttl: %v", err)
	}
	if ttl <= 0 || ttl > 30*time.Second {
		t.Errorf("ttl: got %v, want (0, 30s] — every entry must expire", ttl)
	}
}

func TestRedisDeleteMakesItAMiss(t *testing.T) {
	ctx := context.Background()
	c := New(testClient, 30*time.Second)
	u := testUnit()

	if err := c.Set(ctx, u); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := c.Delete(ctx, u.UnitID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := c.Get(ctx, u.UnitID); !errors.Is(err, booking.ErrCacheMiss) {
		t.Fatalf("after delete: got %v, want booking.ErrCacheMiss", err)
	}
	// A redelivered event deletes a key that's already gone: not an error.
	if err := c.Delete(ctx, u.UnitID); err != nil {
		t.Errorf("second delete: %v", err)
	}
}

// TestRedisDownIsNotAMiss checks that an unreachable Redis is reported as a
// failure, not a miss, so GetUnit falls back without trying to store.
func TestRedisDownIsNotAMiss(t *testing.T) {
	ctx := context.Background()
	down := redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1", // nothing listens here
		DialTimeout: 100 * time.Millisecond,
		MaxRetries:  -1,
	})
	defer func() { _ = down.Close() }()
	c := New(down, 30*time.Second)

	_, err := c.Get(ctx, uuid.New())
	if err == nil || errors.Is(err, booking.ErrCacheMiss) {
		t.Fatalf("redis down: got %v, want a non-miss error", err)
	}
	if err := c.Set(ctx, testUnit()); err == nil {
		t.Error("set with redis down: got nil, want an error")
	}
	if err := c.Delete(ctx, uuid.New()); err == nil {
		t.Error("delete with redis down: got nil, want an error")
	}
}

// TestRedisSetReportsRefusedWrites checks that Set returns Redis's own answer.
// Redis is reachable (PING works) but refuses writes: with maxmemory at 1 byte
// and no eviction, every SET fails with an OOM error.
func TestRedisSetReportsRefusedWrites(t *testing.T) {
	ctx := context.Background()
	for k, v := range map[string]string{"maxmemory": "1", "maxmemory-policy": "noeviction"} {
		old, err := testClient.ConfigGet(ctx, k).Result()
		if err != nil {
			t.Fatalf("config get %s: %v", k, err)
		}
		if err := testClient.ConfigSet(ctx, k, v).Err(); err != nil {
			t.Fatalf("config set %s: %v", k, err)
		}
		t.Cleanup(func() { _ = testClient.ConfigSet(ctx, k, old[k]).Err() })
	}
	if err := testClient.Ping(ctx).Err(); err != nil {
		t.Fatalf("ping: %v (Redis must be reachable for this test)", err)
	}

	err := New(testClient, 30*time.Second).Set(ctx, testUnit())
	if err == nil {
		t.Fatal("set refused by Redis: got nil, want the error Redis returned")
	}
}
