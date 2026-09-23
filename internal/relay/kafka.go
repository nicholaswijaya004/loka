package relay

import (
	"context"
	"strconv"

	"github.com/nicholaswijaya004/loka/internal/storage"
	"github.com/twmb/franz-go/pkg/kgo"
)

type KafkaPublisher struct {
	client *kgo.Client
	topic  string
}

func NewKafkaPublisher(client *kgo.Client, topic string) *KafkaPublisher {
	return &KafkaPublisher{client: client, topic: topic}
}

func (p *KafkaPublisher) Publish(ctx context.Context, e storage.Outbox) error {
	record := &kgo.Record{
		Topic: p.topic,
		Key:   []byte(e.AggregateID.String()),
		Value: e.Payload,
		Headers: []kgo.RecordHeader{
			{Key: "event_id", Value: []byte(strconv.FormatInt(e.ID, 10))},
			{Key: "event_type", Value: []byte(e.EventType)},
		},
	}
	return p.client.ProduceSync(ctx, record).FirstErr()
}
