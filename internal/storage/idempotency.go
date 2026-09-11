package storage

import (
	"context"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"time"
)

type IdempotencyKey struct {
	Key            string
	BookingID      *uuid.UUID
	RequestHash    string
	ResponseStatus *int
	ResponseBody   []byte
	State          string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	ExpiresAt      *time.Time
}

func (s *Store) ClaimIdempotencyKey(ctx context.Context, key, requestHash string) (bool, error) {
	_, err := s.db.Exec(ctx, `
		INSERT INTO idempotency_keys (idempotency_key, request_hash, state)
		VALUES ($1, $2, 'in_progress')
	`, key, requestHash)

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return false, nil
	}

	if err != nil {
		return false, fmt.Errorf("claim idempotency key: %w", err)
	}

	return true, nil
}

func (s *Store) GetIdempotencyKey(ctx context.Context, key string) (*IdempotencyKey, error) {
	var idempotencyKey IdempotencyKey
	err := s.db.QueryRow(ctx, `
		SELECT idempotency_key, booking_id, request_hash, response_status, response_body, state, created_at, updated_at, expires_at
		FROM idempotency_keys
		WHERE idempotency_key = $1
	`, key).Scan(
		&idempotencyKey.Key, &idempotencyKey.BookingID, &idempotencyKey.RequestHash,
		&idempotencyKey.ResponseStatus, &idempotencyKey.ResponseBody, &idempotencyKey.State,
		&idempotencyKey.CreatedAt, &idempotencyKey.UpdatedAt, &idempotencyKey.ExpiresAt,
	)

	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrIdempotencyKeyNotFound
	}

	if err != nil {
		return nil, fmt.Errorf("get idempotency key: %w", err)
	}

	return &idempotencyKey, nil
}

func (s *Store) CompleteIdempotencyKey(ctx context.Context, key string, bookingID uuid.UUID, responseStatus int, responseBody []byte) error {
	tag, err := s.db.Exec(ctx, `
		UPDATE idempotency_keys
		SET booking_id = $2, response_status = $3, response_body = $4, state = 'completed', expires_at = NOW() + INTERVAL '24 hours', updated_at = NOW()
		WHERE idempotency_key = $1 AND state = 'in_progress'
	`, key, bookingID, responseStatus, responseBody)

	if err != nil {
		return fmt.Errorf("complete idempotency key: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return fmt.Errorf("complete idempotency key %s: not in progress", key)
	}

	return nil
}

func (s *Store) ReleaseIdempotencyKey(ctx context.Context, key string) error {
	tag, err := s.db.Exec(ctx, `
		DELETE FROM idempotency_keys
		WHERE idempotency_key = $1 AND state = 'in_progress'
	`, key)

	if err != nil {
		return fmt.Errorf("release idempotency key: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return fmt.Errorf("release idempotency key %s: not in progress", key)
	}

	return nil
}
