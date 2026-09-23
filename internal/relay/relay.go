package relay

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/nicholaswijaya004/loka/internal/storage"
)

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
			if err := r.publisher.Publish(ctx, e); err != nil {
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
