package payments

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/sony/gobreaker/v2"
)

// BreakerConfig sets when the circuit breaker opens and how it recovers.
type BreakerConfig struct {
	TripAfter   uint32        // consecutive provider failures that open the breaker
	OpenTimeout time.Duration // how long it stays open before a test call
	MaxRequests uint32        // test calls allowed while half-open
}

// BreakerProvider is a Provider that wraps another Provider with a circuit
// breaker. When the provider is clearly down, calls fail fast instead of each
// waiting out its timeout. The worker can't tell it apart from any other
// Provider, so it needs no changes.
//
// One BreakerProvider must be shared by every caller of the same provider:
// a breaker only works if it sees all the calls.
type BreakerProvider struct {
	next Provider
	cb   *gobreaker.CircuitBreaker[*ChargeResult]
}

var _ Provider = (*BreakerProvider)(nil)

func NewBreakerProvider(next Provider, cfg BreakerConfig, logger *slog.Logger) *BreakerProvider {
	cb := gobreaker.NewCircuitBreaker[*ChargeResult](gobreaker.Settings{
		Name:        "payment-provider",
		MaxRequests: cfg.MaxRequests,
		Interval:    0, // consecutive failures reset on any success anyway
		Timeout:     cfg.OpenTimeout,
		ReadyToTrip: func(c gobreaker.Counts) bool {
			return c.ConsecutiveFailures >= cfg.TripAfter
		},
		IsSuccessful: providerLooksHealthy,
		OnStateChange: func(name string, from, to gobreaker.State) {
			logger.Warn("circuit breaker state changed", "breaker", name, "from", from, "to", to)
		},
	})
	return &BreakerProvider{next: next, cb: cb}
}

// providerLooksHealthy decides which outcomes count against the provider.
// Only a transient error that we didn't cause does: a timeout, a refused
// connection, a 5xx. A decline (nil error), a rejected or unrecognised
// request (the provider answered; the fault is ours), and our own shutdown
// all say nothing bad about the provider's health.
func providerLooksHealthy(err error) bool {
	return err == nil || IsPermanent(err) || errors.Is(err, context.Canceled)
}

func (b *BreakerProvider) Charge(ctx context.Context, req ChargeRequest) (*ChargeResult, error) {
	res, err := b.cb.Execute(func() (*ChargeResult, error) {
		return b.next.Charge(ctx, req)
	})
	if errors.Is(err, gobreaker.ErrOpenState) || errors.Is(err, gobreaker.ErrTooManyRequests) {
		// The breaker refused without calling the provider. Transient: the
		// booking waits for its next attempt. Say so clearly in the logs,
		// since "breaker open" and "provider timed out" mean different things.
		return nil, fmt.Errorf("charge booking %s: circuit breaker refused the call: %w", req.BookingID, err)
	}
	return res, err
}
