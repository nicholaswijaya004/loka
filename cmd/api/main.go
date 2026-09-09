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

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nicholaswijaya004/loka/internal/api"
	"github.com/nicholaswijaya004/loka/internal/booking"
	"github.com/nicholaswijaya004/loka/internal/storage"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

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

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		logger.Error("db pool init failed", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	pingCtx, cancelPing := context.WithTimeout(ctx, 5*time.Second)
	defer cancelPing()
	if err := pool.Ping(pingCtx); err != nil {
		logger.Error("db unreachable", "error", err)
		os.Exit(1)
	}

	store := storage.NewStore(pool)
	unsafe := os.Getenv("UNSAFE_DECREMENT") == "1"
	svc := booking.NewService(store, unsafe)
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

	mux.HandleFunc("POST /bookings", h.CreateBooking)
	mux.HandleFunc("GET /bookings/{id}", h.GetBooking)

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

	logger.Info("shutdown complete")
}
