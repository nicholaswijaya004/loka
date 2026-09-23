// Command relay publishes outbox events from Postgres to Kafka.
//
// Configuration (environment, with local defaults):
//
//	DATABASE_URL   postgres://loka:loka@localhost:5432/loka?sslmode=disable
//	KAFKA_BROKERS  localhost:9092 (comma-separated)
//	KAFKA_TOPIC    booking-events
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/nicholaswijaya004/loka/internal/relay"
	"github.com/nicholaswijaya004/loka/internal/storage"
)

const (
	batchSize    = 100
	pollInterval = 500 * time.Millisecond
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dsn := envOr("DATABASE_URL", "postgres://loka:loka@localhost:5432/loka?sslmode=disable")
	brokers := strings.Split(envOr("KAFKA_BROKERS", "localhost:9092"), ",")
	topic := envOr("KAFKA_TOPIC", "booking-events")

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return fmt.Errorf("parse dsn: %w", err)
	}
	cfg.MaxConns = 2 // one batch transaction at a time, plus headroom

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return fmt.Errorf("db pool: %w", err)
	}
	defer pool.Close()

	pingCtx, cancelPing := context.WithTimeout(ctx, 5*time.Second)
	defer cancelPing()
	if err := pool.Ping(pingCtx); err != nil {
		return fmt.Errorf("db unreachable: %w", err)
	}

	client, err := kgo.NewClient(
		kgo.SeedBrokers(brokers...),
		// Turn a broker outage into an error instead of retrying while the
		// relay's transaction holds its row locks.
		kgo.RecordDeliveryTimeout(10*time.Second),
	)
	if err != nil {
		return fmt.Errorf("kafka client: %w", err)
	}
	defer client.Close()

	r := relay.New(
		storage.NewStore(pool),
		relay.NewKafkaPublisher(client, topic),
		batchSize, pollInterval, logger,
	)

	if os.Getenv("CRASH_AFTER_PUBLISH") == "1" {
		r.SetAfterPublish(func() {
			logger.Warn("simulating crash after publish, before marking")
			os.Exit(1)
		})
	}

	logger.Info("relay started", "topic", topic, "batch_size", batchSize, "poll_interval", pollInterval)
	err = r.Run(ctx)
	logger.Info("relay stopped")
	return err
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
