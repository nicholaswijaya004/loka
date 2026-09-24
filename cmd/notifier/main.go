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

	"github.com/nicholaswijaya004/loka/internal/consumer"
	"github.com/nicholaswijaya004/loka/internal/notifier"
	"github.com/nicholaswijaya004/loka/internal/storage"
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
	cfg.MaxConns = 4

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
		kgo.ConsumerGroup(notifier.ConsumerName),
		kgo.ConsumeTopics(topic),
		// A brand-new group starts from the oldest message, so bookings made
		// before the notifier was first deployed still get a confirmation.
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
		// Offsets are committed by Run, only after the database commit.
		kgo.DisableAutoCommit(),
		// Hold rebalances until Run has finished and committed each batch.
		kgo.BlockRebalanceOnPoll(),
	)
	if err != nil {
		return fmt.Errorf("kafka client: %w", err)
	}
	defer client.Close()

	c := consumer.New(notifier.ConsumerName, storage.NewStore(pool), notifier.New(logger), logger)

	if os.Getenv("CRASH_AFTER_DB_COMMIT") == "1" {
		c.SetAfterBatch(func() {
			logger.Warn("simulating crash after db commit, before offset commit")
			os.Exit(1)
		})
	}

	logger.Info("notifier started", "topic", topic, "group", notifier.ConsumerName)
	err = c.Run(ctx, client)
	logger.Info("notifier stopped")
	return err
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
