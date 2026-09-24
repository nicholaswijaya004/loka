package consumer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/nicholaswijaya004/loka/internal/storage"
)

var ErrPoisonMessage = errors.New("poison message")

type Consumer struct {
	name    string // permanent: it's the key in processed_events
	store   *storage.Store
	handler Handler
	logger  *slog.Logger
}

func New(name string, store *storage.Store, handler Handler, logger *slog.Logger) *Consumer {
	return &Consumer{name: name, store: store, handler: handler, logger: logger}
}

type Handler interface {
	Handle(ctx context.Context, tx *storage.Store, e Event) error
}

func (c *Consumer) Process(ctx context.Context, e Event) (isNew bool, err error) {
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
