package payments

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

// ChargeOutcome is the provider's answer to a charge it received.
type ChargeOutcome string

const (
	ChargeSucceeded ChargeOutcome = "succeeded"
	ChargeDeclined  ChargeOutcome = "declined"
)

// ErrChargeRejected means the provider rejected our request itself (for
// example a 400): the request is malformed, which is a bug in our code or bad
// data. Retrying the same request can never succeed, and it is not a decline,
// so the booking must not be cancelled. The worker escalates instead.
//
// Any error from Charge that does not wrap ErrChargeRejected is treated as
// "no answer" (timeout, connection refused, 5xx) and retried later.
var ErrChargeRejected = errors.New("payment provider rejected the request")

// ErrUnexpectedProviderResponse means the provider answered, but not in a
// form we understand (an unknown status, an unreadable body). Whether money
// moved is unknown, and retrying replays the same answer, so it is escalated,
// never treated as a decline.
var ErrUnexpectedProviderResponse = errors.New("unexpected payment provider response")

// ChargeRequest is one charge to send. IdempotencyKey must be the same on
// every retry of the same attempt, so the provider charges at most once.
type ChargeRequest struct {
	IdempotencyKey string
	BookingID      uuid.UUID
	AmountMinor    int64
	Currency       string
}

// ChargeResult is a charge the provider answered: succeeded or declined.
// A decline is a result, not an error.
type ChargeResult struct {
	Outcome       ChargeOutcome
	ChargeID      string // the provider's id; stored as payments.provider_payment_id
	DeclineReason string // set when Outcome is ChargeDeclined
}

// Provider charges money. Charge returns exactly one of:
//   - a result and nil: the provider answered (succeeded or declined);
//   - nil and an error wrapping ErrChargeRejected: our request is invalid;
//   - nil and any other error: no answer; safe to retry with the same key.
type Provider interface {
	Charge(ctx context.Context, req ChargeRequest) (*ChargeResult, error)
}

// IsPermanent reports whether retrying the same charge can never help. The
// worker escalates these instead of waiting for the lease to retry.
func IsPermanent(err error) bool {
	return errors.Is(err, ErrChargeRejected) || errors.Is(err, ErrUnexpectedProviderResponse)
}
