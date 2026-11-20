// Package testdb provisions an isolated, migrated PostgreSQL database per test.
// Tests using it should carry the "integration" build tag.
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wuxujun/ai-agent/wiki-agent/internal/storage/postgres"
)

// EnvVar names the admin connection string used to create throwaway databases.
const EnvVar = "TEST_DATABASE_URL"

// New creates a fresh database, applies migrations and returns a pool. The
// database is dropped when the test finishes. Skips if TEST_DATABASE_URL is unset.
func New(t testing.TB) *pgxpool.Pool {
	t.Helper()
	admin := os.Getenv(EnvVar)
	if admin == "" {
		t.Skipf("%s not set; run `make test-integration`", EnvVar)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "wiki_test_" + hex.EncodeToString(b)

	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		c, err := pgx.Connect(ctx, admin)
		if err != nil {
			t.Logf("cleanup connect: %v", err)
			return
		}
		defer c.Close(ctx)
		if _, err := c.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Logf("drop database: %v", err)
		}
	})

	u, err := url.Parse(admin)
	if err != nil {
		t.Fatalf("parse %s: %v", EnvVar, err)
	}
	u.Path = "/" + name
	dsn := u.String()

	if err := postgres.Migrate(dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := postgres.NewPool(ctx, postgres.PoolConfig{URL: dsn, MaxConns: 4, ConnectTimeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}
