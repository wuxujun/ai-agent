package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/wuxujun/ai-agent/internal/types"
)

type redisTaskSummary struct {
	TaskSummary
	IndexMember string `json:"index_member"`
}

func (r *RedisStore) taskSummaryKey(id string) string   { return "task:summary:v1:" + id }
func (r *RedisStore) taskMetadataKey(id string) string  { return "task:metadata:v1:" + id }
func (r *RedisStore) taskTraceKey(id string) string     { return "task:trace:v1:" + id }
func (r *RedisStore) taskTraceTimeKey(id string) string { return "task:trace_time:v1:" + id }

func taskSummaryMember(createdAt time.Time, id string) string {
	return createdAt.UTC().Format("20060102T150405.000000000Z") + ":" + id
}

func summaryIDFromMember(member string) (string, error) {
	separator := strings.IndexByte(member, ':')
	if separator < 1 || separator == len(member)-1 {
		return "", fmt.Errorf("invalid Redis task summary index member")
	}
	return member[separator+1:], nil
}

func encodeRedisTaskReadModel(task *types.Task) (summary, metadata, traces []byte, member string, err error) {
	copyTask := *task
	if copyTask.CreatedAt.IsZero() {
		// Pre-creation timestamps from older stores have no chronology. Keep
		// them at the beginning of the timeline with a nonzero cursor value.
		copyTask.CreatedAt = time.Unix(0, 0).UTC()
	}
	member = taskSummaryMember(copyTask.CreatedAt, copyTask.ID)
	summary, err = json.Marshal(redisTaskSummary{TaskSummary: summaryOf(&copyTask), IndexMember: member})
	if err != nil {
		return nil, nil, nil, "", err
	}
	copyTask.Trace = nil
	copyTask.Memories = nil
	metadata, err = json.Marshal(&copyTask)
	if err != nil {
		return nil, nil, nil, "", err
	}
	traceSnapshot := task.Trace
	if traceSnapshot == nil {
		traceSnapshot = []types.StepTrace{}
	}
	traces, err = json.Marshal(traceSnapshot)
	return summary, metadata, traces, member, err
}

// A repair uses compare-and-set against the complete task JSON. This prevents
// a migration reader from replacing a newer snapshot saved by another worker.
var repairTaskReadModelScript = redis.NewScript(`
	local raw = redis.call('GET', KEYS[1])
	if not raw then return 0 end
	if raw ~= ARGV[1] then return -1 end
	if redis.call('EXISTS', KEYS[2]) == 1 then return 1 end
	local summary = cjson.decode(ARGV[2])
	local tenant = summary['tenant_id'] or ''
	local status = summary['status'] or ''
	redis.call('SET', KEYS[2], ARGV[2])
	redis.call('SET', KEYS[3], ARGV[3])
	redis.call('DEL', KEYS[4], KEYS[5])
	local traces = cjson.decode(ARGV[4])
	for i = 1, #traces do redis.call('RPUSH', KEYS[4], cjson.encode(traces[i])) end
	redis.call('ZADD', ARGV[6], 0, ARGV[5])
	redis.call('ZADD', ARGV[7] .. tenant, 0, ARGV[5])
	redis.call('ZADD', ARGV[8] .. status, 0, ARGV[5])
	redis.call('ZADD', ARGV[9] .. tenant .. ':' .. status, 0, ARGV[5])
	return 1
`)

func (r *RedisStore) repairTaskReadModel(ctx context.Context, id string) error {
	for attempt := 0; attempt < 5; attempt++ {
		raw, err := r.client.Get(ctx, r.taskKey(id)).Result()
		if errors.Is(err, redis.Nil) {
			return nil
		}
		if err != nil {
			return err
		}
		var task types.Task
		if err := json.Unmarshal([]byte(raw), &task); err != nil {
			return fmt.Errorf("invalid task JSON during Redis summary migration: %w", err)
		}
		summary, metadata, traces, member, err := encodeRedisTaskReadModel(&task)
		if err != nil {
			return err
		}
		result, err := repairTaskReadModelScript.Run(ctx, r.client, []string{
			r.taskKey(id), r.taskSummaryKey(id), r.taskMetadataKey(id), r.taskTraceKey(id), r.taskTraceTimeKey(id),
		}, raw, summary, metadata, traces, member, taskSummaryIndex, taskSummaryTenantBase,
			taskSummaryStatusBase, taskSummaryTenantStatusBase).Int64()
		if err != nil {
			return err
		}
		if result >= 0 {
			return nil
		}
	}
	return fmt.Errorf("task %s changed repeatedly during Redis summary migration", id)
}

func (r *RedisStore) ensureTaskSummaryIndexes(ctx context.Context) error {
	if err := r.ensureTaskIndexes(ctx); err != nil {
		return err
	}
	migrated, err := r.client.Exists(ctx, taskSummaryMigrationMarker).Result()
	if err != nil || migrated > 0 {
		return err
	}
	lockKey := "tasks:summary:v1:migration_lock"
	owner := uuid.NewString()
	acquired, err := r.client.SetNX(ctx, lockKey, owner, 5*time.Minute).Result()
	if err != nil {
		return err
	}
	if !acquired {
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
				migrated, err := r.client.Exists(ctx, taskSummaryMigrationMarker).Result()
				if err != nil || migrated > 0 {
					return err
				}
			}
		}
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = releaseLeaseScript.Run(releaseCtx, r.client, []string{lockKey}, owner).Err()
	}()
	var cursor uint64
	for {
		renewed, err := renewTaskLeaseScript.Run(ctx, r.client, []string{lockKey}, owner, (5 * time.Minute).Milliseconds()).Int64()
		if err != nil {
			return err
		}
		if renewed != 1 {
			return errors.New("Redis task summary migration lease lost")
		}
		entries, next, err := r.client.ZScan(ctx, tasksIndexV2, cursor, "*", 500).Result()
		if err != nil {
			return err
		}
		for i := 0; i < len(entries); i += 2 {
			if err := r.repairTaskReadModel(ctx, entries[i]); err != nil {
				return err
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	finished, err := finishTaskIndexMigrationScript.Run(ctx, r.client,
		[]string{lockKey, taskSummaryMigrationMarker}, owner).Int64()
	if err != nil {
		return err
	}
	if finished != 1 {
		return errors.New("Redis task summary migration lease lost")
	}
	return nil
}

func (r *RedisStore) readTaskMetadata(ctx context.Context, id string) (*types.Task, error) {
	raw, err := r.client.Get(ctx, r.taskMetadataKey(id)).Result()
	if errors.Is(err, redis.Nil) {
		return nil, sql.ErrNoRows
	}
	if err != nil {
		return nil, err
	}
	var task types.Task
	if err := json.Unmarshal([]byte(raw), &task); err != nil {
		return nil, err
	}
	return &task, nil
}

func (r *RedisStore) readTaskTracePage(ctx context.Context, id string, afterSequence int64, limit int) ([]TaskTraceEvent, error) {
	if afterSequence > int64(^uint64(0)>>1)-int64(limit) {
		return []TaskTraceEvent{}, nil
	}
	pipe := r.client.Pipeline()
	traceCommand := pipe.LRange(ctx, r.taskTraceKey(id), afterSequence, afterSequence+int64(limit)-1)
	timeCommand := pipe.LRange(ctx, r.taskTraceTimeKey(id), afterSequence, afterSequence+int64(limit)-1)
	_, err := pipe.Exec(ctx)
	if err != nil {
		return nil, err
	}
	values := traceCommand.Val()
	times := timeCommand.Val()
	items := make([]TaskTraceEvent, 0, len(values))
	for i, raw := range values {
		var trace types.StepTrace
		if err := json.Unmarshal([]byte(raw), &trace); err != nil {
			return nil, err
		}
		item := TaskTraceEvent{Sequence: afterSequence + int64(i) + 1, Trace: trace}
		if i < len(times) && times[i] != "" {
			at, err := time.Parse(time.RFC3339Nano, times[i])
			if err != nil {
				return nil, err
			}
			at = at.UTC()
			item.RecordedAt = &at
		}
		items = append(items, item)
	}
	return items, nil
}
