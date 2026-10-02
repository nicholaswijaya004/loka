package relay

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

func headerMap(t *testing.T, ctx context.Context, e storage.Outbox) map[string]string {
	t.Helper()
	got := map[string]string{}
	for _, h := range newRecord(ctx, "booking-events", e).Headers {
		if _, dup := got[h.Key]; dup {
			t.Errorf("header %q set twice", h.Key)
		}
		got[h.Key] = string(h.Value)
	}
	return got
}

// The record carries the span in ctx (the relay's publish span) as its
// traceparent, so the consumer's span becomes that span's child.
func TestNewRecordCarriesTheSpanInContext(t *testing.T) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	provider := sdktrace.NewTracerProvider()
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	ctx, span := provider.Tracer("test").Start(context.Background(), "publish booking.created")
	defer span.End()

	e := storage.Outbox{ID: 42, AggregateID: uuid.New(), EventType: "booking.created", Payload: []byte(`{}`)}
	got := headerMap(t, ctx, e)

	sc := span.SpanContext()
	want := map[string]string{
		"event_id":    "42",
		"event_type":  "booking.created",
		"traceparent": fmt.Sprintf("00-%s-%s-01", sc.TraceID(), sc.SpanID()),
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("header %s: got %q, want %q", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("headers: got %v, want exactly %v", got, want)
	}
}

// An event published outside any trace gets only its own headers.
func TestNewRecordWithoutTraceHasNoTraceHeaders(t *testing.T) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	e := storage.Outbox{ID: 7, AggregateID: uuid.New(), EventType: "booking.created", Payload: []byte(`{}`)}

	got := headerMap(t, context.Background(), e)
	if len(got) != 2 || got["event_id"] != "7" || got["event_type"] != "booking.created" {
		t.Errorf("headers: got %v, want only event_id and event_type", got)
	}
}

// Key and value are unchanged by the tracing work: the key keeps one booking's
// events on one partition, in order.
func TestNewRecordKeyAndValue(t *testing.T) {
	e := storage.Outbox{ID: 1, AggregateID: uuid.New(), EventType: "booking.created", Payload: []byte(`{"a":1}`)}
	r := newRecord(context.Background(), "booking-events", e)
	if r.Topic != "booking-events" || string(r.Key) != e.AggregateID.String() || string(r.Value) != `{"a":1}` {
		t.Errorf("record: topic %q key %q value %q", r.Topic, r.Key, r.Value)
	}
}
