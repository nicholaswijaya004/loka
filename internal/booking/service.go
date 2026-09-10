package booking

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"github.com/nicholaswijaya004/loka/internal/storage"
	"log/slog"
	"time"
)

type Service struct {
	store  Store
	logger *slog.Logger
	unsafe bool
}

func NewService(store Store, logger *slog.Logger, unsafe bool) *Service {
	return &Service{store: store, logger: logger, unsafe: unsafe}
}

type Store interface {
	GetInventoryUnit(ctx context.Context, id uuid.UUID) (*storage.InventoryUnit, error)
	DecrementAvailability(ctx context.Context, id uuid.UUID, qty int) error
	DecrementAvailabilityUnsafe(ctx context.Context, id uuid.UUID, newAvailable int) error
	InsertBooking(ctx context.Context, b *storage.Booking) error
	GetBooking(ctx context.Context, id uuid.UUID) (*storage.Booking, error)
	ClaimIdempotencyKey(ctx context.Context, key, requestHash string) (bool, error)
	GetIdempotencyKey(ctx context.Context, key string) (*storage.IdempotencyKey, error)
	CompleteIdempotencyKey(ctx context.Context, key string, bookingID uuid.UUID, responseStatus int, responseBody []byte) error
	ReleaseIdempotencyKey(ctx context.Context, key string) error
}

func (s *Service) Create(ctx context.Context, unitID, customerID uuid.UUID, qty int, visitDateTime time.Time) (*storage.Booking, error) {
	if qty <= 0 {
		return nil, ErrInvalidQty
	}
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

	if s.unsafe {
		err = s.store.DecrementAvailabilityUnsafe(ctx, unitID, unit.AvailableUnits-qty)
	} else {
		err = s.store.DecrementAvailability(ctx, unitID, qty)
	}

	if errors.Is(err, storage.ErrSoldOut) {
		return nil, ErrSoldOut
	}
	if err != nil {
		return nil, err
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
	err = s.store.InsertBooking(ctx, booking)
	if err != nil {
		return nil, err
	}
	return booking, nil
}

func (s *Service) Get(ctx context.Context, id uuid.UUID) (*storage.Booking, error) {
	return s.store.GetBooking(ctx, id)
}
