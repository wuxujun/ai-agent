package config

import (
	"fmt"
	"math"
	"time"
)

// PostgresPoolConfig is applied when a store is opened; changes require restart.
// Zero values use bounded defaults rather than database/sql's unlimited pool.
type PostgresPoolConfig struct {
	MaxOpenConns           int `mapstructure:"max_open_conns"`
	MaxIdleConns           int `mapstructure:"max_idle_conns"`
	ConnMaxLifetimeMinutes int `mapstructure:"conn_max_lifetime_minutes"`
}

func (p PostgresPoolConfig) Normalized() (PostgresPoolConfig, error) {
	if p.MaxOpenConns < 0 || p.MaxIdleConns < 0 || p.ConnMaxLifetimeMinutes < 0 {
		return p, fmt.Errorf("store.postgres pool settings must be non-negative")
	}
	if p.MaxOpenConns == 0 {
		p.MaxOpenConns = 50
	}
	if p.MaxIdleConns == 0 {
		p.MaxIdleConns = min(10, p.MaxOpenConns)
	}
	if p.ConnMaxLifetimeMinutes == 0 {
		p.ConnMaxLifetimeMinutes = 30
	}
	if p.MaxIdleConns > p.MaxOpenConns {
		return p, fmt.Errorf("store.postgres.max_idle_conns must not exceed max_open_conns")
	}
	if uint64(p.ConnMaxLifetimeMinutes) > uint64(math.MaxInt64/int64(time.Minute)) {
		return p, fmt.Errorf("store.postgres.conn_max_lifetime_minutes exceeds duration limit")
	}
	return p, nil
}
