package booking

import (
	"context"
	"errors"
	"log/slog"
	"math/rand"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/nicholaswijaya004/loka/internal/storage"
)

type Service struct {
	store    Store
	logger   *slog.Logger
	unsafe   bool
	strategy string
	retries  atomic.Int64
}

func NewService(store Store, logger *slog.Logger, unsafe bool, strategy string) *Service {
	return &Service{store: store, logger: logger, unsafe: unsafe, strategy: strategy}
}

type Store interface {
	GetInventoryUnit(ctx context.Context, id uuid.UUID) (*storage.InventoryUnit, error)
	GetInventoryUnitForUpdate(ctx context.Context, id uuid.UUID) (*storage.InventoryUnit, error)
	DecrementAvailability(ctx context.Context, id uuid.UUID, qty int) error
	DecrementAvailabilityUnsafe(ctx context.Context, id uuid.UUID, newAvailable int) error
	DecrementAvailabilityOptimistic(ctx context.Context, id uuid.UUID, qty int, version int) error
	InsertBooking(ctx context.Context, b *storage.Booking) error
	GetBooking(ctx context.Context, id uuid.UUID) (*storage.Booking, error)
	ClaimIdempotencyKey(ctx context.Context, key, requestHash string) (bool, error)
	GetIdempotencyKey(ctx context.Context, key string) (*storage.IdempotencyKey, error)
	CompleteIdempotencyKey(ctx context.Context, key string, bookingID uuid.UUID, responseStatus int, responseBody []byte) error
	ReleaseIdempotencyKey(ctx context.Context, key string) error
	WithTx(ctx context.Context, fn func(Store) error) error
}

const (
	maxOptimisticBackoff = 2 * time.Second
	backoffBase          = 10 * time.Millisecond
	maxBackoffShift      = 10
)

func (s *Service) Create(ctx context.Context, unitID, customerID uuid.UUID, qty int, visitDateTime time.Time) (*storage.Booking, error) {
	if qty <= 0 {
		return nil, ErrInvalidQty
	}

	if s.strategy == "forupdate" {
		return s.createForUpdate(ctx, unitID, customerID, qty, visitDateTime)
	}
	if s.strategy == "optimistic" {
		return s.createOptimistic(ctx, unitID, customerID, qty, visitDateTime)
	}
	return s.createSingleStatement(ctx, unitID, customerID, qty, visitDateTime)
}

const maxOptimisticRetries = 50

func (s *Service) createOptimistic(ctx context.Context, unitID, customerID uuid.UUID, qty int, visitDateTime time.Time) (*storage.Booking, error) {
	for attempt := 0; attempt < maxOptimisticRetries; attempt++ {
		unit, err := s.store.GetInventoryUnit(ctx, unitID)
		if err != nil {
			return nil, err
		}
		if unit.MinBook > qty {
			return nil, ErrMinBook
		}
		if unit.AvailableUnits < qty {
			return nil, ErrSoldOut
		}

		// Create a new booking
		booking := &storage.Booking{
			UnitID:        unitID,
			CustomerID:    customerID,
			Qty:           qty,
			VisitDateTime: visitDateTime,
			TotalMinor:    int64(qty) * unit.PriceMinor,
			Currency:      unit.Currency,
			BookingStatus: "pending",
		}

		err = s.store.WithTx(ctx, func(tx Store) error {
			if err := tx.DecrementAvailabilityOptimistic(ctx, unitID, qty, unit.Version); err != nil {
				return err
			}
			return tx.InsertBooking(ctx, booking)
		})

		if errors.Is(err, storage.ErrVersionConflict) {
			s.retries.Add(1)
			shift := attempt
			if shift > maxBackoffShift {
				shift = maxBackoffShift
			}
			backoff := time.Duration(1<<shift) * backoffBase
			if backoff > maxOptimisticBackoff {
				backoff = maxOptimisticBackoff
			}
			jitter := time.Duration(rand.Int63n(int64(backoff)))
			select {
			case <-time.After(jitter):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			continue
		}
		if errors.Is(err, storage.ErrSoldOut) {
			return nil, ErrSoldOut
		}
		if err != nil {
			return nil, err
		}
		return booking, nil
	}
	return nil, ErrTooManyRetries
}

func (s *Service) createForUpdate(ctx context.Context, unitID, customerID uuid.UUID, qty int, visitDateTime time.Time) (*storage.Booking, error) {
	var booking *storage.Booking

	err := s.store.WithTx(ctx, func(tx Store) error {
		// Check if the inventory unit exists and is available
		unit, err := tx.GetInventoryUnitForUpdate(ctx, unitID)
		if err != nil {
			return err
		}
		if unit.MinBook > qty {
			return ErrMinBook
		}
		if unit.AvailableUnits < qty {
			return ErrSoldOut
		}

		// Create a new booking
		booking = &storage.Booking{
			UnitID:        unitID,
			CustomerID:    customerID,
			Qty:           qty,
			VisitDateTime: visitDateTime,
			TotalMinor:    int64(qty) * unit.PriceMinor,
			Currency:      unit.Currency,
			BookingStatus: "pending",
		}
		if s.unsafe {
			if err := tx.DecrementAvailabilityUnsafe(ctx, unitID, unit.AvailableUnits-qty); err != nil {
				return err
			}
		} else {
			if err := tx.DecrementAvailability(ctx, unitID, qty); err != nil {
				return err
			}
		}
		return tx.InsertBooking(ctx, booking)
	})

	if errors.Is(err, storage.ErrSoldOut) {
		return nil, ErrSoldOut
	}
	if err != nil {
		return nil, err
	}

	return booking, nil
}

func (s *Service) createSingleStatement(ctx context.Context, unitID, customerID uuid.UUID, qty int, visitDateTime time.Time) (*storage.Booking, error) {
	// Check if the inventory unit exists and is available
	unit, err := s.store.GetInventoryUnit(ctx, unitID)
	if err != nil {
		return nil, err
	}
	if unit.MinBook > qty {
		return nil, ErrMinBook
	}
	if unit.AvailableUnits < qty {
		return nil, ErrSoldOut
	}

	// Create a new booking
	booking := &storage.Booking{
		UnitID:        unitID,
		CustomerID:    customerID,
		Qty:           qty,
		VisitDateTime: visitDateTime,
		TotalMinor:    int64(qty) * unit.PriceMinor,
		Currency:      unit.Currency,
		BookingStatus: "pending",
	}

	err = s.store.WithTx(ctx, func(tx Store) error {
		if s.unsafe {
			if err := tx.DecrementAvailabilityUnsafe(ctx, unitID, unit.AvailableUnits-qty); err != nil {
				return err
			}
		} else {
			if err := tx.DecrementAvailability(ctx, unitID, qty); err != nil {
				return err
			}
		}
		return tx.InsertBooking(ctx, booking)
	})

	if errors.Is(err, storage.ErrSoldOut) {
		return nil, ErrSoldOut
	}
	if err != nil {
		return nil, err
	}

	return booking, nil
}

func (s *Service) Get(ctx context.Context, id uuid.UUID) (*storage.Booking, error) {
	return s.store.GetBooking(ctx, id)
}

func (s *Service) Retries() int64 { return s.retries.Load() }
