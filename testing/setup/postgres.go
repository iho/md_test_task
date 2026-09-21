// Package setup contains testcontainers-based fixtures shared by the
// integration tests in testing/integration. Nothing here is imported by
// application code - it is test-only infrastructure.
package setup

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	orderpostgres "ordersvc/internal/repository/postgres"
)

// Postgres starts a disposable Postgres container with the order schema
// applied and returns a ready *sql.DB. Cleanup is registered on tb, so
// callers just do: db := setup.Postgres(t).
func Postgres(tb testing.TB) *sql.DB {
	tb.Helper()
	ctx := context.Background()

	container, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("orders"),
		tcpostgres.WithUsername("orders"),
		tcpostgres.WithPassword("orders"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(30*time.Second),
		),
	)
	if err != nil {
		tb.Fatalf("start postgres container: %v", err)
	}
	tb.Cleanup(func() {
		if err := container.Terminate(context.Background()); err != nil {
			tb.Logf("terminate postgres container: %v", err)
		}
	})

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		tb.Fatalf("get connection string: %v", err)
	}

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		tb.Fatalf("open db: %v", err)
	}
	tb.Cleanup(func() {
		if err := db.Close(); err != nil {
			tb.Errorf("close postgres connection: %v", err)
		}
	})

	if err := waitForPing(db, 30*time.Second); err != nil {
		tb.Fatalf("ping db: %v", err)
	}
	if err := orderpostgres.Migrate(ctx, db); err != nil {
		tb.Fatalf("apply schema: %v", err)
	}
	return db
}

func waitForPing(db *sql.DB, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var err error
	for time.Now().Before(deadline) {
		if err = db.Ping(); err == nil {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("db not reachable: %w", err)
}
