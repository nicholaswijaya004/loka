package relay

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/nicholaswijaya004/loka/internal/storage"
)

func headerMap(t *testing.T, ctx context.Context, e storage.Outbox) map[string]string {
	t.Helper()
	got := map[string]string{}
	for _, h := range newRecord(ctx, "booking-events", e).Headers {
		if _, dup := got[h.Key]; dup {
			t.Errorf("header %q set twice", h.Key)
		}
		got[h.Key] = string(h.Value)
	}
	return got
}

// The record carries the span in ctx (the relay's publish span) as its
// traceparent, so the consumer's span becomes that span's child.
func TestNewRecordCarriesTheSpanInContext(t *testing.T) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	provider := sdktrace.NewTracerProvider()
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	ctx, span := provider.Tracer("test").Start(context.Background(), "publish booking.created")
	defer span.End()

	e := storage.Outbox{ID: 42, AggregateID: uuid.New(), EventType: "booking.created", Payload: []byte(`{}`)}
	got := headerMap(t, ctx, e)

	sc := span.SpanContext()
	want := map[string]string{
		"event_id":    "42",
		"event_type":  "booking.created",
		"traceparent": fmt.Sprintf("00-%s-%s-01", sc.TraceID(), sc.SpanID()),
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("header %s: got %q, want %q", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("headers: got %v, want exactly %v", got, want)
	}
}

// An event published outside any trace gets only its own headers.
func TestNewRecordWithoutTraceHasNoTraceHeaders(t *testing.T) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	e := storage.Outbox{ID: 7, AggregateID: uuid.New(), EventType: "booking.created", Payload: []byte(`{}`)}

	got := headerMap(t, context.Background(), e)
	if len(got) != 2 || got["event_id"] != "7" || got["event_type"] != "booking.created" {
		t.Errorf("headers: got %v, want only event_id and event_type", got)
	}
}

// Key and value are unchanged by the tracing work: the key keeps one booking's
// events on one partition, in order.
func TestNewRecordKeyAndValue(t *testing.T) {
	e := storage.Outbox{ID: 1, AggregateID: uuid.New(), EventType: "booking.created", Payload: []byte(`{"a":1}`)}
	r := newRecord(context.Background(), "booking-events", e)
	if r.Topic != "booking-events" || string(r.Key) != e.AggregateID.String() || string(r.Value) != `{"a":1}` {
		t.Errorf("record: topic %q key %q value %q", r.Topic, r.Key, r.Value)
	}
}

// newFakeKafka starts an in-process Kafka with the topic's 3 partitions.
func newFakeKafka(t *testing.T) []string {
	t.Helper()
	c, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(3, "booking-events"))
	if err != nil {
		t.Fatalf("start fake kafka: %v", err)
	}
	t.Cleanup(c.Close)
	return c.ListenAddrs()
}

// testMessages builds n events with ids 1..n, each for its own booking, so
// they spread over the partitions. Payloads of the ids in big are too large
// for a 1 KiB batch.
func testMessages(n int, big ...int64) []Message {
	tooBig := map[int64]bool{}
	for _, id := range big {
		tooBig[id] = true
	}
	msgs := make([]Message, n)
	for i := range msgs {
		id := int64(i + 1)
		payload := []byte(`{}`)
		if tooBig[id] {
			payload = bytes.Repeat([]byte("x"), 4096)
		}
		msgs[i] = Message{
			Ctx:   context.Background(),
			Event: storage.Outbox{ID: id, AggregateID: uuid.New(), EventType: "booking.created", Payload: payload},
		}
	}
	return msgs
}

// All messages of a batch reach Kafka, each with a nil result and the
// traceparent of its own publish span, not one shared by the batch.
func TestKafkaPublisherSendsTheBatch(t *testing.T) {
	addrs := newFakeKafka(t)
	producer, err := kgo.NewClient(kgo.SeedBrokers(addrs...))
	if err != nil {
		t.Fatalf("producer: %v", err)
	}
	defer producer.Close()

	otel.SetTextMapPropagator(propagation.TraceContext{})
	provider := sdktrace.NewTracerProvider()
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	msgs := testMessages(5)
	wantParent := map[string]string{} // event_id → traceparent
	for i := range msgs {
		ctx, span := provider.Tracer("test").Start(context.Background(), "publish booking.created")
		span.End()
		msgs[i].Ctx = ctx
		sc := span.SpanContext()
		wantParent[strconv.FormatInt(msgs[i].Event.ID, 10)] = fmt.Sprintf("00-%s-%s-01", sc.TraceID(), sc.SpanID())
	}
	errs := NewKafkaPublisher(producer, "booking-events").Publish(context.Background(), msgs)
	if len(errs) != len(msgs) {
		t.Fatalf("results: got %d, want one per message (%d)", len(errs), len(msgs))
	}
	for i, err := range errs {
		if err != nil {
			t.Errorf("message %d: %v", i, err)
		}
	}

	consumer, err := kgo.NewClient(kgo.SeedBrokers(addrs...),
		kgo.ConsumeTopics("booking-events"),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}
	defer consumer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	got := map[string]string{} // event_id → traceparent
	for len(got) < len(msgs) {
		fetches := consumer.PollFetches(ctx)
		if ctx.Err() != nil {
			t.Fatalf("read back: got %v after 10s, want event ids 1..%d", got, len(msgs))
		}
		fetches.EachRecord(func(r *kgo.Record) {
			h := map[string]string{}
			for _, kv := range r.Headers {
				h[kv.Key] = string(kv.Value)
			}
			got[h["event_id"]] = h["traceparent"]
		})
	}
	for id, want := range wantParent {
		if tp, ok := got[id]; !ok {
			t.Errorf("event %s never reached kafka", id)
		} else if tp != want {
			t.Errorf("event %s: traceparent %q, want its own span %q", id, tp, want)
		}
	}
}

// Each result belongs to its own message. Records too large for a batch fail
// at once, before the others are acknowledged, so results that come back in
// completion order would land on the wrong messages.
func TestKafkaPublisherReportsEachRecordsOwnError(t *testing.T) {
	addrs := newFakeKafka(t)
	producer, err := kgo.NewClient(kgo.SeedBrokers(addrs...), kgo.ProducerBatchMaxBytes(1024))
	if err != nil {
		t.Fatalf("producer: %v", err)
	}
	defer producer.Close()

	msgs := testMessages(5, 2, 4) // events 2 and 4 are too large
	errs := NewKafkaPublisher(producer, "booking-events").Publish(context.Background(), msgs)
	if len(errs) != len(msgs) {
		t.Fatalf("results: got %d, want one per message (%d)", len(errs), len(msgs))
	}
	for i, m := range msgs {
		wantFail := m.Event.ID == 2 || m.Event.ID == 4
		if wantFail && errs[i] == nil {
			t.Errorf("event %d (too large): got nil, want an error", m.Event.ID)
		}
		if !wantFail && errs[i] != nil {
			t.Errorf("event %d: got %v, want nil", m.Event.ID, errs[i])
		}
	}
}
