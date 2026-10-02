//go:build integration

package booking

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/nicholaswijaya004/loka/internal/telemetry"
)

var (
	expirerRecorderOnce sync.Once
	expirerSpans        *tracetest.SpanRecorder
)

// expirerRecorder installs a recording global provider, once per test
// binary. The package's tracer binds to the first global provider it sees,
// so a provider installed by a later test would never receive a span.
func expirerRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	expirerRecorderOnce.Do(func() {
		expirerSpans = tracetest.NewSpanRecorder()
		otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(expirerSpans)))
	})
	otel.SetTextMapPropagator(propagation.TraceContext{})
	return expirerSpans
}

// storeBookingTrace stores a new request span on the booking row, as
// POST /bookings does, and returns it. The span comes from its own provider,
// so it is not in the recorder.
func storeBookingTrace(t *testing.T, bookingID uuid.UUID) trace.SpanContext {
	t.Helper()
	provider := sdktrace.NewTracerProvider()
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	ctx, span := provider.Tracer("test").Start(context.Background(), "POST /bookings")
	span.End()
	execSQL(t, `UPDATE bookings SET trace_context = $2 WHERE booking_id = $1`,
		bookingID, telemetry.Inject(ctx))
	return span.SpanContext()
}

// expireSpanFor returns the one ended "expire booking" span for a booking.
func expireSpanFor(t *testing.T, rec *tracetest.SpanRecorder, bookingID uuid.UUID) sdktrace.ReadOnlySpan {
	t.Helper()
	var found []sdktrace.ReadOnlySpan
	for _, s := range rec.Ended() {
		if s.Name() != "expire booking" {
			continue
		}
		for _, a := range s.Attributes() {
			if a.Key == "booking.id" && a.Value.AsString() == bookingID.String() {
				found = append(found, s)
			}
		}
	}
	if len(found) != 1 {
		t.Fatalf("booking %s: got %d expire spans, want 1", bookingID, len(found))
	}
	return found[0]
}

func intAttr(s sdktrace.ReadOnlySpan, key string) (int64, bool) {
	for _, a := range s.Attributes() {
		if string(a.Key) == key {
			return a.Value.AsInt64(), true
		}
	}
	return 0, false
}

// cancelledTraceparent returns the traceparent on the booking's
// booking.cancelled outbox row; "" where trace_context is NULL.
func cancelledTraceparent(t *testing.T, bookingID uuid.UUID) string {
	t.Helper()
	var tp string
	if err := testPool.QueryRow(context.Background(), `
		SELECT coalesce(trace_context->>'traceparent', '') FROM outbox_events
		WHERE aggregate_id = $1 AND event_type = $2`,
		bookingID, EventBookingCancelled).Scan(&tp); err != nil {
		t.Fatalf("read cancelled event trace: %v", err)
	}
	return tp
}

func traceparentOf(sc trace.SpanContext) string {
	return fmt.Sprintf("00-%s-%s-01", sc.TraceID(), sc.SpanID())
}

// Each expired booking gets its own span, in its own trace, under the request
// that created it, and its booking.cancelled event carries that span, so the
// relay and the consumers continue the same trace. One batch, two traces.
func TestExpirerContinuesEachBookingsTrace(t *testing.T) {
	resetDB(t)
	rec := expirerRecorder(t)
	ids := []uuid.UUID{createHeldBooking(t, testUnitID, 1), createHeldBooking(t, testUnitID, 1)}
	parents := make([]trace.SpanContext, len(ids))
	for i, id := range ids {
		parents[i] = storeBookingTrace(t, id)
		makeOld(t, id, 20*time.Minute)
	}

	if _, err := newTestExpirer(50).RunOnce(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	for i, id := range ids {
		span := expireSpanFor(t, rec, id)
		if got, want := span.SpanContext().TraceID(), parents[i].TraceID(); got != want {
			t.Errorf("booking %d: trace id %s, want the booking's trace %s", i, got, want)
		}
		if got, want := span.Parent().SpanID(), parents[i].SpanID(); got != want {
			t.Errorf("booking %d: parent span %s, want the request span %s", i, got, want)
		}
		if span.Status().Code != codes.Unset {
			t.Errorf("booking %d: status %v, want unset", i, span.Status().Code)
		}
		if n, ok := intAttr(span, "expire.batch_size"); !ok || n != 2 {
			t.Errorf("booking %d: expire.batch_size %d (set: %v), want 2", i, n, ok)
		}
		if got, want := cancelledTraceparent(t, id), traceparentOf(span.SpanContext()); got != want {
			t.Errorf("booking %d: cancelled event traceparent %q, want the expire span %q", i, got, want)
		}
	}
}

// A booking with no stored trace (created before tracing, or by an untraced
// caller) still expires, and its span starts a trace of its own.
func TestExpirerWithoutStoredTraceStartsItsOwnTrace(t *testing.T) {
	resetDB(t)
	rec := expirerRecorder(t)
	id := createHeldBooking(t, testUnitID, 1)
	makeOld(t, id, 20*time.Minute)

	n, err := newTestExpirer(50).RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if n != 1 {
		t.Fatalf("expired: got %d, want 1", n)
	}

	span := expireSpanFor(t, rec, id)
	if span.Parent().IsValid() {
		t.Errorf("parent %s: want none, a root span", span.Parent().SpanID())
	}
	if got, want := cancelledTraceparent(t, id), traceparentOf(span.SpanContext()); got != want {
		t.Errorf("cancelled event traceparent %q, want the expire span %q", got, want)
	}
}

// If the transaction rolls back after the spans started, the spans say so:
// error status and the recorded error. The booking stays pending, with no
// event, so no span may look like a successful expiry.
func TestExpirerRollbackMarksSpansAsErrors(t *testing.T) {
	resetDB(t)
	rec := expirerRecorder(t)
	id := createHeldBooking(t, testUnitID, 1)
	parent := storeBookingTrace(t, id)
	makeOld(t, id, 20*time.Minute)
	// Releasing the seat would now exceed total_units: the CHECK constraint
	// fails the release, after the spans have started.
	execSQL(t, `UPDATE inventory_units SET available_units = total_units WHERE unit_id = $1`, testUnitID)

	if _, err := newTestExpirer(50).RunOnce(context.Background()); err == nil {
		t.Fatal("run: want an error from the seat release, got nil")
	}

	span := expireSpanFor(t, rec, id)
	if span.SpanContext().TraceID() != parent.TraceID() {
		t.Errorf("trace id %s, want the booking's trace %s", span.SpanContext().TraceID(), parent.TraceID())
	}
	if span.Status().Code != codes.Error {
		t.Errorf("status %v, want error", span.Status().Code)
	}
	var recorded bool
	for _, e := range span.Events() {
		if e.Name == "exception" {
			recorded = true
		}
	}
	if !recorded {
		t.Error("want the error recorded on the span")
	}
	if s := bookingStatus(t, id); s != StatusPending {
		t.Errorf("status %q, want %q: the expiry rolled back", s, StatusPending)
	}
	if n := expiredEvents(t, id); n != 0 {
		t.Errorf("cancelled events: got %d, want 0", n)
	}
}
