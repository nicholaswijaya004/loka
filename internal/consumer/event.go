package consumer

import (
	"fmt"
	"strconv"

	"github.com/twmb/franz-go/pkg/kgo"
)

type Event struct {
	ID      int64
	Type    string
	Key     string
	Payload []byte
}

func header(r *kgo.Record, key string) (string, bool) {
	for _, h := range r.Headers {
		if h.Key == key {
			return string(h.Value), true
		}
	}
	return "", false
}

func parseEvent(r *kgo.Record) (Event, error) {
	rawID, ok := header(r, "event_id")
	if !ok {
		return Event{}, fmt.Errorf("%w: event_id header missing (partition %d, offset %d)",
			ErrPoisonMessage, r.Partition, r.Offset)
	}

	id, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil {
		return Event{}, fmt.Errorf("%w: event_id %q is not a number (partition %d, offset %d)",
			ErrPoisonMessage, rawID, r.Partition, r.Offset)
	}

	eventType, ok := header(r, "event_type")
	if !ok || eventType == "" {
		return Event{}, fmt.Errorf("%w: event_type header missing (partition %d, offset %d)",
			ErrPoisonMessage, r.Partition, r.Offset)
	}

	return Event{
		ID:      id,
		Type:    eventType,
		Key:     string(r.Key),
		Payload: r.Value,
	}, nil
}
