package telemetry

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// startSpan starts a real, recorded span, as a handler or worker would have.
func startSpan(t *testing.T) (context.Context, trace.Span) {
	t.Helper()
	otel.SetTextMapPropagator(propagation.TraceContext{})
	provider := sdktrace.NewTracerProvider()
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	return provider.Tracer("test").Start(context.Background(), "parent")
}

// A span injected into headers and extracted again is the same trace: this is
// what every async hop (outbox row, Kafka header, booking row) relies on.
func TestInjectExtractKeepsTheTrace(t *testing.T) {
	ctx, span := startSpan(t)
	defer span.End()

	headers := Inject(ctx)
	if headers["traceparent"] == "" {
		t.Fatalf("headers: got %v, want a traceparent", headers)
	}

	got := trace.SpanContextFromContext(Extract(context.Background(), headers))
	want := span.SpanContext()
	if !got.IsValid() {
		t.Fatal("extracted context carries no trace")
	}
	if got.TraceID() != want.TraceID() {
		t.Errorf("trace ID: got %s, want %s", got.TraceID(), want.TraceID())
	}
	// The extracted span is the remote parent: the span that was injected.
	if got.SpanID() != want.SpanID() {
		t.Errorf("parent span ID: got %s, want %s", got.SpanID(), want.SpanID())
	}
	if !got.IsRemote() {
		t.Error("extracted span context should be marked remote: it came from another process")
	}
	if !got.IsSampled() {
		t.Error("sampled flag lost: the next process would drop the trace")
	}
}

// A span started from the extracted context joins the trace as a child of the
// injected span, which is how the relay, consumer and worker spans attach.
func TestSpanFromExtractedContextIsAChild(t *testing.T) {
	ctx, parent := startSpan(t)
	defer parent.End()

	remote := Extract(context.Background(), Inject(ctx))
	provider := sdktrace.NewTracerProvider()
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	_, child := provider.Tracer("test").Start(remote, "child")
	defer child.End()

	if child.SpanContext().TraceID() != parent.SpanContext().TraceID() {
		t.Errorf("child trace ID: got %s, want %s",
			child.SpanContext().TraceID(), parent.SpanContext().TraceID())
	}
	if child.SpanContext().SpanID() == parent.SpanContext().SpanID() {
		t.Error("child reused the parent's span ID; it should get its own")
	}
	if ro, ok := child.(sdktrace.ReadOnlySpan); ok && ro.Parent().SpanID() != parent.SpanContext().SpanID() {
		t.Errorf("child's parent: got %s, want %s", ro.Parent().SpanID(), parent.SpanContext().SpanID())
	}
}

// With no span, Inject returns nil, so the column stores NULL, not {}.
func TestInjectWithoutSpanIsNil(t *testing.T) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	if headers := Inject(context.Background()); headers != nil {
		t.Errorf("headers: got %v, want nil", headers)
	}
}

// Extracting nothing, or garbage, leaves ctx as it was: no trace appears from
// nowhere, and a bad row never breaks the hop that reads it.
func TestExtractWithoutTraceLeavesContext(t *testing.T) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	for name, headers := range map[string]map[string]string{
		"nil":     nil,
		"empty":   {},
		"invalid": {"traceparent": "not-a-traceparent"},
	} {
		t.Run(name, func(t *testing.T) {
			got := Extract(context.Background(), headers)
			if trace.SpanContextFromContext(got).IsValid() {
				t.Errorf("got a trace from %v, want none", headers)
			}
		})
	}
}
