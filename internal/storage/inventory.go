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

type InventoryUnit struct {
	UnitID         uuid.UUID
	Name           string
	Description    *string // nullable → pointer
	AvailableUnits int
	TotalUnits     int
	Currency       string
	PriceMinor     int64
	MinBook        int
	Version        int
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

func (s *Store) GetInventoryUnit(ctx context.Context, id uuid.UUID) (*InventoryUnit, error) {
	var u InventoryUnit
	err := s.db.QueryRow(ctx, `
        SELECT unit_id, name, description, available_units, total_units,
               currency, price_minor, min_book, version, created_at, updated_at
        FROM inventory_units WHERE unit_id = $1`, id,
	).Scan(
		&u.UnitID, &u.Name, &u.Description, &u.AvailableUnits, &u.TotalUnits,
		&u.Currency, &u.PriceMinor, &u.MinBook, &u.Version, &u.CreatedAt, &u.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUnitNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get inventory unit: %w", err)
	}
	return &u, nil
}

func (s *Store) DecrementAvailability(ctx context.Context, id uuid.UUID, qty int) error {
	var pgErr *pgconn.PgError
	tag, err := s.db.Exec(ctx, `
		UPDATE inventory_units
		SET available_units = available_units - $1, updated_at = NOW()
		WHERE unit_id = $2
	`, qty, id)
	if errors.As(err, &pgErr) && pgErr.ConstraintName == "chk_availability" {
		return ErrSoldOut
	}
	if err != nil {
		return fmt.Errorf("decrement availability: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrUnitNotFound
	}
	return nil
}

// DecrementAvailabilityUnsafe writes an absolute value computed by the caller
// from a previously-read availability. This deliberately trusts a stale read
// and is retained only to demonstrate the lost-update anomaly under load.
// Production paths must use DecrementAvailability.
func (s *Store) DecrementAvailabilityUnsafe(ctx context.Context, id uuid.UUID, newAvailable int) error {
	var pgErr *pgconn.PgError
	tag, err := s.db.Exec(ctx, `
		UPDATE inventory_units SET available_units = $1 WHERE unit_id = $2
	`, newAvailable, id)
	if errors.As(err, &pgErr) && pgErr.ConstraintName == "chk_availability" {
		return ErrSoldOut
	}
	if err != nil {
		return fmt.Errorf("decrement availability: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrUnitNotFound
	}
	return nil
}
