package store

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/config"
)

// Explicit HA integration contract: select this test against dedicated services.
// Ordinary external integration CI may use a smaller single-instance database.
func TestPostgresPoolHAExternal(t *testing.T) {
	if os.Getenv("AI_AGENT_RUN_EXTERNAL_INTEGRATION") != "true" {
		t.Skip("set AI_AGENT_RUN_EXTERNAL_INTEGRATION=true to use dedicated external services")
	}
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Fatal("TEST_POSTGRES_DSN is required when external integration is enabled")
	}
	pool, err := config.Get().Store.Postgres.Normalized()
	if err != nil {
		t.Fatal(err)
	}
	const headroom = 10
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	var stores [2]*PostgresStore
	for i := range stores {
		st, err := NewPostgresStore(dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		stores[i] = st
	}

	var maximum, reserved int
	err = stores[0].db.QueryRowContext(ctx, `SELECT
		current_setting('max_connections')::int,
		current_setting('superuser_reserved_connections')::int +
		COALESCE(current_setting('reserved_connections', true), '0')::int`).Scan(&maximum, &reserved)
	if err != nil {
		t.Fatal(err)
	}
	required := len(stores)*pool.MaxOpenConns + headroom
	t.Logf("HA pool budget: instances=%d per_instance=%d extra_clients=%d max_connections=%d reserved=%d",
		len(stores), pool.MaxOpenConns, headroom, maximum, reserved)
	if maximum-reserved < required {
		t.Fatalf("insufficient non-reserved connections: available=%d, required=%d (two pools plus %d extra clients)", maximum-reserved, required, headroom)
	}

	extra, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	extra.SetMaxOpenConns(headroom)
	extra.SetMaxIdleConns(0)
	t.Cleanup(func() { _ = extra.Close() })

	var held []*sql.Conn
	// Release held connections before closing their pools, including fatal paths.
	t.Cleanup(func() {
		for _, conn := range held {
			_ = conn.Close()
		}
	})
	hold := func(db *sql.DB, count int) {
		t.Helper()
		for i := 0; i < count; i++ {
			conn, err := db.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			held = append(held, conn)
			var value int
			if err := conn.QueryRowContext(ctx, "SELECT 1").Scan(&value); err != nil {
				t.Fatal(err)
			}
			if value != 1 {
				t.Fatalf("connection query returned %d", value)
			}
		}
	}
	for i, st := range stores {
		hold(st.db, pool.MaxOpenConns)
		stats := st.db.Stats()
		if stats.MaxOpenConnections != pool.MaxOpenConns || stats.InUse != pool.MaxOpenConns {
			t.Fatalf("instance %d did not fill its configured pool: %+v", i, stats)
		}
	}
	hold(extra, headroom)
	for i, st := range stores {
		if stats := st.db.Stats(); stats.InUse != pool.MaxOpenConns {
			t.Fatalf("instance %d lost held connections: %+v", i, stats)
		}
	}
	if stats := extra.Stats(); stats.InUse != headroom {
		t.Fatalf("extra clients did not connect: %+v", stats)
	}
	t.Logf("simultaneously held %d live connections across two instance pools and extra clients", len(held))
	for _, conn := range held {
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
	}
	held = nil
	for i, st := range stores {
		stats := st.db.Stats()
		if stats.InUse != 0 || stats.Idle > pool.MaxIdleConns {
			t.Fatalf("instance %d did not drain its pool: %+v", i, stats)
		}
	}
}
