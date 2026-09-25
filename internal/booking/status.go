package booking

import (
	"errors"
	"fmt"
)

const (
	StatusPending        = "pending"
	StatusPaymentPending = "payment_pending"
	StatusConfirmed      = "confirmed"
	StatusCancelled      = "cancelled"
)

// ErrIllegalTransition means the requested move isn't part of the booking
// state machine. It is always a bug in the caller, never a race.
var ErrIllegalTransition = errors.New("illegal booking status transition")

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
