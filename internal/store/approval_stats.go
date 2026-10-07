package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/wuxujun/ai-agent/internal/types"
)

var approvalStatStatuses = [...]types.DurableApprovalStatus{
	types.ApprovalPending, types.ApprovalApproved, types.ApprovalRejected,
	types.ApprovalExpired, types.ApprovalConsumed,
}

func (stats *ApprovalStats) setCount(status types.DurableApprovalStatus, count int64) {
	switch status {
	case types.ApprovalPending:
		stats.Pending = count
	case types.ApprovalApproved:
		stats.Approved = count
	case types.ApprovalRejected:
		stats.Rejected = count
	case types.ApprovalExpired:
		stats.Expired = count
	case types.ApprovalConsumed:
		stats.Consumed = count
	}
}

func (m *MemoryStore) GetApprovalStats(ctx context.Context, tenantID string) (ApprovalStats, error) {
	var stats ApprovalStats
	if tenantID == "" {
		return stats, fmt.Errorf("approval tenant is required")
	}
	if err := ctx.Err(); err != nil {
		return stats, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, approval := range m.approvals {
		if approval.TenantID != tenantID {
			continue
		}
		switch approval.Status {
		case types.ApprovalPending:
			stats.Pending++
			if stats.OldestPendingAt == nil || approval.CreatedAt.Before(*stats.OldestPendingAt) {
				at := approval.CreatedAt
				stats.OldestPendingAt = &at
			}
		case types.ApprovalApproved:
			stats.Approved++
		case types.ApprovalRejected:
			stats.Rejected++
		case types.ApprovalExpired:
			stats.Expired++
		case types.ApprovalConsumed:
			stats.Consumed++
		}
	}
	return stats, ctx.Err()
}

func (s *SQLiteStore) GetApprovalStats(ctx context.Context, tenantID string) (ApprovalStats, error) {
	return s.approvalBackend().stats(ctx, tenantID)
}

func (p *PostgresStore) GetApprovalStats(ctx context.Context, tenantID string) (ApprovalStats, error) {
	return p.approvalBackend().stats(ctx, tenantID)
}

func (b approvalSQLBackend) stats(ctx context.Context, tenantID string) (ApprovalStats, error) {
	var stats ApprovalStats
	if tenantID == "" {
		return stats, fmt.Errorf("approval tenant is required")
	}
	rows, err := b.db.QueryContext(ctx, b.bind(`SELECT status, COUNT(*) FROM approvals WHERE tenant_id = ? GROUP BY status`), tenantID)
	if err != nil {
		return stats, err
	}
	for rows.Next() {
		var status types.DurableApprovalStatus
		var count int64
		if err := rows.Scan(&status, &count); err != nil {
			rows.Close()
			return stats, err
		}
		stats.setCount(status, count)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return stats, err
	}
	if err := rows.Close(); err != nil {
		return stats, err
	}
	if stats.Pending > 0 {
		var at time.Time
		err := b.db.QueryRowContext(ctx, b.bind(`SELECT created_at FROM approvals WHERE tenant_id = ? AND status = ? ORDER BY created_at ASC LIMIT 1`),
			tenantID, types.ApprovalPending).Scan(&at)
		if err != nil && err != sql.ErrNoRows {
			return stats, err
		}
		if err == nil {
			at = at.UTC()
			stats.OldestPendingAt = &at
		}
	}
	return stats, nil
}

func (r *RedisStore) GetApprovalStats(ctx context.Context, tenantID string) (ApprovalStats, error) {
	var stats ApprovalStats
	if tenantID == "" {
		return stats, fmt.Errorf("approval tenant is required")
	}
	if err := r.ensureApprovalTenantIndex(ctx, tenantID); err != nil {
		return stats, err
	}
	var counts [len(approvalStatStatuses)]*redis.IntCmd
	var oldest *redis.ZSliceCmd
	_, err := r.client.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		for i, status := range approvalStatStatuses {
			counts[i] = pipe.ZCard(ctx, approvalTenantStatusIndex(tenantID, status))
		}
		oldest = pipe.ZRangeWithScores(ctx, approvalTenantStatusIndex(tenantID, types.ApprovalPending), 0, 0)
		return nil
	})
	if err != nil {
		return stats, err
	}
	for i, status := range approvalStatStatuses {
		stats.setCount(status, counts[i].Val())
	}
	if first := oldest.Val(); len(first) > 0 {
		at := time.UnixMicro(int64(first[0].Score)).UTC()
		stats.OldestPendingAt = &at
	}
	return stats, nil
}
