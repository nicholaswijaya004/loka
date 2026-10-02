package telemetry

import (
	"context"
	"fmt"
	"log/slog"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Setup sends this process's spans to the OTLP endpoint (Jaeger), named
// after service, and sets the W3C traceparent propagator that carries a
// trace between processes. Call shutdown before exiting, or spans still in
// the buffer are lost.
//
// The endpoint comes from OTEL_EXPORTER_OTLP_ENDPOINT (default
// localhost:4317). Spans are exported in the background: if the collector is
// down they are dropped, and the process carries on.
func Setup(ctx context.Context, service string, logger *slog.Logger) (shutdown func(context.Context) error, err error) {
	exporter, err := otlptracegrpc.New(ctx, otlptracegrpc.WithInsecure())
	if err != nil {
		return nil, fmt.Errorf("otlp exporter: %w", err)
	}
	res, err := resource.New(ctx, resource.WithAttributes(attribute.String("service.name", service)))
	if err != nil {
		return nil, fmt.Errorf("resource: %w", err)
	}
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		// Follow the incoming trace's decision; new traces are always recorded.
		// Production would use TraceIDRatioBased(0.01) here, or tail sampling.
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample())),
	)
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		logger.Error("opentelemetry", "error", err)
	}))
	return provider.Shutdown, nil
}

// Inject returns the propagation headers (traceparent, and any others the
// propagator writes) of the span in ctx, for storing in a row or sending in
// message headers, or nil if ctx carries no span.
func Inject(ctx context.Context) map[string]string {
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	if len(carrier) == 0 {
		return nil
	}
	return carrier
}

// Extract returns ctx with the trace in headers as its parent, so the next
// span started from it joins that trace. Empty or invalid headers leave ctx
// unchanged.
func Extract(ctx context.Context, headers map[string]string) context.Context {
	if len(headers) == 0 {
		return ctx
	}
	return otel.GetTextMapPropagator().Extract(ctx, propagation.MapCarrier(headers))
}
