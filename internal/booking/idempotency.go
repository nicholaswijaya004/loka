package booking

import (
	"context"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/nicholaswijaya004/loka/internal/storage"
	"time"
)

func (s *Service) CreateIdempotent(
	ctx context.Context,
	key, hash string,
	unitID, customerID uuid.UUID,
	qty int,
	visitDateTime time.Time,
) (*storage.Booking, bool, error) {
	claimed, err := s.store.ClaimIdempotencyKey(ctx, key, hash)
	if err != nil {
		return nil, false, err
	}

	if !claimed {
		existing, err := s.store.GetIdempotencyKey(ctx, key)
		if errors.Is(err, storage.ErrIdempotencyKeyNotFound) {
			return nil, false, ErrRequestInFlight
		}

		if err != nil {
			return nil, false, err
		}

		if existing.RequestHash != hash {
			return nil, false, ErrKeyReused
		}

		if existing.State == "in_progress" {
			return nil, false, ErrRequestInFlight
		}

		if existing.State == "completed" {
			if existing.BookingID == nil {
				return nil, false, ErrCorruptIdempotencyRecord
			}
			b, err := s.store.GetBooking(ctx, *existing.BookingID)
			if err != nil {
				return nil, false, err
			}
			return b, true, nil
		}

		return nil, false, fmt.Errorf("%w: %q for key %s", ErrUnexpectedIdempotencyState, existing.State, key)
	}

	booking, err := s.Create(ctx, unitID, customerID, qty, visitDateTime)
	if err != nil {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		errIn := s.store.ReleaseIdempotencyKey(releaseCtx, key)
		if errIn != nil {
			s.logger.Error("failed to release idempotency key", "error", errIn)
		}
		return nil, false, err
	}

	// The response body is not stored. The booking id is enough to reconstruct
	// the response on replay, and re-fetching keeps this layer ignorant of the
	// API's response shape. If replay latency ever mattered, caching the
	// serialized body here would be the optimization — at the cost of coupling
	// the service to the transport.
	err = s.store.CompleteIdempotencyKey(ctx, key, booking.BookingID, 201, nil)
	if err != nil {
		return nil, false, err
	}

	return booking, false, nil
}
