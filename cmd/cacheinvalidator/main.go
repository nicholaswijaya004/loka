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
	"github.com/redis/go-redis/v9"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/nicholaswijaya004/loka/internal/consumer"
	"github.com/nicholaswijaya004/loka/internal/storage"
	"github.com/nicholaswijaya004/loka/internal/unitcache"
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
	redisAddr := envOr("REDIS_ADDR", "localhost:6379")

	// Postgres only for the framework's processed_events claim; events are
	// handled one at a time, so two connections are plenty.
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return fmt.Errorf("parse dsn: %w", err)
	}
	cfg.MaxConns = 2

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

	// Default timeouts: unlike the API, nobody waits on this process, and a
	// failed DEL is retried by the framework anyway.
	rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
	defer func() { _ = rdb.Close() }()
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		// Not fatal: Run retries each event until Redis is back, without
		// committing its offset, so no invalidation is lost.
		logger.Warn("redis unreachable at startup", "addr", redisAddr, "error", err)
	}

	client, err := kgo.NewClient(
		kgo.SeedBrokers(brokers...),
		kgo.ConsumerGroup(unitcache.ConsumerName),
		kgo.ConsumeTopics(topic),
		// A brand-new group starts at the newest message: a cache entry lives
		// one TTL at most, so changes from before the first deploy have
		// nothing left to invalidate.
		kgo.ConsumeResetOffset(kgo.NewOffset().AtEnd()),
		// Offsets are committed by Run, only after each event is handled.
		kgo.DisableAutoCommit(),
		// Hold rebalances until Run has finished and committed each batch.
		kgo.BlockRebalanceOnPoll(),
	)
	if err != nil {
		return fmt.Errorf("kafka client: %w", err)
	}
	defer client.CloseAllowingRebalance()

	// The invalidator only deletes, so the TTL is never used here.
	inv := unitcache.NewInvalidator(unitcache.New(rdb, 0), logger)
	c := consumer.New(unitcache.ConsumerName, storage.NewStore(pool), inv, logger)

	logger.Info("cache invalidator started", "topic", topic, "group", unitcache.ConsumerName, "redis", redisAddr)
	err = c.Run(ctx, client)
	logger.Info("cache invalidator stopped")
	return err
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
