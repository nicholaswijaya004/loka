package payments

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/sony/gobreaker/v2"
)

var errProviderDown = errors.New("provider timed out") // a transient failure

// scriptedProvider returns whatever the test sets, and counts real calls, so
// tests can see whether the breaker let a call through or refused it.
type scriptedProvider struct {
	mu    sync.Mutex
	calls int
	res   *ChargeResult
	err   error
}

func (p *scriptedProvider) Charge(context.Context, ChargeRequest) (*ChargeResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	return p.res, p.err
}

func (p *scriptedProvider) set(res *ChargeResult, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.res, p.err = res, err
}

func (p *scriptedProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

var succeeded = &ChargeResult{Outcome: ChargeSucceeded, ChargeID: "ch_1"}

func newTestBreaker(next Provider, openTimeout time.Duration) *BreakerProvider {
	return NewBreakerProvider(next, BreakerConfig{
		TripAfter:   5,
		OpenTimeout: openTimeout,
		MaxRequests: 1,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func charge(b *BreakerProvider) (*ChargeResult, error) {
	return b.Charge(context.Background(), testChargeReq)
}

// Five transient failures in a row open the breaker; the sixth call fails
// fast without reaching the provider, and is transient.
func TestBreakerOpensAfterConsecutiveFailures(t *testing.T) {
	fake := &scriptedProvider{err: errProviderDown}
	b := newTestBreaker(fake, time.Minute)

	for i := 0; i < 5; i++ {
		if _, err := charge(b); !errors.Is(err, errProviderDown) {
			t.Fatalf("call %d: got %v, want the provider's error passed through", i+1, err)
		}
	}

	_, err := charge(b)
	if !errors.Is(err, gobreaker.ErrOpenState) {
		t.Errorf("sixth call: got %v, want gobreaker.ErrOpenState", err)
	}
	if IsPermanent(err) {
		t.Error("a refused call must be transient, so the booking is retried later")
	}
	if got := fake.callCount(); got != 5 {
		t.Errorf("provider calls: got %d, want 5 — the open breaker must not call it", got)
	}
}

// Outcomes that don't mean the provider is sick must never open the breaker,
// no matter how many there are.
func TestBreakerIgnoresHealthySignals(t *testing.T) {
	tests := []struct {
		name string
		res  *ChargeResult
		err  error
	}{
		{"declines", &ChargeResult{Outcome: ChargeDeclined, DeclineReason: "insufficient_funds"}, nil},
		{"rejected requests", nil, fmt.Errorf("%w: status 400", ErrChargeRejected)},
		{"unrecognised responses", nil, fmt.Errorf("%w: unknown status", ErrUnexpectedProviderResponse)},
		{"our own shutdown", nil, fmt.Errorf("no answer: %w", context.Canceled)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &scriptedProvider{res: tt.res, err: tt.err}
			b := newTestBreaker(fake, time.Minute)

			for i := 0; i < 20; i++ {
				_, err := charge(b)
				if errors.Is(err, gobreaker.ErrOpenState) {
					t.Fatalf("breaker opened after %d %s", i, tt.name)
				}
			}
			if got := fake.callCount(); got != 20 {
				t.Errorf("provider calls: got %d, want all 20 to go through", got)
			}
		})
	}
}

// A success in between resets the count: failures must be consecutive.
func TestBreakerSuccessResetsFailureCount(t *testing.T) {
	fake := &scriptedProvider{err: errProviderDown}
	b := newTestBreaker(fake, time.Minute)

	for i := 0; i < 4; i++ {
		_, _ = charge(b)
	}
	fake.set(succeeded, nil)
	if _, err := charge(b); err != nil {
		t.Fatalf("success: %v", err)
	}
	fake.set(nil, errProviderDown)
	for i := 0; i < 4; i++ {
		if _, err := charge(b); errors.Is(err, gobreaker.ErrOpenState) {
			t.Fatalf("breaker opened after only %d failures since the last success", i+1)
		}
	}
}

// After the open timeout, one test call goes through. If it succeeds, the
// breaker closes and normal traffic flows again.
func TestBreakerRecoversAfterTimeout(t *testing.T) {
	fake := &scriptedProvider{err: errProviderDown}
	b := newTestBreaker(fake, 50*time.Millisecond)

	for i := 0; i < 5; i++ {
		_, _ = charge(b)
	}
	if _, err := charge(b); !errors.Is(err, gobreaker.ErrOpenState) {
		t.Fatalf("breaker should be open, got %v", err)
	}

	fake.set(succeeded, nil) // the provider recovers
	time.Sleep(100 * time.Millisecond)

	res, err := charge(b) // the half-open test call
	if err != nil || res.Outcome != ChargeSucceeded {
		t.Fatalf("test call: got %+v, %v; want a success", res, err)
	}
	for i := 0; i < 3; i++ {
		if _, err := charge(b); err != nil {
			t.Errorf("call %d after recovery: %v", i+1, err)
		}
	}
}

// If the test call fails, the breaker opens again immediately.
func TestBreakerReopensWhenTestCallFails(t *testing.T) {
	fake := &scriptedProvider{err: errProviderDown}
	b := newTestBreaker(fake, 50*time.Millisecond)

	for i := 0; i < 5; i++ {
		_, _ = charge(b)
	}
	time.Sleep(100 * time.Millisecond)

	before := fake.callCount()
	_, _ = charge(b) // the test call: provider still down
	if got := fake.callCount() - before; got != 1 {
		t.Fatalf("test call: provider called %d times, want exactly 1", got)
	}
	if _, err := charge(b); !errors.Is(err, gobreaker.ErrOpenState) {
		t.Errorf("after a failed test call: got %v, want the breaker open again", err)
	}
}

// While half-open, only MaxRequests calls may probe the provider; the rest
// are refused, and refused as transient.
func TestBreakerHalfOpenAllowsOneProbe(t *testing.T) {
	gate := make(chan struct{})
	slow := &blockingProvider{release: gate}
	b := newTestBreaker(slow, 50*time.Millisecond)

	slow.failNext(5)
	for i := 0; i < 5; i++ {
		_, _ = charge(b)
	}
	time.Sleep(100 * time.Millisecond)

	probeDone := make(chan struct{})
	go func() { _, _ = charge(b); close(probeDone) }() // the probe, held inside the provider
	slow.waitForCall(t)

	_, err := charge(b) // a second caller while the probe is in flight
	if !errors.Is(err, gobreaker.ErrTooManyRequests) {
		t.Errorf("second call while half-open: got %v, want gobreaker.ErrTooManyRequests", err)
	}
	if IsPermanent(err) {
		t.Error("a refused call must be transient")
	}

	close(gate)
	<-probeDone
}

// blockingProvider fails a set number of calls, then holds later calls until
// released, so a test can observe the breaker while a probe is in flight.
type blockingProvider struct {
	mu       sync.Mutex
	failures int
	release  chan struct{}
	entered  chan struct{}
}

func (p *blockingProvider) failNext(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failures = n
	p.entered = make(chan struct{}, 1)
}

func (p *blockingProvider) waitForCall(t *testing.T) {
	t.Helper()
	select {
	case <-p.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the probe never reached the provider")
	}
}

func (p *blockingProvider) Charge(context.Context, ChargeRequest) (*ChargeResult, error) {
	p.mu.Lock()
	if p.failures > 0 {
		p.failures--
		p.mu.Unlock()
		return nil, errProviderDown
	}
	p.mu.Unlock()

	p.entered <- struct{}{}
	<-p.release
	return succeeded, nil
}
