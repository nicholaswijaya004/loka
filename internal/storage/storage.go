package storage

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var ErrUnitNotFound = errors.New("inventory unit not found")
var ErrSoldOut = errors.New("inventory unit sold out")
var ErrBookingNotFound = errors.New("booking not found")

type DBTX interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

type Store struct{ db DBTX }

func NewStore(db DBTX) *Store {
	return &Store{db: db}
}
