// Command kafkademo is a learning tool for exploring Kafka with franz-go.
// It is not part of the Loka service.
//
// It produces keyed demo events to booking-events and consumes them as the
// consumer group demo-go, committing offsets manually.
//
// Usage:
//
//	go run ./cmd/kafkademo produce
//	go run ./cmd/kafkademo consume
//	CRASH_BEFORE_COMMIT=1 go run ./cmd/kafkademo consume   # exit before committing

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"

	"github.com/twmb/franz-go/pkg/kgo"
)

const (
	topic   = "booking-events"
	groupID = "demo-go"
)

var seeds = []string{"localhost:9092"}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		return errors.New("usage: kafkademo produce|consume")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	switch os.Args[1] {
	case "produce":
		return produce(ctx)
	case "consume":
		return consume(ctx)
	default:
		return fmt.Errorf("unknown mode %q", os.Args[1])
	}
}

func produce(ctx context.Context) error {
	cl, err := kgo.NewClient(kgo.SeedBrokers(seeds...))
	if err != nil {
		return fmt.Errorf("create producer: %w", err)
	}
	defer cl.Close()

	for i := 1; i <= 5; i++ {
		key := fmt.Sprintf("booking-%d", i)
		record := &kgo.Record{
			Topic: topic,
			Key:   []byte(key),
			Value: fmt.Appendf(nil, `{"booking_id":"%s"}`, key),
		}

		if err := cl.ProduceSync(ctx, record).FirstErr(); err != nil {
			return fmt.Errorf("produce %s: %w", key, err)
		}
		fmt.Printf("key=%s partition=%d offset=%d\n", key, record.Partition, record.Offset)
	}
	return nil
}

func consume(ctx context.Context) error {
	cl, err := kgo.NewClient(
		kgo.SeedBrokers(seeds...),
		kgo.ConsumerGroup(groupID),
		kgo.ConsumeTopics(topic),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
		kgo.DisableAutoCommit(),
		kgo.OnPartitionsAssigned(func(_ context.Context, _ *kgo.Client, a map[string][]int32) {
			fmt.Printf("assigned: %v\n", a)
		}),
		kgo.OnPartitionsRevoked(func(_ context.Context, _ *kgo.Client, r map[string][]int32) {
			fmt.Printf("revoked: %v\n", r)
		}),
	)
	if err != nil {
		return fmt.Errorf("create consumer: %w", err)
	}
	defer cl.Close()

	fmt.Printf("consuming %s as group %s (Ctrl-C to stop)\n", topic, groupID)

	for {
		fetches := cl.PollFetches(ctx)
		if ctx.Err() != nil {
			return nil
		}

		fetches.EachError(func(t string, p int32, err error) {
			fmt.Fprintf(os.Stderr, "fetch error topic=%s partition=%d: %v\n", t, p, err)
		})

		fetches.EachRecord(func(r *kgo.Record) {
			fmt.Printf("key=%s partition=%d offset=%d value=%s\n",
				r.Key, r.Partition, r.Offset, r.Value)
		})

		if os.Getenv("CRASH_BEFORE_COMMIT") == "1" {
			fmt.Println("simulating crash before commit")
			os.Exit(1)
		}

		if err := cl.CommitUncommittedOffsets(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "commit: %v\n", err)
		}
	}
}
