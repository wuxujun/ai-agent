package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/wuxujun/ai-agent/internal/types"
)

func (r *RedisStore) approvalKey(id string) string { return "approval:" + id }

func approvalTaskIndex(tenantID, taskID string) string {
	return "approvals:task:" + tenantID + ":" + taskID
}

func approvalTenantIndex(tenantID string) string { return "approvals:tenant:" + tenantID }
func approvalTenantStatusIndex(tenantID string, status types.DurableApprovalStatus) string {
	return "approvals:tenant-status:" + tenantID + ":" + string(status)
}
func approvalTenantIndexMarker(tenantID string) string {
	return "approvals:tenant-indexed:v2:" + tenantID
}

var createApprovalScript = redis.NewScript(`
	if redis.call('EXISTS', KEYS[1]) == 1 then return 0 end
	redis.call('SET', KEYS[1], ARGV[1])
	redis.call('ZADD', KEYS[2], ARGV[2], ARGV[3])
	redis.call('ZADD', KEYS[3], ARGV[2], ARGV[3])
	redis.call('ZADD', KEYS[4], ARGV[2], ARGV[3])
	return 1
`)

var backfillApprovalTenantScript = redis.NewScript(`
	if redis.call('GET', KEYS[1]) ~= ARGV[1] then return 0 end
	redis.call('ZADD', KEYS[2], ARGV[2], ARGV[3])
	redis.call('ZADD', KEYS[3], ARGV[2], ARGV[3])
	return 1
`)

func (r *RedisStore) CreateApproval(ctx context.Context, approval *types.DurableApproval) error {
	record := types.CloneDurableApproval(approval)
	if err := record.Validate(); err != nil {
		return err
	}
	now := time.Now().UTC()
	if record.CreatedAt.IsZero() {
		record.CreatedAt = now
	}
	record.CreatedAt = record.CreatedAt.UTC().Truncate(time.Microsecond)
	record.UpdatedAt = now
	if record.Version == 0 {
		record.Version = 1
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("marshal approval: %w", err)
	}
	created, err := createApprovalScript.Run(ctx, r.client,
		[]string{r.approvalKey(record.ID), approvalTaskIndex(record.TenantID, record.TaskID), approvalTenantIndex(record.TenantID), approvalTenantStatusIndex(record.TenantID, record.Status)},
		encoded, record.CreatedAt.UnixMicro(), record.ID).Int64()
	if err != nil {
		return err
	}
	if created == 0 {
		return fmt.Errorf("approval %q already exists", record.ID)
	}
	return nil
}

func (r *RedisStore) GetApproval(ctx context.Context, id, tenantID string) (*types.DurableApproval, error) {
	raw, err := r.client.Get(ctx, r.approvalKey(id)).Bytes()
	if err == redis.Nil {
		return nil, sql.ErrNoRows
	}
	if err != nil {
		return nil, err
	}
	var approval types.DurableApproval
	if err := json.Unmarshal(raw, &approval); err != nil {
		return nil, fmt.Errorf("decode approval %q: %w", id, err)
	}
	if approval.TenantID != tenantID {
		return nil, sql.ErrNoRows
	}
	return types.CloneDurableApproval(&approval), nil
}

func (r *RedisStore) ListTaskApprovals(ctx context.Context, taskID, tenantID string, status types.DurableApprovalStatus) ([]*types.DurableApproval, error) {
	ids, err := r.client.ZRange(ctx, approvalTaskIndex(tenantID, taskID), 0, -1).Result()
	if err != nil || len(ids) == 0 {
		return []*types.DurableApproval{}, err
	}
	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = r.approvalKey(id)
	}
	values, err := r.client.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, err
	}
	result := make([]*types.DurableApproval, 0, len(values))
	for _, value := range values {
		if value == nil {
			continue
		}
		var approval types.DurableApproval
		if err := json.Unmarshal([]byte(value.(string)), &approval); err != nil {
			return nil, fmt.Errorf("decode task approval: %w", err)
		}
		if approval.TaskID == taskID && approval.TenantID == tenantID && (status == "" || approval.Status == status) {
			result = append(result, &approval)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].ID < result[j].ID
		}
		return result[i].CreatedAt.Before(result[j].CreatedAt)
	})
	return result, nil
}

// Existing Redis databases predate the tenant index. A tenant's first inbox
// read reconstructs it from durable records; concurrent creators also update
// the index atomically, so repeating this migration is harmless.
func (r *RedisStore) ensureApprovalTenantIndex(ctx context.Context, tenantID string) error {
	indexed, err := r.client.Exists(ctx, approvalTenantIndexMarker(tenantID)).Result()
	if err != nil || indexed != 0 {
		return err
	}
	var cursor uint64
	for {
		keys, next, err := r.client.Scan(ctx, cursor, "approval:*", 200).Result()
		if err != nil {
			return err
		}
		for _, key := range keys {
			raw, err := r.client.Get(ctx, key).Bytes()
			if err == redis.Nil {
				continue
			}
			if err != nil {
				return err
			}
			var approval types.DurableApproval
			if err := json.Unmarshal(raw, &approval); err != nil {
				return fmt.Errorf("decode approval tenant index: %w", err)
			}
			if approval.TenantID == tenantID {
				if err := backfillApprovalTenantScript.Run(ctx, r.client,
					[]string{key, approvalTenantIndex(tenantID), approvalTenantStatusIndex(tenantID, approval.Status)},
					raw, approval.CreatedAt.UnixMicro(), approval.ID).Err(); err != nil {
					return err
				}
			}
		}
		cursor = next
		if cursor == 0 {
			return r.client.Set(ctx, approvalTenantIndexMarker(tenantID), "1", 0).Err()
		}
	}
}

func (r *RedisStore) ListApprovals(ctx context.Context, filter ApprovalListFilter) ([]*types.DurableApproval, error) {
	if filter.TenantID == "" {
		return nil, fmt.Errorf("approval tenant is required")
	}
	if err := r.ensureApprovalTenantIndex(ctx, filter.TenantID); err != nil {
		return nil, err
	}
	index := approvalTenantIndex(filter.TenantID)
	if filter.Status != "" {
		index = approvalTenantStatusIndex(filter.TenantID, filter.Status)
	}
	limit := resolveLimit(filter.Limit, 50, 200)
	result := make([]*types.DurableApproval, 0, limit)
	for start := int64(0); len(result) < limit; start += 100 {
		ids, err := r.client.ZRevRange(ctx, index, start, start+99).Result()
		if err != nil {
			return nil, err
		}
		if len(ids) == 0 {
			break
		}
		keys := make([]string, 0, len(ids))
		for _, id := range ids {
			keys = append(keys, r.approvalKey(id))
		}
		values, err := r.client.MGet(ctx, keys...).Result()
		if err != nil {
			return nil, err
		}
		for _, value := range values {
			if value == nil {
				continue
			}
			var approval types.DurableApproval
			if err := json.Unmarshal([]byte(value.(string)), &approval); err != nil {
				return nil, fmt.Errorf("decode approval inbox: %w", err)
			}
			if approval.TenantID != filter.TenantID || (filter.Status != "" && approval.Status != filter.Status) {
				continue
			}
			if !filter.BeforeCreatedAt.IsZero() && filter.BeforeID != "" &&
				(approval.CreatedAt.After(filter.BeforeCreatedAt) ||
					(approval.CreatedAt.Equal(filter.BeforeCreatedAt) && approval.ID >= filter.BeforeID)) {
				continue
			}
			result = append(result, &approval)
		}
		if len(ids) < 100 {
			break
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].ID > result[j].ID
		}
		return result[i].CreatedAt.After(result[j].CreatedAt)
	})
	if len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (r *RedisStore) updateApproval(ctx context.Context, id string, mutate func(*types.DurableApproval) (bool, error)) (bool, error) {
	key := r.approvalKey(id)
	var changed bool
	err := r.client.Watch(ctx, func(tx *redis.Tx) error {
		raw, err := tx.Get(ctx, key).Bytes()
		if err == redis.Nil {
			return sql.ErrNoRows
		}
		if err != nil {
			return err
		}
		var approval types.DurableApproval
		if err := json.Unmarshal(raw, &approval); err != nil {
			return err
		}
		previousStatus := approval.Status
		changed, err = mutate(&approval)
		if err != nil || !changed {
			return err
		}
		encoded, err := json.Marshal(&approval)
		if err != nil {
			return err
		}
		_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			pipe.Set(ctx, key, encoded, 0)
			if approval.Status != previousStatus {
				pipe.ZRem(ctx, approvalTenantStatusIndex(approval.TenantID, previousStatus), approval.ID)
				pipe.ZAdd(ctx, approvalTenantStatusIndex(approval.TenantID, approval.Status), redis.Z{Score: float64(approval.CreatedAt.UnixMicro()), Member: approval.ID})
			}
			return nil
		})
		return err
	}, key)
	if err == redis.TxFailedErr {
		return false, nil
	}
	return changed && err == nil, err
}

func (r *RedisStore) TransitionApproval(ctx context.Context, id, tenantID string, expectedVersion int64, from, to types.DurableApprovalStatus, payload []byte) (bool, error) {
	if !types.CanTransitionApproval(from, to) {
		return false, fmt.Errorf("invalid approval transition %s -> %s", from, to)
	}
	return r.updateApproval(ctx, id, func(approval *types.DurableApproval) (bool, error) {
		if approval.TenantID != tenantID || approval.Version != expectedVersion || approval.Status != from {
			return false, nil
		}
		now := time.Now().UTC()
		approval.Status = to
		approval.Version++
		approval.ResolutionPayload = append([]byte(nil), payload...)
		approval.UpdatedAt = now
		approval.ResolvedAt = now
		if to == types.ApprovalConsumed || to == types.ApprovalRejected || to == types.ApprovalExpired {
			approval.Owner = ""
			approval.LeaseExpiresAt = time.Time{}
		}
		return true, nil
	})
}

func (r *RedisStore) AcquireApprovalLease(ctx context.Context, id, owner string, ttl time.Duration) (bool, error) {
	if owner == "" || ttl <= 0 {
		return false, nil
	}
	return r.updateApproval(ctx, id, func(approval *types.DurableApproval) (bool, error) {
		now := time.Now().UTC()
		if approval.Owner != "" && approval.Owner != owner && approval.LeaseExpiresAt.After(now) {
			return false, nil
		}
		approval.Owner = owner
		approval.LeaseExpiresAt = now.Add(ttl)
		return true, nil
	})
}

func (r *RedisStore) ReleaseApprovalLease(ctx context.Context, id, owner string) error {
	_, err := r.updateApproval(ctx, id, func(approval *types.DurableApproval) (bool, error) {
		if approval.Owner != owner {
			return false, nil
		}
		approval.Owner = ""
		approval.LeaseExpiresAt = time.Time{}
		return true, nil
	})
	if err == sql.ErrNoRows {
		return nil
	}
	return err
}

func (r *RedisStore) DeleteTerminalApprovalsBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	var cursor uint64
	var deleted int64
	for {
		keys, next, err := r.client.Scan(ctx, cursor, "approval:*", 200).Result()
		if err != nil {
			return deleted, err
		}
		for _, key := range keys {
			removed := false
			err := r.client.Watch(ctx, func(tx *redis.Tx) error {
				raw, getErr := tx.Get(ctx, key).Bytes()
				if getErr == redis.Nil {
					return nil
				}
				if getErr != nil {
					return getErr
				}
				var approval types.DurableApproval
				if decodeErr := json.Unmarshal(raw, &approval); decodeErr != nil {
					return fmt.Errorf("decode approval cleanup record: %w", decodeErr)
				}
				terminal := approval.Status == types.ApprovalConsumed || approval.Status == types.ApprovalExpired
				if !terminal || approval.ResolvedAt.IsZero() || !approval.ResolvedAt.Before(cutoff) {
					return nil
				}
				_, txErr := tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
					pipe.Del(ctx, key)
					pipe.ZRem(ctx, approvalTaskIndex(approval.TenantID, approval.TaskID), approval.ID)
					pipe.ZRem(ctx, approvalTenantIndex(approval.TenantID), approval.ID)
					pipe.ZRem(ctx, approvalTenantStatusIndex(approval.TenantID, approval.Status), approval.ID)
					return nil
				})
				removed = txErr == nil
				return txErr
			}, key)
			if err != nil && err != redis.TxFailedErr {
				return deleted, err
			}
			if removed {
				deleted++
			}
		}
		cursor = next
		if cursor == 0 {
			return deleted, nil
		}
	}
}
