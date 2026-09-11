package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// lockTaskLeaseSQL takes a write lock before reading the lease. A no-op UPDATE
// works both for SQLite's transaction write lock and PostgreSQL's row lock.
// Time must be checked after acquiring the lock, since waiting may outlive a lease.
func lockTaskLeaseSQL(ctx context.Context, tx *sql.Tx, id string, postgres bool) (string, int64, error) {
	placeholder := "?"
	if postgres {
		placeholder = "$1"
	}
	if _, err := tx.ExecContext(ctx, `UPDATE task_leases SET owner = owner WHERE task_id = `+placeholder, id); err != nil {
		return "", 0, err
	}
	var owner string
	var expiresAt int64
	err := tx.QueryRowContext(ctx, `SELECT owner, expires_at FROM task_leases WHERE task_id = `+placeholder, id).Scan(&owner, &expiresAt)
	return owner, expiresAt, err
}

func guardTaskLeaseSQL(ctx context.Context, tx *sql.Tx, id string, postgres bool) error {
	lease, scoped := ctx.Value(taskLeaseContextKey{}).(taskLeaseScope)
	if !scoped {
		return nil
	}
	if lease.id != id || lease.owner == "" {
		return ErrTaskLeaseLost
	}
	owner, expiresAt, err := lockTaskLeaseSQL(ctx, tx, id, postgres)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrTaskLeaseLost
	}
	if err != nil {
		return err
	}
	if owner != lease.owner || expiresAt <= time.Now().UnixNano() {
		return ErrTaskLeaseLost
	}
	return nil
}

func renewTaskLeaseSQL(ctx context.Context, db *sql.DB, id, owner string, ttl time.Duration, postgres bool) (bool, error) {
	if owner == "" || ttl <= 0 {
		return false, nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback() //nolint:errcheck
	currentOwner, expiresAt, err := lockTaskLeaseSQL(ctx, tx, id, postgres)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	now := time.Now()
	if currentOwner != owner || expiresAt <= now.UnixNano() {
		return false, nil
	}
	query := `UPDATE task_leases SET expires_at = ? WHERE task_id = ?`
	if postgres {
		query = `UPDATE task_leases SET expires_at = $1 WHERE task_id = $2`
	}
	if _, err := tx.ExecContext(ctx, query, now.Add(ttl).UnixNano(), id); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
