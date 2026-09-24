//go:build integration

package consumer

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nicholaswijaya004/loka/internal/testdb"
)

var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	pool, cleanup, err := testdb.Start(context.Background())
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration setup: %v\n", err)
		cleanup()
		os.Exit(1)
	}
	testPool = pool

	code := m.Run()
	cleanup()
	os.Exit(code)
}
