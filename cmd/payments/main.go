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

	"github.com/nicholaswijaya004/loka/internal/booking"
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
	BackoffBase:   5 * time.Second,
	MaxBackoff:    5 * time.Minute,
}

// The expirer's deadline comes from BOOKING_EXPIRE_AFTER; the chaos test sets
// it short so bookings actually expire during a run.
var expirerCfg = booking.ExpirerConfig{
	BatchSize: 50,
	Interval:  5 * time.Second,
}

// The breaker opens after 5 consecutive provider failures, stays open for
// 30s (longer than one charge timeout, so a probe never overlaps a call still
// in flight), then lets a single probe through.
var breakerCfg = payments.BreakerConfig{
	TripAfter:   5,
	OpenTimeout: 30 * time.Second,
	MaxRequests: 1,
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

	expireAfter, err := time.ParseDuration(envOr("BOOKING_EXPIRE_AFTER", "15m"))
	if err != nil || expireAfter <= 0 {
		return fmt.Errorf("BOOKING_EXPIRE_AFTER: want a positive duration like 15m, got %q", os.Getenv("BOOKING_EXPIRE_AFTER"))
	}
	expirerCfg.After = expireAfter

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return fmt.Errorf("parse dsn: %w", err)
	}
	// One connection per parallel charge's outcome, plus the consumer and claims.
	cfg.MaxConns = int32(workerCfg.BatchSize) + 5

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

	// Decorator stack: the breaker wraps the HTTP client. One breaker, shared
	// by every charge the worker makes, so it sees all the calls.
	provider := payments.NewBreakerProvider(
		payments.NewHTTPProvider(providerURL, chargeTimeout),
		breakerCfg, logger,
	)
	worker := payments.NewWorker(store, provider, logger, workerCfg)
	expirer := booking.NewExpirer(store, logger, expirerCfg)

	logger.Info("payments started",
		"topic", topic, "group", payments.ConsumerName, "provider", providerURL,
		"batch_size", workerCfg.BatchSize, "lease", workerCfg.Lease, "charge_timeout", chargeTimeout,
		"backoff_base", workerCfg.BackoffBase, "max_backoff", workerCfg.MaxBackoff,
		"escalate_after", workerCfg.EscalateAfter,
		"breaker_trip_after", breakerCfg.TripAfter, "breaker_open_timeout", breakerCfg.OpenTimeout, "expire_after", expirerCfg.After, "expire_batch_size", expirerCfg.BatchSize,
		"expire_interval", expirerCfg.Interval)

	// Run all three. If any stops, cancel the others, then wait for them too.
	runners := []func() error{
		func() error { return sagaStarter.Run(ctx, client) },
		func() error { return worker.Run(ctx) },
		func() error { return expirer.Run(ctx) },
	}
	errs := make(chan error, len(runners))
	for _, run := range runners {
		go func() { errs <- run() }()
	}

	results := []error{<-errs}
	stop()
	for range len(runners) - 1 {
		results = append(results, <-errs)
	}

	logger.Info("payments stopped")
	return errors.Join(results...)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
