package unitcache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/nicholaswijaya004/loka/internal/booking"
	"github.com/nicholaswijaya004/loka/internal/storage"
	"github.com/redis/go-redis/v9"
)

type Redis struct {
	client *redis.Client
	ttl    time.Duration
}

func New(client *redis.Client, ttl time.Duration) *Redis {
	return &Redis{
		client: client,
		ttl:    ttl,
	}
}

func Key(id uuid.UUID) string {
	return "unit:" + id.String()
}

func (r *Redis) Get(ctx context.Context, id uuid.UUID) (*storage.InventoryUnit, error) {
	// Implementation of Get method to retrieve InventoryUnit from Redis
	data, err := r.client.Get(ctx, Key(id)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, booking.ErrCacheMiss
	}

	if err != nil {
		return nil, fmt.Errorf("unit cache get: %w", err)
	}

	var u storage.InventoryUnit
	if err := json.Unmarshal(data, &u); err != nil {
		return nil, fmt.Errorf("unit cache decode: %w", err)
	}
	return &u, nil
}

func (r *Redis) Set(ctx context.Context, unit *storage.InventoryUnit) error {
	// Implementation of Set method to store InventoryUnit in Redis
	data, err := json.Marshal(unit)
	if err != nil {
		return fmt.Errorf("unit cache encode: %w", err)
	}

	err = r.client.Set(ctx, Key(unit.UnitID), data, r.ttl).Err()
	if err != nil {
		return fmt.Errorf("unit cache set: %w", err)
	}
	return nil
}

func (r *Redis) Delete(ctx context.Context, id uuid.UUID) error {
	// Implementation of Delete method to remove InventoryUnit from Redis
	err := r.client.Del(ctx, Key(id)).Err()
	if err != nil {
		return fmt.Errorf("unit cache delete: %w", err)
	}
	return nil
}
