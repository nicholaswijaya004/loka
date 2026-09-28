// Command payments runs the payment saga (ADR-003): a consumer that starts it
// on booking.created, and a worker that charges and records the outcome.
//
// Configuration (environment, with local defaults):
//
//	DATABASE_URL          postgres://loka:loka@localhost:5432/loka?sslmode=disable
//	KAFKA_BROKERS         localhost:9092 (comma-separated)
//	KAFKA_TOPIC           booking-events
//	PAYMENT_PROVIDER_URL  http://localhost:8081
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/nicholaswijaya004/loka/internal/consumer"
	"github.com/nicholaswijaya004/loka/internal/payments"
	"github.com/nicholaswijaya004/loka/internal/storage"
)

const (
	chargeTimeout = 10 * time.Second // must stay below lease
)

var workerCfg = payments.WorkerConfig{
	BatchSize:     10,
	Interval:      500 * time.Millisecond,
	Lease:         30 * time.Second,
	EscalateAfter: 30 * time.Minute,
}

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
	providerURL := envOr("PAYMENT_PROVIDER_URL", "http://localhost:8081")

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return fmt.Errorf("parse dsn: %w", err)
	}
	// One connection per parallel charge's outcome, plus the consumer and claims.
	cfg.MaxConns = int32(workerCfg.BatchSize) + 4

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
		kgo.ConsumerGroup(payments.ConsumerName),
		kgo.ConsumeTopics(topic),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
		kgo.DisableAutoCommit(),
		kgo.BlockRebalanceOnPoll(),
	)
	if err != nil {
		return fmt.Errorf("kafka client: %w", err)
	}
	defer client.Close()

	store := storage.NewStore(pool)
	sagaStarter := consumer.New(payments.ConsumerName, store, payments.New(logger), logger)
	worker := payments.NewWorker(store, payments.NewHTTPProvider(providerURL, chargeTimeout), logger, workerCfg)

	logger.Info("payments started",
		"topic", topic, "group", payments.ConsumerName, "provider", providerURL,
		"batch_size", workerCfg.BatchSize, "lease", workerCfg.Lease, "charge_timeout", chargeTimeout)

	// Run both. If either stops, cancel the other, then wait for it too.
	errs := make(chan error, 2)
	go func() { errs <- sagaStarter.Run(ctx, client) }()
	go func() { errs <- worker.Run(ctx) }()

	first := <-errs
	stop()
	second := <-errs

	logger.Info("payments stopped")
	return errors.Join(first, second)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
