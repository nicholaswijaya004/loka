package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrUnitNotFound = errors.New("inventory unit not found")
var ErrSoldOut = errors.New("inventory unit sold out")
var ErrBookingNotFound = errors.New("booking not found")
var ErrIdempotencyKeyNotFound = errors.New("idempotency key not found")
var ErrVersionConflict = errors.New("version conflict")
var ErrSerializationFailure = errors.New("serialization failure")
var ErrStatusConflict = errors.New("booking status changed")

type DBTX interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

type Store struct{ db DBTX }

func NewStore(db DBTX) *Store {
	return &Store{db: db}
}

func isSerializationFailure(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "40001"
}

func (s *Store) WithTx(ctx context.Context, fn func(*Store) error) error {
	pool, ok := s.db.(*pgxpool.Pool)
	if !ok {
		return errors.New("WithTx: store already inside a transaction")
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}

	defer func() {
		_ = tx.Rollback(ctx)
	}()

	if err := fn(NewStore(tx)); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}

	return nil
}

func (s *Store) WithSerializableTx(ctx context.Context, fn func(*Store) error) error {
	pool, ok := s.db.(*pgxpool.Pool)
	if !ok {
		return errors.New("WithSerializableTx: store already inside a transaction")
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return fmt.Errorf("begin serializable transaction: %w", err)
	}

	defer func() {
		_ = tx.Rollback(ctx)
	}()

	if err := fn(NewStore(tx)); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		if isSerializationFailure(err) {
			return ErrSerializationFailure
		}
		return fmt.Errorf("commit serialized transaction: %w", err)
	}

	return nil
}
