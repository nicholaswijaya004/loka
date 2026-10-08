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
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/trace"
)

var tracer = otel.Tracer("github.com/nicholaswijaya004/loka/internal/relay")

// Message is one outbox event and the context of its publish span; its Kafka
// headers point at that span.
type Message struct {
	Ctx   context.Context
	Event storage.Outbox
}

// Publisher sends a batch at once. It returns one error per message, in the
// same order as msgs; nil means Kafka acknowledged that message.
type Publisher interface {
	Publish(ctx context.Context, msgs []Message) []error
}

type Relay struct {
	store         *storage.Store
	publisher     Publisher
	batchSize     int
	interval      time.Duration
	logger        *slog.Logger
	afterPublish  func()
	meterProvider metric.MeterProvider
	metrics       *metrics
}

func New(store *storage.Store, publisher Publisher, batchSize int, interval time.Duration, logger *slog.Logger, opts ...Option) *Relay {
	r := &Relay{store: store, publisher: publisher, batchSize: batchSize, interval: interval, logger: logger,
		meterProvider: otel.GetMeterProvider()}
	for _, opt := range opts {
		opt(r)
	}
	m, err := newMetrics(r.meterProvider)
	if err != nil {
		// Publishing matters more than counting it: carry on unmeasured.
		logger.Error("relay metrics setup failed, running without metrics", "error", err)
		m, _ = newMetrics(noop.NewMeterProvider())
	}
	r.metrics = m
	return r
}

func (r *Relay) RunOnce(ctx context.Context) (int, error) {
	start := time.Now()
	published, fetched := 0, 0

	err := r.store.WithTx(ctx, func(tx *storage.Store) error {
		events, err := tx.FetchUnpublishedOutboxEvents(ctx, r.batchSize)
		if err != nil {
			return err
		}
		if len(events) == 0 {
			return nil
		}
		fetched = len(events)

		msgs := make([]Message, len(events))
		spans := make([]trace.Span, len(events))
		for i, e := range events {
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
			msgs[i], spans[i] = Message{Ctx: pctx, Event: e}, span
		}

		defer func() {
			for _, span := range spans {
				span.End()
			}
		}()

		errs := r.publisher.Publish(ctx, msgs)
		if len(errs) != len(msgs) {
			return fmt.Errorf("publisher returned %d errors for %d messages", len(errs), len(msgs))
		}

		sent := make([]int64, 0, len(events))
		for i, err := range errs {
			if err != nil {
				spans[i].RecordError(err)
				spans[i].SetStatus(codes.Error, "publish failed")
				r.logger.Warn("failed to publish outbox event", "event_id", events[i].ID, "error", err)
				r.metrics.failed.Add(ctx, 1)
				if err := tx.RecordOutboxFailure(ctx, events[i].ID, err.Error()); err != nil {
					return err
				}
				continue
			}
			sent = append(sent, events[i].ID)
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
	// Empty polls are left out: two a second at about a millisecond each,
	// they would drag every percentile down to the cost of doing nothing.
	if fetched > 0 {
		r.metrics.batch.Record(ctx, time.Since(start).Seconds())
	}
	if err != nil {
		return 0, fmt.Errorf("relay batch: %w", err)
	}
	// Counted only now that the batch has committed: a rolled-back batch is
	// fetched and published again, and would otherwise be counted twice.
	r.metrics.published.Add(ctx, int64(published))
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
