package relay

import (
	"context"
	"strconv"

	"github.com/nicholaswijaya004/loka/internal/storage"
	"github.com/nicholaswijaya004/loka/internal/telemetry"
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
	return p.client.ProduceSync(ctx, newRecord(ctx, p.topic, e)).FirstErr()
}

// newRecord builds the Kafka record for an outbox event. Its headers carry
// the event's id and type, plus the trace of the span in ctx (the relay's
// publish span), so the consumer's span becomes that span's child. An event
// without a trace simply gets no trace headers.
func newRecord(ctx context.Context, topic string, e storage.Outbox) *kgo.Record {
	traceHeaders := telemetry.Inject(ctx)

	headers := make([]kgo.RecordHeader, 0, 2+len(traceHeaders))
	headers = append(headers,
		kgo.RecordHeader{Key: "event_id", Value: []byte(strconv.FormatInt(e.ID, 10))},
		kgo.RecordHeader{Key: "event_type", Value: []byte(e.EventType)},
	)
	for k, v := range traceHeaders {
		headers = append(headers, kgo.RecordHeader{Key: k, Value: []byte(v)})
	}

	return &kgo.Record{
		Topic:   topic,
		Key:     []byte(e.AggregateID.String()),
		Value:   e.Payload,
		Headers: headers,
	}
}
