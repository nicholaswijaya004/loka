package consumer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/nicholaswijaya004/loka/internal/storage"
	"github.com/nicholaswijaya004/loka/internal/telemetry"
)

var tracer = otel.Tracer("github.com/nicholaswijaya004/loka/internal/consumer")

var ErrPoisonMessage = errors.New("poison message")

type Consumer struct {
	name       string // permanent: it's the key in processed_events
	store      *storage.Store
	handler    Handler
	logger     *slog.Logger
	afterBatch func()
}

func New(name string, store *storage.Store, handler Handler, logger *slog.Logger) *Consumer {
	return &Consumer{name: name, store: store, handler: handler, logger: logger}
}

type Handler interface {
	Handle(ctx context.Context, tx *storage.Store, e Event) error
}

// Process handles one event in one transaction: claim it, then hand it to the
// handler. Each call is one consumer span, continuing the trace in the
// event's headers (the relay's publish span), so a retried event shows each
// attempt, and a redelivered one shows as a duplicate.
func (c *Consumer) Process(ctx context.Context, e Event) (isNew bool, err error) {
	ctx, span := tracer.Start(telemetry.Extract(ctx, e.Headers), "process "+e.Type,
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			attribute.Int64("outbox.event_id", e.ID),
			attribute.String("outbox.event_type", e.Type),
			attribute.String("consumer", c.name),
			attribute.String("booking.id", e.Key),
		),
	)
	defer func() {
		switch {
		case err != nil:
			span.RecordError(err)
			span.SetStatus(codes.Error, "process failed")
		case !isNew:
			span.SetAttributes(attribute.Bool("duplicate", true))
		}
		span.End()
	}()

	err = c.store.WithTx(ctx, func(tx *storage.Store) error {
		claimed, err := tx.ClaimEvent(ctx, c.name, e.ID)
		if err != nil {
			return err
		}
		if !claimed {
			return nil
		}
		if err := c.handler.Handle(ctx, tx, e); err != nil {
			return fmt.Errorf("handle event %d (%s): %w", e.ID, e.Type, err)
		}
		isNew = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return isNew, nil
}

func (c *Consumer) SetAfterBatch(fn func()) { c.afterBatch = fn }
