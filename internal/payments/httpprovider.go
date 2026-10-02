package payments

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// HTTPProvider is the Provider that talks to a payment provider over HTTP
// (the paymock locally). It translates HTTP into the Provider contract.
type HTTPProvider struct {
	client  *http.Client
	baseURL string
}

var _ Provider = (*HTTPProvider)(nil)

// NewHTTPProvider needs a timeout shorter than the worker's lease, so a worker
// always gives up on its call before another worker can claim the booking.
func NewHTTPProvider(baseURL string, timeout time.Duration) *HTTPProvider {
	return &HTTPProvider{
		client: &http.Client{
			Timeout: timeout,
			// A client span per call, and the traceparent header on the request,
			// so the provider's own span joins the charge's trace.
			Transport: otelhttp.NewTransport(http.DefaultTransport,
				otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
					return r.Method + " " + r.URL.Path
				}),
			),
		},
		baseURL: strings.TrimRight(baseURL, "/"),
	}
}

// The provider's wire format: the paymock's contract, not ours.
type chargeRequestBody struct {
	BookingID  uuid.UUID `json:"booking_id"`
	TotalMinor int64     `json:"total_minor"`
	Currency   string    `json:"currency"`
}

type chargeResponseBody struct {
	ChargeID      string `json:"charge_id"`
	PaymentStatus string `json:"payment_status"`
	FailureReason string `json:"failure_reason"`
}

const maxBody = 64 << 10 // cap on any response body we read

func (p *HTTPProvider) Charge(ctx context.Context, req ChargeRequest) (*ChargeResult, error) {
	body, err := json.Marshal(chargeRequestBody{
		BookingID:  req.BookingID,
		TotalMinor: req.AmountMinor,
		Currency:   req.Currency,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: encode request: %v", ErrChargeRejected, err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/charges", bytes.NewReader(body))
	if err != nil {
		// Only a malformed base URL gets here: a configuration bug.
		return nil, fmt.Errorf("%w: build request: %v", ErrChargeRejected, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Idempotency-Key", req.IdempotencyKey)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		// No HTTP answer at all: timeout, connection refused, ctx cancelled.
		// Money may or may not have moved; the same key makes a retry safe.
		return nil, fmt.Errorf("charge booking %s: no answer: %w", req.BookingID, err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch code := resp.StatusCode; {
	case code == http.StatusOK:
		return decodeChargeResult(resp.Body, req.BookingID)

	case code == http.StatusRequestTimeout || code == http.StatusTooManyRequests:
		// 4xx, but meaning "try again later".
		return nil, fmt.Errorf("charge booking %s: provider busy: status %d", req.BookingID, code)

	case code >= 400 && code < 500:
		// Our request is wrong (bad payload, credentials, URL). Retrying the
		// same request can't help, and it is not the customer's decline.
		return nil, fmt.Errorf("%w: booking %s: status %d: %s",
			ErrChargeRejected, req.BookingID, code, readSnippet(resp.Body))

	default:
		// 5xx or anything else: the provider failed, possibly after charging.
		// Unknown outcome: never a decline; retry with the same key.
		return nil, fmt.Errorf("charge booking %s: provider error: status %d: %s",
			req.BookingID, code, readSnippet(resp.Body))
	}
}

func decodeChargeResult(r io.Reader, bookingID uuid.UUID) (*ChargeResult, error) {
	var b chargeResponseBody
	if err := json.NewDecoder(io.LimitReader(r, maxBody)).Decode(&b); err != nil {
		return nil, fmt.Errorf("%w: booking %s: unreadable 200 body: %v",
			ErrUnexpectedProviderResponse, bookingID, err)
	}

	switch b.PaymentStatus {
	case string(ChargeSucceeded):
		if b.ChargeID == "" {
			return nil, fmt.Errorf("%w: booking %s: succeeded without a charge_id",
				ErrUnexpectedProviderResponse, bookingID)
		}
		return &ChargeResult{Outcome: ChargeSucceeded, ChargeID: b.ChargeID}, nil

	case string(ChargeDeclined):
		return &ChargeResult{Outcome: ChargeDeclined, ChargeID: b.ChargeID, DeclineReason: b.FailureReason}, nil

	default:
		return nil, fmt.Errorf("%w: booking %s: unknown payment_status %q",
			ErrUnexpectedProviderResponse, bookingID, b.PaymentStatus)
	}
}

// readSnippet returns the start of a body, for error messages.
func readSnippet(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, 1024))
	return strings.TrimSpace(string(b))
}
