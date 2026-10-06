package booking

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/nicholaswijaya004/loka/internal/storage"
	"github.com/nicholaswijaya004/loka/internal/telemetry"
)

const (
	defaultMaxOptimisticRetries   = 50
	defaultMaxSerializableRetries = 50
	bookingAggregate              = "booking"
	bookingCreatedVersion         = 1
	eventBookingCreated           = "booking.created"
)

type Service struct {
	store    Store
	reader   Reader
	cache    UnitCache
	logger   *slog.Logger
	unsafe   bool
	strategy string
	retries  atomic.Int64

	maxOptimisticRetries   int
	maxSerializableRetries int
}

type bookingCreatedPayload struct {
	Version       int       `json:"version"`
	BookingID     uuid.UUID `json:"booking_id"`
	UnitID        uuid.UUID `json:"unit_id"`
	CustomerID    uuid.UUID `json:"customer_id"`
	Qty           int       `json:"qty"`
	VisitDateTime time.Time `json:"visit_date_time"`
	TotalMinor    int64     `json:"total_minor"`
	Currency      string    `json:"currency"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"created_at"`
}

// Reader serves the read-only endpoints (GET /bookings/{id}, GET /units/{id})
// through their own connection pool. It reads the same primary as the write
// path, so a GET right after a 201 always finds the booking. The booking path
// never uses it.
type Reader interface {
	GetBooking(ctx context.Context, id uuid.UUID) (*storage.Booking, error)
	GetInventoryUnit(ctx context.Context, id uuid.UUID) (*storage.InventoryUnit, error)
}

type Option func(*Service)

// UnitCache holds units for the availability page. Get returns ErrCacheMiss
// when it has no entry for the unit; any other error means the cache itself
// failed. Entries may be stale, so the booking path never reads it.
type UnitCache interface {
	Get(ctx context.Context, id uuid.UUID) (*storage.InventoryUnit, error)
	Set(ctx context.Context, unit *storage.InventoryUnit) error
}

// noCache is the UnitCache when caching is off: every Get is a miss and Set
// keeps nothing, so GetUnit always reads Postgres without a nil check.
type noCache struct{}

func (noCache) Get(ctx context.Context, id uuid.UUID) (*storage.InventoryUnit, error) {
	return nil, ErrCacheMiss
}

func (noCache) Set(ctx context.Context, unit *storage.InventoryUnit) error {
	return nil
}

// WithReader sends Get to r instead of the write store, so reads can use
// their own connection pool.
func WithReader(r Reader) Option {
	return func(s *Service) { s.reader = r }
}

// WithUnitCache puts c in front of GetUnit. Without it, GetUnit reads
// Postgres on every call.
func WithUnitCache(c UnitCache) Option {
	return func(s *Service) { s.cache = c }
}

func NewService(store Store, logger *slog.Logger, unsafe bool, strategy string, opts ...Option) *Service {
	s := &Service{
		store:                  store,
		reader:                 store,
		cache:                  noCache{},
		logger:                 logger,
		unsafe:                 unsafe,
		strategy:               strategy,
		maxOptimisticRetries:   defaultMaxOptimisticRetries,
		maxSerializableRetries: defaultMaxSerializableRetries,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
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
	WithSerializableTx(ctx context.Context, fn func(Store) error) error
	InsertOutboxEvent(ctx context.Context, o *storage.Outbox) error
}

func (s *Service) waitWithBackoff(ctx context.Context, attempt int) bool {
	const (
		backoffBase     = 10 * time.Millisecond
		maxBackoff      = 2 * time.Second
		maxBackoffShift = 10
	)
	shift := attempt
	if shift > maxBackoffShift {
		shift = maxBackoffShift
	}
	backoff := time.Duration(1<<shift) * backoffBase
	if backoff > maxBackoff {
		backoff = maxBackoff
	}
	jitter := time.Duration(rand.Int63n(int64(backoff)))
	select {
	case <-time.After(jitter):
		return true
	case <-ctx.Done():
		return false
	}
}

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
	if s.strategy == "serializable" {
		return s.createSerializable(ctx, unitID, customerID, qty, visitDateTime)
	}
	return s.createSingleStatement(ctx, unitID, customerID, qty, visitDateTime)
}

// GetUnit returns a unit for the availability page: the cache first, then the
// read pool. A cache failure never fails the request; it costs a Postgres read
// and a warning.
func (s *Service) GetUnit(ctx context.Context, id uuid.UUID) (*storage.InventoryUnit, error) {
	unit, err := s.cache.Get(ctx, id)
	if err == nil {
		return unit, nil
	}
	cacheDown := !errors.Is(err, ErrCacheMiss)
	if cacheDown {
		s.logger.Warn("unit cache unavailable, reading postgres", "unit_id", id, "error", err)
	}

	unit, err = s.reader.GetInventoryUnit(ctx, id)
	if err != nil {
		return nil, err
	}

	// Storing into a cache that just failed would only add another timeout.
	if !cacheDown {
		if err := s.cache.Set(ctx, unit); err != nil {
			s.logger.Warn("unit cache store failed", "unit_id", id, "error", err)
		}
	}
	return unit, nil
}

func (s *Service) insertBookingWithEvent(ctx context.Context, tx Store, b *storage.Booking) error {
	// The request's trace travels with both rows: the relay reads it from the
	// outbox row (Kafka headers), the worker and expirer from the booking row.
	headers := telemetry.Inject(ctx)
	b.TraceContext = headers

	if err := tx.InsertBooking(ctx, b); err != nil {
		return err
	}

	payload, err := json.Marshal(bookingCreatedPayload{
		Version:       bookingCreatedVersion,
		BookingID:     b.BookingID,
		UnitID:        b.UnitID,
		CustomerID:    b.CustomerID,
		Qty:           b.Qty,
		VisitDateTime: b.VisitDateTime,
		TotalMinor:    b.TotalMinor,
		Currency:      b.Currency,
		Status:        b.BookingStatus,
		CreatedAt:     b.CreatedAt,
	})
	if err != nil {
		return fmt.Errorf("marshal booking.created payload: %w", err)
	}

	return tx.InsertOutboxEvent(ctx, &storage.Outbox{
		AggregateType: bookingAggregate,
		AggregateID:   b.BookingID,
		EventType:     eventBookingCreated,
		Payload:       payload,
		TraceContext:  headers,
	})
}

func (s *Service) createSerializable(ctx context.Context, unitID, customerID uuid.UUID, qty int, visitDateTime time.Time) (*storage.Booking, error) {
	var booking *storage.Booking

	for attempt := 0; attempt < s.maxSerializableRetries; attempt++ {
		err := s.store.WithSerializableTx(ctx, func(tx Store) error {
			unit, err := tx.GetInventoryUnit(ctx, unitID)
			if err != nil {
				return err
			}
			if unit.MinBook > qty {
				return ErrMinBook
			}
			if unit.AvailableUnits < qty {
				return ErrSoldOut
			}

			booking = &storage.Booking{
				UnitID:        unitID,
				CustomerID:    customerID,
				Qty:           qty,
				VisitDateTime: visitDateTime,
				TotalMinor:    int64(qty) * unit.PriceMinor,
				Currency:      unit.Currency,
				BookingStatus: "pending",
			}
			if err := tx.DecrementAvailability(ctx, unitID, qty); err != nil {
				return err
			}
			return s.insertBookingWithEvent(ctx, tx, booking)
		})

		if errors.Is(err, storage.ErrSerializationFailure) {
			s.retries.Add(1)
			if !s.waitWithBackoff(ctx, attempt) {
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

func (s *Service) createOptimistic(ctx context.Context, unitID, customerID uuid.UUID, qty int, visitDateTime time.Time) (*storage.Booking, error) {
	for attempt := 0; attempt < s.maxOptimisticRetries; attempt++ {
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
			return s.insertBookingWithEvent(ctx, tx, booking)
		})

		if errors.Is(err, storage.ErrVersionConflict) {
			s.retries.Add(1)
			if !s.waitWithBackoff(ctx, attempt) {
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
		return s.insertBookingWithEvent(ctx, tx, booking)
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
		return s.insertBookingWithEvent(ctx, tx, booking)
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
	return s.reader.GetBooking(ctx, id)
}

func (s *Service) Retries() int64 { return s.retries.Load() }
