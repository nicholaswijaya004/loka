package payments

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// A charge made inside a span sends that trace to the provider in a
// traceparent header, so the provider's span joins the booking's trace.
func TestHTTPProviderSendsTheTrace(t *testing.T) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	tp := sdktrace.NewTracerProvider()
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	ctx, span := tp.Tracer("test").Start(context.Background(), "charge booking")
	defer span.End()

	var got string
	p := providerFor(t, time.Second, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("traceparent")
		respond(http.StatusOK, `{"charge_id":"ch_1","payment_status":"succeeded"}`)(w, r)
	})

	if _, err := p.Charge(ctx, testChargeReq); err != nil {
		t.Fatalf("charge: %v", err)
	}

	// 00-<trace id>-<span id>-<flags>: the trace id must be the caller's.
	parts := strings.Split(got, "-")
	if len(parts) != 4 || parts[1] != span.SpanContext().TraceID().String() {
		t.Errorf("traceparent %q: want trace id %s", got, span.SpanContext().TraceID())
	}
}

// Without a span there is nothing to continue: no header is sent.
func TestHTTPProviderWithoutTraceSendsNoHeader(t *testing.T) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	var got string
	p := providerFor(t, time.Second, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("traceparent")
		respond(http.StatusOK, `{"charge_id":"ch_1","payment_status":"succeeded"}`)(w, r)
	})

	if _, err := p.Charge(context.Background(), testChargeReq); err != nil {
		t.Fatalf("charge: %v", err)
	}
	if got != "" {
		t.Errorf("traceparent %q: want none", got)
	}
}
