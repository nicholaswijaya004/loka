package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/nicholaswijaya004/loka/internal/api"
	"github.com/nicholaswijaya004/loka/internal/booking"
	"github.com/nicholaswijaya004/loka/internal/storage"
	"github.com/nicholaswijaya004/loka/internal/telemetry"
)

type storeAdapter struct {
	*storage.Store
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
	cfg.MaxConns = 50
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
	svc := booking.NewService(storeAdapter{store}, logger, unsafe, strategy,
		booking.WithReader(storage.NewStore(readPool)))

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

	mux.Handle("POST /bookings", otelhttp.NewHandler(http.HandlerFunc(h.CreateBooking), "POST /bookings"))
	mux.Handle("GET /bookings/{id}", otelhttp.NewHandler(http.HandlerFunc(h.GetBooking), "GET /bookings/{id}"))

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
