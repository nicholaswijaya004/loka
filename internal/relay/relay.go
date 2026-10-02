package relay

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/nicholaswijaya004/loka/internal/storage"
	"github.com/nicholaswijaya004/loka/internal/telemetry"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

var tracer = otel.Tracer("github.com/nicholaswijaya004/loka/internal/relay")

type Publisher interface {
	Publish(ctx context.Context, e storage.Outbox) error
}

type Relay struct {
	store        *storage.Store
	publisher    Publisher
	batchSize    int
	interval     time.Duration
	logger       *slog.Logger
	afterPublish func()
}

func New(store *storage.Store, publisher Publisher, batchSize int, interval time.Duration, logger *slog.Logger) *Relay {
	return &Relay{store: store, publisher: publisher, batchSize: batchSize, interval: interval, logger: logger}
}

func (r *Relay) RunOnce(ctx context.Context) (int, error) {
	published := 0

	err := r.store.WithTx(ctx, func(tx *storage.Store) error {
		events, err := tx.FetchUnpublishedOutboxEvents(ctx, r.batchSize)
		if err != nil {
			return err
		}

		sent := make([]int64, 0, len(events))
		for _, e := range events {
			// Continue the booking's trace from the outbox row; the span is the
			// parent of the consumer's, via the Kafka headers Publish writes.
			pctx, span := tracer.Start(telemetry.Extract(ctx, e.TraceContext), "publish "+e.EventType,
				trace.WithSpanKind(trace.SpanKindProducer),
				trace.WithAttributes(
					attribute.Int64("outbox.event_id", e.ID),
					attribute.String("outbox.event_type", e.EventType),
					attribute.String("booking.id", e.AggregateID.String()),
				),
			)
			err := r.publisher.Publish(pctx, e)
			if err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, "publish failed")
			}
			span.End()

			if err != nil {
				r.logger.Warn("publish failed", "event_id", e.ID, "error", err)
				if err := tx.RecordOutboxFailure(ctx, e.ID, err.Error()); err != nil {
					return err
				}
				break
			}
			sent = append(sent, e.ID)
		}

		if r.afterPublish != nil && len(sent) > 0 {
			r.afterPublish()
		}

		if err := tx.MarkOutboxEventsPublished(ctx, sent); err != nil {
			return err
		}
		published = len(sent)
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("relay batch: %w", err)
	}
	return published, nil
}

func (r *Relay) Run(ctx context.Context) error {
	for {
		n, err := r.RunOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			r.logger.Error("relay batch failed", "error", err)
		}

		if err == nil && n == r.batchSize {
			continue
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(r.interval):
		}
	}
}

func (r *Relay) SetAfterPublish(fn func()) {
	r.afterPublish = fn
}
