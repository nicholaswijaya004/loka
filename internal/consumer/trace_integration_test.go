//go:build integration

package consumer

import (
	"context"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/nicholaswijaya004/loka/internal/storage"
	"github.com/nicholaswijaya004/loka/internal/telemetry"
	"github.com/nicholaswijaya004/loka/internal/testdb"
)

// The consumer's tracer is a package variable, bound to the first global
// provider ever set. So the tests set one provider once, share its recorder,
// and tell their spans apart by trace ID.
var (
	recorderOnce sync.Once
	recorder     *tracetest.SpanRecorder
)

func spanRecorder() *tracetest.SpanRecorder {
	recorderOnce.Do(func() {
		recorder = tracetest.NewSpanRecorder()
		otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder)))
		otel.SetTextMapPropagator(propagation.TraceContext{})
	})
	return recorder
}

// tracedEvent returns testEvent carrying the headers the relay would send:
// the traceparent of a publish span, which it also returns.
func tracedEvent(t *testing.T) (Event, trace.SpanContext) {
	t.Helper()
	spanRecorder()
	ctx, publish := otel.Tracer("test").Start(context.Background(), "publish booking.created",
		trace.WithSpanKind(trace.SpanKindProducer))
	publish.End()

	e := testEvent
	e.Headers = map[string]string{"event_id": "7", "event_type": e.Type}
	for k, v := range telemetry.Inject(ctx) {
		e.Headers[k] = v
	}
	return e, publish.SpanContext()
}

// processSpans returns the ended process spans of one trace, in order.
func processSpans(traceID trace.TraceID) []sdktrace.ReadOnlySpan {
	var found []sdktrace.ReadOnlySpan
	for _, s := range spanRecorder().Ended() {
		if s.SpanContext().TraceID() == traceID && s.Name() == "process booking.created" {
			found = append(found, s)
		}
	}
	return found
}

func attr(s sdktrace.ReadOnlySpan, key string) (attribute.Value, bool) {
	for _, a := range s.Attributes() {
		if string(a.Key) == key {
			return a.Value, true
		}
	}
	return attribute.Value{}, false
}

// ctxHandler records the context it was called with.
type ctxHandler struct{ ctx context.Context }

func (h *ctxHandler) Handle(ctx context.Context, _ *storage.Store, _ Event) error {
	h.ctx = ctx
	return nil
}

// Process continues the trace from the event's headers: one consumer span, a
// child of the relay's publish span, and the handler runs inside it, so its
// work (and choice B's re-stamping of the booking) belongs to that span.
func TestProcessContinuesTheTraceFromHeaders(t *testing.T) {
	testdb.Reset(t, testPool)
	e, publish := tracedEvent(t)
	h := &ctxHandler{}

	if _, err := newTestConsumer(h).Process(context.Background(), e); err != nil {
		t.Fatalf("process: %v", err)
	}

	spans := processSpans(publish.TraceID())
	if len(spans) != 1 {
		t.Fatalf("process spans: got %d, want 1", len(spans))
	}
	span := spans[0]
	if span.Parent().SpanID() != publish.SpanID() {
		t.Errorf("parent: got %s, want the publish span %s", span.Parent().SpanID(), publish.SpanID())
	}
	if span.SpanKind() != trace.SpanKindConsumer {
		t.Errorf("kind: got %s, want consumer", span.SpanKind())
	}
	if _, dup := attr(span, "duplicate"); dup {
		t.Error("a first delivery must not be marked duplicate")
	}
	if got := trace.SpanContextFromContext(h.ctx).SpanID(); got != span.SpanContext().SpanID() {
		t.Errorf("handler ran in span %s, want the process span %s", got, span.SpanContext().SpanID())
	}
}

// A redelivered event shows in the booking's trace as a second process span,
// marked duplicate: the idempotent consumer at work, made visible.
func TestProcessMarksRedeliveryAsDuplicate(t *testing.T) {
	testdb.Reset(t, testPool)
	e, publish := tracedEvent(t)
	c := newTestConsumer(&fakeHandler{})

	for range 2 {
		if _, err := c.Process(context.Background(), e); err != nil {
			t.Fatalf("process: %v", err)
		}
	}

	spans := processSpans(publish.TraceID())
	if len(spans) != 2 {
		t.Fatalf("process spans: got %d, want 2", len(spans))
	}
	if v, ok := attr(spans[1], "duplicate"); !ok || !v.AsBool() {
		t.Error("second delivery: want duplicate=true")
	}
}

// A failed attempt is an error span; the retry that follows is its own span,
// so the trace shows every attempt.
func TestProcessFailureIsAnErrorSpan(t *testing.T) {
	testdb.Reset(t, testPool)
	e, publish := tracedEvent(t)

	if _, err := newTestConsumer(&fakeHandler{fail: true}).Process(context.Background(), e); err == nil {
		t.Fatal("process: got nil, want the handler's error")
	}
	if _, err := newTestConsumer(&fakeHandler{}).Process(context.Background(), e); err != nil {
		t.Fatalf("retry: %v", err)
	}

	spans := processSpans(publish.TraceID())
	if len(spans) != 2 {
		t.Fatalf("process spans: got %d, want 2 (the failure, then the retry)", len(spans))
	}
	if spans[0].Status().Code != codes.Error {
		t.Errorf("first attempt: got status %s, want error", spans[0].Status().Code)
	}
	if spans[1].Status().Code == codes.Error {
		t.Error("retry: want no error")
	}
}
