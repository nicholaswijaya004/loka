//go:build integration

package relay

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/nicholaswijaya004/loka/internal/storage"
	"github.com/nicholaswijaya004/loka/internal/telemetry"
	"github.com/nicholaswijaya004/loka/internal/testdb"
)

// The relay's tracer is a package variable, bound to the global provider the
// first time one is set. So the tests set it once and share one recorder,
// telling their spans apart by trace ID.
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

// apiSpan stands in for the POST /bookings span whose trace the API stored.
func apiSpan(t *testing.T) trace.SpanContext {
	t.Helper()
	_, span := otel.Tracer("test").Start(context.Background(), "POST /bookings")
	span.End()
	return span.SpanContext()
}

func insertTracedEvent(t *testing.T, parent trace.SpanContext) int64 {
	t.Helper()
	e := &storage.Outbox{
		AggregateType: "booking",
		AggregateID:   uuid.New(),
		EventType:     "booking.created",
		Payload:       []byte(`{"test":true}`),
		TraceContext:  telemetry.Inject(trace.ContextWithSpanContext(context.Background(), parent)),
	}
	if err := storage.NewStore(testPool).InsertOutboxEvent(context.Background(), e); err != nil {
		t.Fatalf("insert event: %v", err)
	}
	return e.ID
}

// ctxPublisher records the context each event was published with, and fails
// on chosen ids.
type ctxPublisher struct {
	mu     sync.Mutex
	failOn map[int64]bool
	ctxs   map[int64]context.Context
}

func (p *ctxPublisher) Publish(ctx context.Context, e storage.Outbox) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ctxs[e.ID] = ctx
	if p.failOn[e.ID] {
		return errInjectedPublish
	}
	return nil
}

// publishSpan returns the one ended span of the given trace, failing if the
// relay made none or several.
func publishSpan(t *testing.T, traceID trace.TraceID) sdktrace.ReadOnlySpan {
	t.Helper()
	var found []sdktrace.ReadOnlySpan
	for _, s := range spanRecorder().Ended() {
		if s.SpanContext().TraceID() == traceID && s.Name() == "publish booking.created" {
			found = append(found, s)
		}
	}
	if len(found) != 1 {
		t.Fatalf("publish spans in trace %s: got %d, want 1", traceID, len(found))
	}
	return found[0]
}

// The relay continues the booking's trace from the outbox row: one producer
// span per event, a child of the stored API span, and the context handed to
// the publisher carries that span (so its Kafka headers point at it).
func TestRunOnceContinuesTheStoredTrace(t *testing.T) {
	testdb.Reset(t, testPool)
	spanRecorder()
	parent := apiSpan(t)
	id := insertTracedEvent(t, parent)

	pub := &ctxPublisher{ctxs: map[int64]context.Context{}}
	if _, err := New(storage.NewStore(testPool), pub, 10, 0, testLogger).RunOnce(context.Background()); err != nil {
		t.Fatalf("run once: %v", err)
	}

	span := publishSpan(t, parent.TraceID())
	if span.Parent().SpanID() != parent.SpanID() {
		t.Errorf("parent: got %s, want the API span %s", span.Parent().SpanID(), parent.SpanID())
	}
	if span.SpanKind() != trace.SpanKindProducer {
		t.Errorf("kind: got %s, want producer", span.SpanKind())
	}
	if span.Status().Code == codes.Error {
		t.Errorf("status: got error on a successful publish")
	}
	if got := trace.SpanContextFromContext(pub.ctxs[id]); got.SpanID() != span.SpanContext().SpanID() {
		t.Errorf("publisher got span %s, want the publish span %s", got.SpanID(), span.SpanContext().SpanID())
	}
	attrs := map[string]string{}
	for _, a := range span.Attributes() {
		attrs[string(a.Key)] = a.Value.String()
	}
	if attrs["outbox.event_type"] != "booking.created" || attrs["outbox.event_id"] == "" || attrs["booking.id"] == "" {
		t.Errorf("attributes: got %v", attrs)
	}
}

// A failed publish shows as an error span in the booking's trace, and the
// event stays unpublished for the next poll.
func TestRunOnceMarksFailedPublishSpanAsError(t *testing.T) {
	testdb.Reset(t, testPool)
	spanRecorder()
	parent := apiSpan(t)
	id := insertTracedEvent(t, parent)

	pub := &ctxPublisher{ctxs: map[int64]context.Context{}, failOn: map[int64]bool{id: true}}
	if _, err := New(storage.NewStore(testPool), pub, 10, 0, testLogger).RunOnce(context.Background()); err != nil {
		t.Fatalf("run once: %v", err)
	}

	span := publishSpan(t, parent.TraceID())
	if span.Status().Code != codes.Error {
		t.Errorf("status: got %s, want error", span.Status().Code)
	}
	if len(span.Events()) == 0 {
		t.Error("want the error recorded on the span")
	}
	if stateOf(t, id).published {
		t.Error("a failed event must stay unpublished")
	}
}

// An event with no stored trace still publishes, in a trace of its own: the
// relay never fails or skips an event over tracing.
func TestRunOnceWithoutStoredTraceStillPublishes(t *testing.T) {
	testdb.Reset(t, testPool)
	spanRecorder()
	ids := insertEvents(t, 1)

	pub := &ctxPublisher{ctxs: map[int64]context.Context{}}
	n, err := New(storage.NewStore(testPool), pub, 10, 0, testLogger).RunOnce(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("run once: published %d, err %v; want 1", n, err)
	}
	if sc := trace.SpanContextFromContext(pub.ctxs[ids[0]]); !sc.IsValid() {
		t.Error("publisher got no span: want a new root span for an untraced event")
	}
}
