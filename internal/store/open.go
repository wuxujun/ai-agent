package store

import (
	"errors"
	"fmt"
	"strings"
)

// Open constructs the configured Store backend. The factory deliberately
// accepts only the backend kind and DSN so callers cannot accidentally pass
// server-only configuration or expose credentials in errors.
func Open(kind, dsn string) (Store, error) {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "memory":
		return NewMemoryStore(), nil
	case "postgres":
		if strings.TrimSpace(dsn) == "" {
			return nil, errors.New("store dsn is required")
		}
		st, err := NewPostgresStore(dsn)
		if err != nil {
			return nil, errors.New("initialize postgres store")
		}
		return st, nil
	case "redis":
		if strings.TrimSpace(dsn) == "" {
			return nil, errors.New("store dsn is required")
		}
		st, err := NewRedisStoreFromURL(dsn)
		if err != nil {
			return nil, errors.New("initialize redis store")
		}
		return st, nil
	case "sqlite", "":
		if strings.TrimSpace(dsn) == "" {
			dsn = "data/agent.db"
		}
		st, err := NewSQLiteStore(dsn)
		if err != nil {
			return nil, fmt.Errorf("initialize sqlite store: %w", sanitizeStoreError(err))
		}
		return st, nil
	default:
		// Preserve the server's historical behavior: unknown or omitted store
		// types use SQLite, while keeping the error category bounded.
		if strings.TrimSpace(dsn) == "" {
			dsn = "data/agent.db"
		}
		st, err := NewSQLiteStore(dsn)
		if err != nil {
			return nil, fmt.Errorf("initialize sqlite store: %w", sanitizeStoreError(err))
		}
		return st, nil
	}
}

func sanitizeStoreError(err error) error {
	if err == nil {
		return nil
	}
	return errors.New("store initialization failed")
}
