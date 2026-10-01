package store

import (
	"context"
	"database/sql"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/config"
)

// database/sql owns the pool independently of the driver. SQLite keeps this
// contention contract hermetic; PostgreSQL integration uses the same setup.
func TestPostgresPoolBoundsAndQueueing(t *testing.T) {
	for _, settings := range []config.PostgresPoolConfig{{}, {MaxOpenConns: 3, MaxIdleConns: 2, ConnMaxLifetimeMinutes: 1}} {
		db, err := sql.Open("sqlite", ":memory:")
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if err := configurePostgresPool(db, settings); err != nil {
			t.Fatal(err)
		}
		pool, _ := settings.Normalized()
		checkPostgresPoolContention(t, db, pool)
	}
}

func TestPostgresPoolExternal(t *testing.T) {
	requireExternalIntegration(t)
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not set")
	}
	st, err := NewPostgresStore(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	pool, err := config.Get().Store.Postgres.Normalized()
	if err != nil {
		t.Fatal(err)
	}
	checkPostgresPoolContention(t, st.db, pool)
}

func checkPostgresPoolContention(t *testing.T, db *sql.DB, pool config.PostgresPoolConfig) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	held := make(chan struct{}, max(100, pool.MaxOpenConns*2))
	release := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < max(100, pool.MaxOpenConns*2); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := db.Conn(ctx)
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.Close()
			var value int
			if err := conn.QueryRowContext(ctx, "SELECT 1").Scan(&value); err != nil {
				t.Error(err)
				return
			}
			held <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
			}
		}()
	}
	for i := 0; i < pool.MaxOpenConns; i++ {
		select {
		case <-held:
		case <-ctx.Done():
			t.Fatal("pool did not fill")
		}
	}
	stats := db.Stats()
	if stats.MaxOpenConnections != pool.MaxOpenConns || stats.OpenConnections != pool.MaxOpenConns {
		t.Errorf("pool stats=%+v", stats)
	}
	select {
	case <-held:
		t.Error("pool exceeded cap")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	wg.Wait()
	stats = db.Stats()
	if stats.InUse != 0 || stats.Idle > pool.MaxIdleConns || stats.WaitCount == 0 {
		t.Fatalf("pool did not drain/queue: %+v", stats)
	}
}
