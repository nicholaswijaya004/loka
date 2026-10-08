package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"

	"github.com/nicholaswijaya004/loka/internal/api"
	"github.com/nicholaswijaya004/loka/internal/booking"
	"github.com/nicholaswijaya004/loka/internal/storage"
	"github.com/nicholaswijaya004/loka/internal/telemetry"
	"github.com/nicholaswijaya004/loka/internal/unitcache"
)

type storeAdapter struct {
	*storage.Store
}

// poolStats is a pool's counters since it started. loadrun.sh reads them
// just before and after the measured run and takes the difference.
type poolStats struct {
	MaxConns             int32   `json:"max_conns"`
	AcquireCount         int64   `json:"acquire_count"`
	EmptyAcquireCount    int64   `json:"empty_acquire_count"`
	EmptyAcquireWaitMs   float64 `json:"empty_acquire_wait_ms"`
	CanceledAcquireCount int64   `json:"canceled_acquire_count"`
	NewConnsCount        int64   `json:"new_conns_count"`
}

func (a storeAdapter) WithTx(ctx context.Context, fn func(booking.Store) error) error {
	return a.Store.WithTx(ctx, func(tx *storage.Store) error {
		return fn(storeAdapter{tx})
	})
}

func (a storeAdapter) WithSerializableTx(ctx context.Context, fn func(booking.Store) error) error {
	return a.Store.WithSerializableTx(ctx, func(tx *storage.Store) error {
		return fn(storeAdapter{tx})
	})
}

func statsOf(p *pgxpool.Pool) poolStats {
	s := p.Stat()
	return poolStats{
		MaxConns:             s.MaxConns(),
		AcquireCount:         s.AcquireCount(),
		EmptyAcquireCount:    s.EmptyAcquireCount(),
		EmptyAcquireWaitMs:   s.EmptyAcquireWaitTime().Seconds() * 1000,
		CanceledAcquireCount: s.CanceledAcquireCount(),
		NewConnsCount:        s.NewConnsCount(),
	}
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	shutdownTracing, err := telemetry.Setup(ctx, "api", logger)
	if err != nil {
		logger.Error("telemetry setup failed", "error", err)
		os.Exit(1)
	}

	defer func() {
		flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownTracing(flushCtx); err != nil {
			logger.Error("telemetry shutdown failed", "error", err)
		}
	}()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://loka:loka@localhost:5432/loka?sslmode=disable"
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		logger.Error("bad dsn", "error", err)
		os.Exit(1)
	}
	maxConns, err := strconv.Atoi(envOr("API_DB_MAX_CONNS", "50"))
	if err != nil || maxConns < 1 {
		logger.Error("bad API_DB_MAX_CONNS", "value", os.Getenv("API_DB_MAX_CONNS"), "error", err)
		os.Exit(1)
	}
	cfg.MaxConns = int32(maxConns)
	cfg.ConnConfig.Tracer = otelpgx.NewTracer()
	cfg.ConnConfig.RuntimeParams["application_name"] = "loka-api"

	// GET /bookings/{id} reads through its own small pool, so it never queues
	// behind POSTs that hold every write connection during a stall. Same
	// primary, so a GET right after a 201 still finds the booking.
	readCfg := cfg.Copy()
	readCfg.MaxConns = 10
	readCfg.ConnConfig.RuntimeParams["application_name"] = "loka-api-read"

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		logger.Error("db pool init failed", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	readPool, err := pgxpool.NewWithConfig(ctx, readCfg)
	if err != nil {
		logger.Error("db read pool init failed", "error", err)
		os.Exit(1)
	}
	defer readPool.Close()

	// Pool stats as metrics, read when Prometheus scrapes (at most once a
	// second). Both pools point at the same host:port/db, which is otelpgx's
	// default pool name, so name them or their series collide.
	for name, p := range map[string]*pgxpool.Pool{"write": pool, "read": readPool} {
		if err := otelpgx.RecordStats(p, otelpgx.WithStatsAttributes(
			attribute.String("db.client.connection.pool.name", name),
		)); err != nil {
			logger.Error("pool metrics setup failed", "pool", name, "error", err)
			os.Exit(1)
		}
	}

	pingCtx, cancelPing := context.WithTimeout(ctx, 5*time.Second)
	defer cancelPing()
	if err := pool.Ping(pingCtx); err != nil {
		logger.Error("db unreachable", "error", err)
		os.Exit(1)
	}

	if err := readPool.Ping(pingCtx); err != nil {
		logger.Error("db unreachable (read pool)", "error", err)
		os.Exit(1)
	}

	store := storage.NewStore(pool)
	unsafe := os.Getenv("UNSAFE_DECREMENT") == "1"
	strategy := os.Getenv("STRATEGY")
	if strategy == "" {
		strategy = "single"
	}
	switch strategy {
	case "single", "forupdate", "optimistic", "serializable":
		// valid
	default:
		logger.Error("unrecognized STRATEGY", "strategy", strategy)
		os.Exit(1)
	}
	logger.Info("booking strategy", "strategy", strategy)
	logger.Info("runtime",
		"gomaxprocs", runtime.GOMAXPROCS(0),
		"write_pool_max_conns", cfg.MaxConns,
		"read_pool_max_conns", readCfg.MaxConns,
	)

	opts := []booking.Option{booking.WithReader(storage.NewStore(readPool))}

	// GET /units/{id} reads through Redis when UNIT_CACHE_TTL > 0. 0 turns the
	// cache off, so one binary serves both sides of a before/after run.
	ttl, err := time.ParseDuration(envOr("UNIT_CACHE_TTL", "30s"))
	if err != nil || ttl < 0 {
		logger.Error("bad UNIT_CACHE_TTL", "value", os.Getenv("UNIT_CACHE_TTL"), "error", err)
		os.Exit(1)
	}
	if ttl > 0 {
		redisAddr := envOr("REDIS_ADDR", "localhost:6379")
		// Above Redis's normal round trip, far below a request's budget. Too
		// short, and a slow-but-healthy Redis turns into fallbacks.
		timeout, err := time.ParseDuration(envOr("UNIT_CACHE_TIMEOUT", "50ms"))
		if err != nil || timeout <= 0 {
			logger.Error("bad UNIT_CACHE_TIMEOUT", "value", os.Getenv("UNIT_CACHE_TIMEOUT"), "error", err)
			os.Exit(1)
		}
		rdb := redis.NewClient(&redis.Options{
			Addr: redisAddr,
			// A cache answers fast or not at all: on a timeout GetUnit falls
			// back to Postgres instead of waiting. No retries either, at both
			// layers: MaxRetries for commands, DialerRetries for connecting
			// (its default, 5 dials 100 ms apart, made every read wait
			// ~400 ms while Redis was down).
			DialTimeout:   200 * time.Millisecond,
			ReadTimeout:   timeout,
			WriteTimeout:  timeout,
			MaxRetries:    -1,
			DialerRetries: 1,
		})
		defer func() {
			if err := rdb.Close(); err != nil {
				logger.Error("redis close failed", "error", err)
			}
		}()

		// Redis down at startup is not fatal: GetUnit falls back to Postgres,
		// and the client reconnects on its own once Redis is back.
		if err := rdb.Ping(pingCtx).Err(); err != nil {
			logger.Warn("redis unreachable at startup, unit reads go to postgres until it is back",
				"addr", redisAddr, "error", err)
		}
		opts = append(opts, booking.WithUnitCache(unitcache.New(rdb, ttl)))
		logger.Info("unit cache on", "addr", redisAddr, "ttl", ttl.String(), "timeout", timeout.String())
	} else {
		logger.Info("unit cache off")
	}

	svc := booking.NewService(storeAdapter{store}, logger, unsafe, strategy, opts...)

	if unsafe {
		logger.Warn("running with UNSAFE_DECREMENT — demonstration mode only")
	}
	h := api.NewHandler(svc, logger)

	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte(`{"status":"ok"}`)); err != nil {
			logger.Error("write failed", "path", r.URL.Path, "error", err)
		}
	})

	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		rctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		w.Header().Set("Content-Type", "application/json")
		if err := pool.Ping(rctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			if _, err := w.Write([]byte(`{"status":"db unavailable"}`)); err != nil {
				logger.Error("write failed", "path", r.URL.Path, "error", err)
			}
			return
		}
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte(`{"status":"ready"}`)); err != nil {
			logger.Error("write failed", "path", r.URL.Path, "error", err)
		}
	})

	// Not wrapped in otelhttp: Prometheus's own scrapes would show up as
	// traffic on the dashboards they feed.
	mux.Handle("GET /metrics", telemetry.MetricsHandler())

	mux.HandleFunc("GET /debug/pool", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]poolStats{
			"write": statsOf(pool),
			"read":  statsOf(readPool),
		}); err != nil {
			logger.Error("write failed", "path", r.URL.Path, "error", err)
		}
	})

	mux.Handle("POST /bookings", otelhttp.NewHandler(http.HandlerFunc(h.CreateBooking), "POST /bookings"))
	mux.Handle("GET /bookings/{id}", otelhttp.NewHandler(http.HandlerFunc(h.GetBooking), "GET /bookings/{id}"))
	mux.Handle("GET /units/{id}", otelhttp.NewHandler(http.HandlerFunc(h.GetUnit), "GET /units/{id}"))

	srv := &http.Server{
		Addr:         ":8080",
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("starting server", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	select {
	case err := <-serverErr:
		logger.Error("server failed", "error", err)
		os.Exit(1)
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	}

	stop()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed, forcing close", "error", err)
		if err := srv.Close(); err != nil {
			logger.Error("force close failed", "error", err)
		}
		os.Exit(1)
	}

	logger.Info("retries", "strategy", strategy, "total", svc.Retries())

	logger.Info("shutdown complete")
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
