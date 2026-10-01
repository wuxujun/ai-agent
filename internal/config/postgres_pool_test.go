package config

import (
	"errors"
	"os"
	"testing"
)

func TestPostgresPoolDefaultsAndValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		pool PostgresPoolConfig
		bad  bool
	}{
		{"defaults", PostgresPoolConfig{}, false},
		{"custom", PostgresPoolConfig{MaxOpenConns: 3, MaxIdleConns: 2, ConnMaxLifetimeMinutes: 1}, false},
		{"small default idle", PostgresPoolConfig{MaxOpenConns: 3}, false},
		{"negative open", PostgresPoolConfig{MaxOpenConns: -1}, true},
		{"negative idle", PostgresPoolConfig{MaxIdleConns: -1}, true},
		{"negative lifetime", PostgresPoolConfig{ConnMaxLifetimeMinutes: -1}, true},
		{"excess idle", PostgresPoolConfig{MaxOpenConns: 3, MaxIdleConns: 4}, true},
		{"overflow lifetime", PostgresPoolConfig{ConnMaxLifetimeMinutes: 153722868}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := tc.pool.Normalized()
			if (err != nil) != tc.bad {
				t.Fatalf("pool=%+v err=%v", p, err)
			}
			if err == nil && (p.MaxOpenConns <= 0 || p.MaxIdleConns > p.MaxOpenConns || p.ConnMaxLifetimeMinutes <= 0) {
				t.Fatalf("unsafe pool: %+v", p)
			}
		})
	}
	p, _ := (PostgresPoolConfig{}).Normalized()
	if p.MaxOpenConns != 50 || p.MaxIdleConns != 10 || p.ConnMaxLifetimeMinutes != 30 {
		t.Fatalf("defaults=%+v", p)
	}
}

func TestPostgresPoolReloadPreservesSnapshot(t *testing.T) {
	path, before := loadBrainReloadFixture(t)
	for _, tc := range []struct {
		pool    string
		restart bool
	}{
		{"max_open_conns: -1", false},
		{"max_open_conns: 60", true},
		{"max_idle_conns: 5", true},
		{"conn_max_lifetime_minutes: 5", true},
	} {
		candidate := append(brainReloadConfig("./data/brain"), []byte("\nstore:\n  postgres:\n    "+tc.pool+"\n")...)
		if err := os.WriteFile(path, candidate, 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := Reload(); err == nil || (tc.restart && !errors.Is(err, ErrRestartRequired)) {
			t.Fatalf("%s: %v", tc.pool, err)
		}
		if Get() != before {
			t.Fatal("rejected reload replaced snapshot")
		}
	}
	if err := os.WriteFile(path, append(brainReloadConfig("./data/brain"), []byte("\nwiki:\n  search_top_k: 9\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Reload(); err != nil {
		t.Fatal(err)
	}
	if Get().Wiki.SearchTopK != 9 {
		t.Fatal("valid reload not applied")
	}
}
