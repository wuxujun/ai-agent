package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/wuxujun/ai-agent/internal/types"
)

type approvalSQLBackend struct {
	db       *sql.DB
	postgres bool
}

func (b approvalSQLBackend) bind(query string) string {
	if !b.postgres {
		return query
	}
	var builder strings.Builder
	argument := 1
	for _, char := range query {
		if char == '?' {
			fmt.Fprintf(&builder, "$%d", argument)
			argument++
		} else {
			builder.WriteRune(char)
		}
	}
	return builder.String()
}

func (b approvalSQLBackend) create(ctx context.Context, approval *types.DurableApproval) error {
	record := types.CloneDurableApproval(approval)
	if err := record.Validate(); err != nil {
		return err
	}
	now := time.Now().UTC()
	if record.CreatedAt.IsZero() {
		record.CreatedAt = now
	}
	record.UpdatedAt = now
	if record.Version == 0 {
		record.Version = 1
	}
	requestJSON, err := json.Marshal(record.Request)
	if err != nil {
		return fmt.Errorf("marshal approval request: %w", err)
	}
	var resolvedAt any
	if !record.ResolvedAt.IsZero() {
		resolvedAt = record.ResolvedAt
	}
	_, err = b.db.ExecContext(ctx, b.bind(`INSERT INTO approvals
(id, task_id, tenant_id, request_json, action_payload, resolution_payload, status, version, owner, lease_expires_at, created_at, updated_at, resolved_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`), record.ID, record.TaskID, record.TenantID, requestJSON,
		record.ActionPayload, nullableBytes(record.ResolutionPayload), record.Status, record.Version, record.Owner,
		timeToMillis(record.LeaseExpiresAt), record.CreatedAt, record.UpdatedAt, resolvedAt)
	return err
}

const approvalSelectColumns = `id, task_id, tenant_id, request_json, action_payload, resolution_payload,
status, version, owner, lease_expires_at, created_at, updated_at, resolved_at`

type rowScanner interface{ Scan(...any) error }

func scanDurableApproval(row rowScanner) (*types.DurableApproval, error) {
	var approval types.DurableApproval
	var requestJSON []byte
	var resolutionPayload []byte
	var leaseMillis int64
	var resolvedAt sql.NullTime
	if err := row.Scan(&approval.ID, &approval.TaskID, &approval.TenantID, &requestJSON, &approval.ActionPayload,
		&resolutionPayload, &approval.Status, &approval.Version, &approval.Owner, &leaseMillis,
		&approval.CreatedAt, &approval.UpdatedAt, &resolvedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(requestJSON, &approval.Request); err != nil {
		return nil, fmt.Errorf("decode approval request: %w", err)
	}
	approval.ResolutionPayload = append([]byte(nil), resolutionPayload...)
	if leaseMillis > 0 {
		approval.LeaseExpiresAt = time.UnixMilli(leaseMillis).UTC()
	}
	if resolvedAt.Valid {
		approval.ResolvedAt = resolvedAt.Time
	}
	return &approval, nil
}

func (b approvalSQLBackend) get(ctx context.Context, id, tenantID string) (*types.DurableApproval, error) {
	return scanDurableApproval(b.db.QueryRowContext(ctx, b.bind(`SELECT `+approvalSelectColumns+` FROM approvals WHERE id = ? AND tenant_id = ?`), id, tenantID))
}

func (b approvalSQLBackend) list(ctx context.Context, taskID, tenantID string, status types.DurableApprovalStatus) ([]*types.DurableApproval, error) {
	query := `SELECT ` + approvalSelectColumns + ` FROM approvals WHERE task_id = ? AND tenant_id = ?`
	args := []any{taskID, tenantID}
	if status != "" {
		query += ` AND status = ?`
		args = append(args, status)
	}
	query += ` ORDER BY created_at, id`
	rows, err := b.db.QueryContext(ctx, b.bind(query), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]*types.DurableApproval, 0)
	for rows.Next() {
		approval, scanErr := scanDurableApproval(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, approval)
	}
	return result, rows.Err()
}

func (b approvalSQLBackend) transition(ctx context.Context, id, tenantID string, expectedVersion int64, from, to types.DurableApprovalStatus, payload []byte) (bool, error) {
	if !types.CanTransitionApproval(from, to) {
		return false, fmt.Errorf("invalid approval transition %s -> %s", from, to)
	}
	now := time.Now().UTC()
	clearLease := to == types.ApprovalConsumed || to == types.ApprovalRejected || to == types.ApprovalExpired
	ownerExpr, leaseExpr := "owner", "lease_expires_at"
	if clearLease {
		ownerExpr, leaseExpr = "''", "0"
	}
	query := fmt.Sprintf(`UPDATE approvals SET status = ?, version = version + 1, resolution_payload = ?,
updated_at = ?, resolved_at = ?, owner = %s, lease_expires_at = %s
WHERE id = ? AND tenant_id = ? AND version = ? AND status = ?`, ownerExpr, leaseExpr)
	result, err := b.db.ExecContext(ctx, b.bind(query), to, nullableBytes(payload), now, now, id, tenantID, expectedVersion, from)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

func (b approvalSQLBackend) acquireLease(ctx context.Context, id, owner string, ttl time.Duration) (bool, error) {
	if owner == "" || ttl <= 0 {
		return false, nil
	}
	now := time.Now().UTC()
	result, err := b.db.ExecContext(ctx, b.bind(`UPDATE approvals SET owner = ?, lease_expires_at = ?
WHERE id = ? AND (owner = '' OR owner = ? OR lease_expires_at <= ?)`), owner, now.Add(ttl).UnixMilli(), id, owner, now.UnixMilli())
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

func (b approvalSQLBackend) releaseLease(ctx context.Context, id, owner string) error {
	_, err := b.db.ExecContext(ctx, b.bind(`UPDATE approvals SET owner = '', lease_expires_at = 0 WHERE id = ? AND owner = ?`), id, owner)
	return err
}

func (b approvalSQLBackend) deleteTerminalBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	result, err := b.db.ExecContext(ctx, b.bind(`DELETE FROM approvals
WHERE status IN (?, ?) AND resolved_at IS NOT NULL AND resolved_at < ?`), types.ApprovalConsumed, types.ApprovalExpired, cutoff.UTC())
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func nullableBytes(value []byte) any {
	if len(value) == 0 {
		return nil
	}
	return value
}

func timeToMillis(value time.Time) int64 {
	if value.IsZero() {
		return 0
	}
	return value.UnixMilli()
}

func (s *SQLiteStore) approvalBackend() approvalSQLBackend {
	return approvalSQLBackend{db: s.db}
}

func (p *PostgresStore) approvalBackend() approvalSQLBackend {
	return approvalSQLBackend{db: p.db, postgres: true}
}

func (s *SQLiteStore) CreateApproval(ctx context.Context, value *types.DurableApproval) error {
	return s.approvalBackend().create(ctx, value)
}
func (p *PostgresStore) CreateApproval(ctx context.Context, value *types.DurableApproval) error {
	return p.approvalBackend().create(ctx, value)
}
func (s *SQLiteStore) GetApproval(ctx context.Context, id, tenant string) (*types.DurableApproval, error) {
	return s.approvalBackend().get(ctx, id, tenant)
}
func (p *PostgresStore) GetApproval(ctx context.Context, id, tenant string) (*types.DurableApproval, error) {
	return p.approvalBackend().get(ctx, id, tenant)
}
func (s *SQLiteStore) ListTaskApprovals(ctx context.Context, task, tenant string, status types.DurableApprovalStatus) ([]*types.DurableApproval, error) {
	return s.approvalBackend().list(ctx, task, tenant, status)
}
func (p *PostgresStore) ListTaskApprovals(ctx context.Context, task, tenant string, status types.DurableApprovalStatus) ([]*types.DurableApproval, error) {
	return p.approvalBackend().list(ctx, task, tenant, status)
}
func (s *SQLiteStore) TransitionApproval(ctx context.Context, id, tenant string, version int64, from, to types.DurableApprovalStatus, payload []byte) (bool, error) {
	return s.approvalBackend().transition(ctx, id, tenant, version, from, to, payload)
}
func (p *PostgresStore) TransitionApproval(ctx context.Context, id, tenant string, version int64, from, to types.DurableApprovalStatus, payload []byte) (bool, error) {
	return p.approvalBackend().transition(ctx, id, tenant, version, from, to, payload)
}
func (s *SQLiteStore) AcquireApprovalLease(ctx context.Context, id, owner string, ttl time.Duration) (bool, error) {
	return s.approvalBackend().acquireLease(ctx, id, owner, ttl)
}
func (p *PostgresStore) AcquireApprovalLease(ctx context.Context, id, owner string, ttl time.Duration) (bool, error) {
	return p.approvalBackend().acquireLease(ctx, id, owner, ttl)
}
func (s *SQLiteStore) ReleaseApprovalLease(ctx context.Context, id, owner string) error {
	return s.approvalBackend().releaseLease(ctx, id, owner)
}
func (p *PostgresStore) ReleaseApprovalLease(ctx context.Context, id, owner string) error {
	return p.approvalBackend().releaseLease(ctx, id, owner)
}
func (s *SQLiteStore) DeleteTerminalApprovalsBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	return s.approvalBackend().deleteTerminalBefore(ctx, cutoff)
}
func (p *PostgresStore) DeleteTerminalApprovalsBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	return p.approvalBackend().deleteTerminalBefore(ctx, cutoff)
}
