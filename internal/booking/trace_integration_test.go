//go:build integration

package booking

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/nicholaswijaya004/loka/internal/storage"
)

// requestSpan starts a span the way otelhttp does for POST /bookings, and
// returns the traceparent it should produce.
func requestSpan(t *testing.T) (context.Context, string) {
	t.Helper()
	otel.SetTextMapPropagator(propagation.TraceContext{})
	provider := sdktrace.NewTracerProvider()
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	ctx, span := provider.Tracer("test").Start(context.Background(), "POST /bookings")
	t.Cleanup(func() { span.End() })

	sc := span.SpanContext()
	return ctx, fmt.Sprintf("00-%s-%s-01", sc.TraceID(), sc.SpanID())
}

// storedTraceparents returns the traceparent stored on the booking row and on
// its booking.created outbox row; "" where trace_context is NULL.
func storedTraceparents(t *testing.T, bookingID uuid.UUID) (booking, outbox string) {
	t.Helper()
	ctx := context.Background()
	if err := testPool.QueryRow(ctx, `
		SELECT coalesce(trace_context->>'traceparent', '') FROM bookings WHERE booking_id = $1`,
		bookingID).Scan(&booking); err != nil {
		t.Fatalf("read booking trace: %v", err)
	}
	if err := testPool.QueryRow(ctx, `
		SELECT coalesce(trace_context->>'traceparent', '') FROM outbox_events
		WHERE aggregate_id = $1 AND event_type = $2`,
		bookingID, eventBookingCreated).Scan(&outbox); err != nil {
		t.Fatalf("read outbox trace: %v", err)
	}
	return booking, outbox
}

// A booking created inside a request's span stores that span as the parent
// on both rows: the relay continues the trace from the outbox row, the worker
// and expirer from the booking row. Every strategy goes through
// insertBookingWithEvent, so every strategy must get it.
func TestCreateStoresRequestTraceOnBothRows(t *testing.T) {
	for _, strategy := range allStrategies {
		t.Run(strategy, func(t *testing.T) {
			resetDB(t)
			ctx, want := requestSpan(t)
			svc := NewService(storeAdapter{storage.NewStore(testPool)}, testLogger, false, strategy)

			b, err := svc.Create(ctx, testUnitID, testCustomerID, 1, testVisit)
			if err != nil {
				t.Fatalf("create: %v", err)
			}

			booking, outbox := storedTraceparents(t, b.BookingID)
			if booking != want {
				t.Errorf("booking traceparent: got %q, want %q", booking, want)
			}
			if outbox != want {
				t.Errorf("outbox traceparent: got %q, want %q", outbox, want)
			}
		})
	}
}

// Without a span (a script, a test, a future caller that isn't traced), both
// rows store SQL NULL: not {} and not JSON null, so "no trace" is IS NULL,
// and the booking itself is unaffected.
func TestCreateWithoutTraceStoresNull(t *testing.T) {
	resetDB(t)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	svc := NewService(storeAdapter{storage.NewStore(testPool)}, testLogger, false, "single")

	b, err := svc.Create(context.Background(), testUnitID, testCustomerID, 1, testVisit)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if n := countRows(t, `SELECT count(*) FROM bookings WHERE booking_id = $1 AND trace_context IS NULL`,
		b.BookingID); n != 1 {
		t.Errorf("booking trace_context: want SQL NULL")
	}
	if n := countRows(t, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND trace_context IS NULL`,
		b.BookingID); n != 1 {
		t.Errorf("outbox trace_context: want SQL NULL")
	}
}
