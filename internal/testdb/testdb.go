//go:build integration

// Package testdb provides a throwaway Postgres for integration tests.
// Each test package calls Start once from TestMain and Reset at the top of each test.
package testdb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

const (
	dbname   = "booking_test"
	username = "test"
	password = "test"
)

// Start launches Postgres, applies the migrations, and returns a pool.
// The returned cleanup closes the pool and removes the container; call it
// even when Start returns an error.
func Start(ctx context.Context) (pool *pgxpool.Pool, cleanup func(), err error) {
	var container *tcpostgres.PostgresContainer
	cleanup = func() {
		if pool != nil {
			pool.Close()
		}
		if termErr := testcontainers.TerminateContainer(container); termErr != nil {
			fmt.Fprintf(os.Stderr, "terminate container: %v\n", termErr)
		}
	}

	container, err = tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase(dbname),
		tcpostgres.WithUsername(username),
		tcpostgres.WithPassword(password),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		return nil, cleanup, fmt.Errorf("start container: %w", err)
	}

	connStr, err := container.ConnectionString(ctx, "sslmode=disable", "application_name=test")
	if err != nil {
		return nil, cleanup, fmt.Errorf("connection string: %w", err)
	}

	if err := applyMigrations(connStr); err != nil {
		return nil, cleanup, fmt.Errorf("migrate: %w", err)
	}

	pool, err = pgxpool.New(ctx, connStr)
	if err != nil {
		return nil, cleanup, fmt.Errorf("pool: %w", err)
	}
	return pool, cleanup, nil
}

// Reset empties every table and re-applies the seed. Tests that call it
// must not run in parallel.
func Reset(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()

	_, err := pool.Exec(ctx, `
		TRUNCATE outbox_events, processed_events, idempotency_keys, payments,
		         bookings, inventory_units, customers
		RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}

	seed, err := os.ReadFile(repoPath("scripts", "seed.sql"))
	if err != nil {
		t.Fatalf("read seed: %v", err)
	}
	if _, err := pool.Exec(ctx, string(seed)); err != nil {
		t.Fatalf("apply seed: %v", err)
	}
}

func applyMigrations(connStr string) (err error) {
	m, err := migrate.New("file://"+repoPath("migrations"), connStr)
	if err != nil {
		return err
	}
	defer func() {
		srcErr, dbErr := m.Close()
		err = errors.Join(err, srcErr, dbErr)
	}()

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return err
	}
	return nil
}

// repoPath resolves a path from the repo root. This file lives two levels
// down (internal/testdb), so the root is ../.. from here.
func repoPath(parts ...string) string {
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(file), "..", "..")
	return filepath.Join(append([]string{root}, parts...)...)
}
