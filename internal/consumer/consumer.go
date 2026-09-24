package consumer

import (
	"context"

	"github.com/nicholaswijaya004/loka/internal/storage"
)

type Event struct {
	ID      int64
	Type    string
	Key     string
	Payload []byte
}

type Handler interface {
	Handle(ctx context.Context, tx *storage.Store, e Event) error
}
