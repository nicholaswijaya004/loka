package payments

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

var testChargeReq = ChargeRequest{
	IdempotencyKey: "key-1",
	BookingID:      uuid.MustParse("5e9acb5a-b1d0-4fcc-a45e-e260ab0018c5"),
	AmountMinor:    150_000_000,
	Currency:       "IDR",
}

// providerFor starts a throwaway HTTP server running h, and returns an
// HTTPProvider pointed at it. The server is closed when the test ends.
func providerFor(t *testing.T, timeout time.Duration, h http.HandlerFunc) *HTTPProvider {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return NewHTTPProvider(srv.URL, timeout)
}

func respond(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

// Every row of the status-code table: which return shape each response gets.
func TestHTTPProviderResponses(t *testing.T) {
	tests := []struct {
		name          string
		status        int
		body          string
		wantOutcome   ChargeOutcome // set when a result is expected
		wantChargeID  string
		wantReason    string
		wantSentinel  error // set when a specific sentinel is expected
		wantPermanent bool
	}{
		{name: "succeeded", status: 200,
			body:        `{"charge_id":"ch_1","payment_status":"succeeded"}`,
			wantOutcome: ChargeSucceeded, wantChargeID: "ch_1"},
		{name: "declined", status: 200,
			body:        `{"charge_id":"ch_2","payment_status":"declined","failure_reason":"insufficient_funds"}`,
			wantOutcome: ChargeDeclined, wantChargeID: "ch_2", wantReason: "insufficient_funds"},

		{name: "400 is our bug", status: 400, body: `{"error":"total_minor must be positive"}`,
			wantSentinel: ErrChargeRejected, wantPermanent: true},
		{name: "404 is our bug", status: 404, body: "not found",
			wantSentinel: ErrChargeRejected, wantPermanent: true},

		{name: "408 means try later", status: 408, body: "timeout"},
		{name: "429 means try later", status: 429, body: "slow down"},
		{name: "500 is unknown", status: 500, body: "boom"},
		{name: "503 is unknown", status: 503, body: "unavailable"},

		{name: "unknown payment_status", status: 200,
			body:         `{"charge_id":"ch_3","payment_status":"processing"}`,
			wantSentinel: ErrUnexpectedProviderResponse, wantPermanent: true},
		{name: "unreadable 200 body", status: 200, body: `not json`,
			wantSentinel: ErrUnexpectedProviderResponse, wantPermanent: true},
		{name: "succeeded without a charge_id", status: 200,
			body:         `{"payment_status":"succeeded"}`,
			wantSentinel: ErrUnexpectedProviderResponse, wantPermanent: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := providerFor(t, 2*time.Second, respond(tt.status, tt.body))

			res, err := p.Charge(context.Background(), testChargeReq)

			if tt.wantOutcome != "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if res.Outcome != tt.wantOutcome || res.ChargeID != tt.wantChargeID || res.DeclineReason != tt.wantReason {
					t.Errorf("result: got %+v", res)
				}
				return
			}

			if err == nil {
				t.Fatalf("got result %+v, want an error", res)
			}
			if res != nil {
				t.Errorf("result: got %+v, want nil alongside an error", res)
			}
			if tt.wantSentinel != nil && !errors.Is(err, tt.wantSentinel) {
				t.Errorf("error: got %v, want one wrapping %v", err, tt.wantSentinel)
			}
			if got := IsPermanent(err); got != tt.wantPermanent {
				t.Errorf("IsPermanent: got %v, want %v (error: %v)", got, tt.wantPermanent, err)
			}
		})
	}
}

// The escalation log must say what the provider objected to.
func TestHTTPProviderRejectionIncludesBody(t *testing.T) {
	p := providerFor(t, 2*time.Second, respond(400, `{"error":"total_minor must be positive"}`))

	_, err := p.Charge(context.Background(), testChargeReq)
	if err == nil || !strings.Contains(err.Error(), "total_minor must be positive") {
		t.Errorf("error: got %v, want it to include the provider's message", err)
	}
}

// The request matches the provider's contract: method, path, headers, body.
func TestHTTPProviderSendsTheRightRequest(t *testing.T) {
	var (
		gotMethod, gotPath, gotKey, gotType string
		gotBody                             chargeRequestBody
		decodeErr                           error
	)
	p := providerFor(t, 2*time.Second, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotKey, gotType = r.Header.Get("Idempotency-Key"), r.Header.Get("Content-Type")
		decodeErr = json.NewDecoder(r.Body).Decode(&gotBody)
		respond(200, `{"charge_id":"ch_1","payment_status":"succeeded"}`)(w, r)
	})

	if _, err := p.Charge(context.Background(), testChargeReq); err != nil {
		t.Fatalf("charge: %v", err)
	}

	if gotMethod != http.MethodPost || gotPath != "/charges" {
		t.Errorf("request: got %s %s, want POST /charges", gotMethod, gotPath)
	}
	if gotKey != testChargeReq.IdempotencyKey {
		t.Errorf("Idempotency-Key: got %q, want %q", gotKey, testChargeReq.IdempotencyKey)
	}
	if gotType != "application/json" {
		t.Errorf("Content-Type: got %q", gotType)
	}
	if decodeErr != nil {
		t.Fatalf("body is not valid JSON: %v", decodeErr)
	}
	want := chargeRequestBody{BookingID: testChargeReq.BookingID, TotalMinor: 150_000_000, Currency: "IDR"}
	if gotBody != want {
		t.Errorf("body: got %+v, want %+v", gotBody, want)
	}
}

// A provider slower than the client's timeout is "no answer": transient.
func TestHTTPProviderTimeoutIsTransient(t *testing.T) {
	p := providerFor(t, 100*time.Millisecond, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done(): // let the server shut down promptly
		}
	})

	start := time.Now()
	_, err := p.Charge(context.Background(), testChargeReq)

	if err == nil {
		t.Fatal("got nil, want a timeout error")
	}
	if IsPermanent(err) {
		t.Errorf("a timeout must be retriable, got permanent: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took %v; the client timeout was not applied", elapsed)
	}
}

// A provider that isn't running at all is also "no answer": transient.
func TestHTTPProviderConnectionRefusedIsTransient(t *testing.T) {
	srv := httptest.NewServer(respond(200, "{}"))
	url := srv.URL
	srv.Close() // nothing is listening at url any more

	_, err := NewHTTPProvider(url, time.Second).Charge(context.Background(), testChargeReq)
	if err == nil {
		t.Fatal("got nil, want a connection error")
	}
	if IsPermanent(err) {
		t.Errorf("connection refused must be retriable, got permanent: %v", err)
	}
}

// A cancelled context (the worker shutting down) aborts the call, and the
// error still says so, so the worker can tell shutdown from failure.
func TestHTTPProviderRespectsCancelledContext(t *testing.T) {
	p := providerFor(t, 2*time.Second, respond(200, `{"charge_id":"ch_1","payment_status":"succeeded"}`))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := p.Charge(ctx, testChargeReq)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error: got %v, want one wrapping context.Canceled", err)
	}
	if IsPermanent(err) {
		t.Errorf("a cancelled call must not be permanent: %v", err)
	}
}
