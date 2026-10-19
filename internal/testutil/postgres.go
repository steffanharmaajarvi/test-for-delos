// Package testutil gives each test its own empty PostgreSQL database.
//
// By default one container per test binary is started with testcontainers.
// Set TEST_DATABASE_URL to an existing server (any database on it; the user
// must be allowed to CREATE DATABASE) to skip Docker.
package testutil

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"delos/internal/db"
)

var (
	adminURL string
	dbSeq    atomic.Int64
)

// Main starts PostgreSQL, runs the tests and stops it. Call it from TestMain.
func Main(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	if adminURL = os.Getenv("TEST_DATABASE_URL"); adminURL != "" {
		return m.Run()
	}
	ctx := context.Background()
	c, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("postgres"),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("postgres"),
		testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).WithStartupTimeout(60*time.Second)),
	)
	if err != nil {
		log.Printf("start postgres container: %v (set TEST_DATABASE_URL to use an existing server)", err)
		return 1
	}
	defer func() { _ = testcontainers.TerminateContainer(c) }()
	if adminURL, err = c.ConnectionString(ctx, "sslmode=disable"); err != nil {
		log.Printf("connection string: %v", err)
		return 1
	}
	return m.Run()
}

// NewDB creates a fresh, migrated database for the test and drops it afterwards.
func NewDB(t testing.TB) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	name := fmt.Sprintf("test_%d_%d", os.Getpid(), dbSeq.Add(1))

	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}

	cfg, err := pgxpool.ParseConfig(adminURL)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	cfg.ConnConfig.Database = name
	cfg.MaxConns = 20
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		if c, err := pgx.Connect(context.Background(), adminURL); err == nil {
			_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			_ = c.Close(context.Background())
		}
	})
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}
