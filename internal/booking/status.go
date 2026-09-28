package booking

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

type Transitioner interface {
	TransitionBookingStatus(ctx context.Context, id uuid.UUID, from, to string, failureReason *string) error
}

const (
	StatusPending        = "pending"
	StatusPaymentPending = "payment_pending"
	StatusConfirmed      = "confirmed"
	StatusCancelled      = "cancelled"
)

// allowedTransitions is the booking state machine. Anything not listed is
// rejected before it reaches the database.
var allowedTransitions = map[string][]string{
	StatusPending:        {StatusPaymentPending, StatusCancelled}, // charge starts / expiry
	StatusPaymentPending: {StatusConfirmed, StatusCancelled},      // charge succeeded / declined
}

func checkTransition(from, to string) error {
	for _, allowed := range allowedTransitions[from] {
		if allowed == to {
			return nil
		}
	}
	return fmt.Errorf("%w: %s → %s", ErrIllegalTransition, from, to)
}

func Transition(ctx context.Context, tr Transitioner, id uuid.UUID, from, to string, reason *string) error {
	if err := checkTransition(from, to); err != nil {
		return err
	}
	return tr.TransitionBookingStatus(ctx, id, from, to, reason)
}
