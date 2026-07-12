package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/memory"
	"github.com/wuxujun/ai-agent/internal/types"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// RedisStore implements Store using a Redis database.
type RedisStore struct {
	client *redis.Client
}

const (
	legacyTasksIndex    = "tasks:index"
	tasksIndexV2        = "tasks:index:v2"
	tasksIndexV2Marker  = "tasks:index:v2:migrated"
	taskStatusIndexBase = "tasks:status:"
)

var saveTaskScript = redis.NewScript(`
	local existing = redis.call('GET', KEYS[1])
	if existing then
		local ok, oldTask = pcall(cjson.decode, existing)
		if ok and oldTask["status"] then
			redis.call('ZREM', ARGV[5] .. oldTask["status"], ARGV[2])
		end
	end
	redis.call('SET', KEYS[1], ARGV[1])
	redis.call('ZADD', KEYS[2], 0, ARGV[2])
	redis.call('ZADD', KEYS[3], ARGV[4], ARGV[2])
	redis.call('ZADD', ARGV[5] .. ARGV[3], 0, ARGV[2])
	return 1
`)

// NewRedisStore creates a new RedisStore using standard URL/connection options.
func NewRedisStore(addr string, password string, db int) *RedisStore {
	rdb := redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: password,
		DB:       db,
	})
	return &RedisStore{client: rdb}
}

// NewRedisStoreFromURL creates a new RedisStore using a Redis URL (e.g. redis://user:pass@host:port/db).
func NewRedisStoreFromURL(urlStr string) (*RedisStore, error) {
	opts, err := redis.ParseURL(urlStr)
	if err != nil {
		return nil, fmt.Errorf("invalid redis url: %w", err)
	}
	rdb := redis.NewClient(opts)
	return &RedisStore{client: rdb}, nil
}

// taskKey formats the Redis key for a task.
func (r *RedisStore) taskKey(id string) string {
	return "task:" + id
}

// Close closes the Redis client.
func (r *RedisStore) Close() error {
	return r.client.Close()
}

// SaveFullTask serializes the entire task struct into JSON and saves it in Redis.
func (r *RedisStore) SaveFullTask(ctx context.Context, task *types.Task) error {
	ctx, span := tracer.Start(ctx, "store.redis.save_full_task")
	defer span.End()

	span.SetAttributes(
		attribute.String("agent.task.id", task.ID),
		attribute.Int("agent.task.trace_count", len(task.Trace)),
	)

	// Persist a copy of the task with Memory.Embedding stripped so the on-disk
	// shape matches the SQL backends. The caller's task struct is left intact.
	persistable := *task
	persistable.Memories = memoriesForPersistence(task.Memories)

	data, err := json.Marshal(&persistable)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "serialize task failed")
		return fmt.Errorf("failed to serialize task: %w", err)
	}

	err = saveTaskScript.Run(
		ctx,
		r.client,
		[]string{r.taskKey(task.ID), tasksIndexV2, legacyTasksIndex},
		data,
		task.ID,
		string(task.Status),
		time.Now().UnixNano(),
		taskStatusIndexBase,
	).Err()
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "redis task save script failed")
		return fmt.Errorf("failed to save task to redis: %w", err)
	}

	if task.Status == types.StatusCompleted {
		// Check if memory already exists to prevent repeated embedding generation
		exists, err := r.client.Exists(ctx, r.memoryKey("mem-"+task.ID)).Result()
		if err == nil && exists == 0 {
			taskSnap := *task
			taskSnap.Trace = make([]types.StepTrace, len(task.Trace))
			copy(taskSnap.Trace, task.Trace)
			taskSnap.Memories = make([]types.Memory, len(task.Memories))
			copy(taskSnap.Memories, task.Memories)
			taskSnap.Unresolved = make([]string, len(task.Unresolved))
			copy(taskSnap.Unresolved, task.Unresolved)

			go func() {
				asyncCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				mem, err := memory.CreateMemoryFromTask(asyncCtx, &taskSnap)
				if err != nil {
					log.Warn("failed to create memory for task in redis store", "task_id", taskSnap.ID, "error", err)
					return
				}
				_ = r.SaveMemory(asyncCtx, mem)
			}()
		}
	}

	return nil
}

// GetTask retrieves and deserializes the task from Redis. Returns sql.ErrNoRows if not found.
func (r *RedisStore) GetTask(ctx context.Context, id string) (*types.Task, error) {
	ctx, span := tracer.Start(ctx, "store.redis.get_task")
	defer span.End()

	span.SetAttributes(attribute.String("agent.task.id", id))

	val, err := r.client.Get(ctx, r.taskKey(id)).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			span.SetAttributes(attribute.Bool("agent.store.found", false))
			return nil, sql.ErrNoRows
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "redis get failed")
		return nil, fmt.Errorf("failed to retrieve task from redis: %w", err)
	}
	span.SetAttributes(attribute.Bool("agent.store.found", true))

	var task types.Task
	err = json.Unmarshal([]byte(val), &task)
	if err != nil {
		return nil, fmt.Errorf("failed to deserialize task: %w", err)
	}

	return &task, nil
}

// ListTasks returns tasks using lexicographically ordered ZSET indexes.
func (r *RedisStore) ListTasks(ctx context.Context, f ListFilter) ([]*types.Task, error) {
	ctx, span := tracer.Start(ctx, "store.list_tasks")
	defer span.End()

	if err := r.ensureTaskIndexes(ctx); err != nil {
		span.RecordError(err)
		return nil, err
	}

	limit := resolveLimit(f.Limit, 50, 500)
	index := tasksIndexV2
	if f.Status != "" {
		index = taskStatusIndexBase + string(f.Status)
	}
	start := int64(f.Offset)
	stop := start + int64(limit) - 1
	ids, err := r.client.ZRange(ctx, index, start, stop).Result()
	if err != nil {
		span.RecordError(err)
		return nil, err
	}

	if len(ids) == 0 {
		span.SetAttributes(attribute.Int("agent.store.task_count", 0))
		return []*types.Task{}, nil
	}

	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = r.taskKey(id)
	}

	vals, err := r.client.MGet(ctx, keys...).Result()
	if err != nil {
		span.RecordError(err)
		return nil, err
	}

	var tasks []*types.Task
	for _, valVal := range vals {
		if valVal == nil {
			continue
		}
		valStr, ok := valVal.(string)
		if !ok {
			continue
		}
		var t types.Task
		if err := json.Unmarshal([]byte(valStr), &t); err != nil {
			continue
		}
		tasks = append(tasks, &t)
	}

	span.SetAttributes(attribute.Int("agent.store.task_count", len(tasks)))
	return tasks, nil
}

func (r *RedisStore) ensureTaskIndexes(ctx context.Context) error {
	migrated, err := r.client.Exists(ctx, tasksIndexV2Marker).Result()
	if err != nil || migrated > 0 {
		return err
	}

	// Use Redis SETNX to acquire a distributed migration lock.
	// We set a 5-minute timeout on the lock to prevent deadlocks in case of crashes.
	lockKey := "tasks:index:v2:migration_lock"
	acquired, err := r.client.SetNX(ctx, lockKey, "1", 5*time.Minute).Result()
	if err != nil {
		return err
	}

	if !acquired {
		// Another instance holds the lock. Poll until the migration marker is set.
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		timeout := time.After(30 * time.Second)
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timeout:
				return fmt.Errorf("timeout waiting for Redis task index migration to be completed by peer")
			case <-ticker.C:
				migrated, err = r.client.Exists(ctx, tasksIndexV2Marker).Result()
				if err != nil {
					return err
				}
				if migrated > 0 {
					return nil
				}
			}
		}
	}
	defer r.client.Del(ctx, lockKey)

	// Double-check the marker now that we hold the lock.
	migrated, err = r.client.Exists(ctx, tasksIndexV2Marker).Result()
	if err != nil || migrated > 0 {
		return err
	}

	ids, err := r.client.ZRange(ctx, legacyTasksIndex, 0, -1).Result()
	if err != nil {
		return err
	}
	const batchSize = 500
	for start := 0; start < len(ids); start += batchSize {
		end := start + batchSize
		if end > len(ids) {
			end = len(ids)
		}
		batch := ids[start:end]
		keys := make([]string, len(batch))
		for i, id := range batch {
			keys[i] = r.taskKey(id)
		}
		values, err := r.client.MGet(ctx, keys...).Result()
		if err != nil {
			return err
		}
		pipe := r.client.TxPipeline()
		for i, raw := range values {
			text, ok := raw.(string)
			if !ok {
				continue
			}
			var task types.Task
			if err := json.Unmarshal([]byte(text), &task); err != nil {
				continue
			}
			pipe.ZAdd(ctx, tasksIndexV2, redis.Z{Score: 0, Member: batch[i]})
			pipe.ZAdd(ctx, taskStatusIndexBase+string(task.Status), redis.Z{Score: 0, Member: batch[i]})
		}
		if _, err := pipe.Exec(ctx); err != nil {
			return err
		}
	}
	return r.client.Set(ctx, tasksIndexV2Marker, "1", 0).Err()
}

// ExistsTask returns true if a task with the given id already exists.
func (r *RedisStore) ExistsTask(ctx context.Context, id string) (bool, error) {
	n, err := r.client.Exists(ctx, r.taskKey(id)).Result()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// memoryKey formats the Redis key for a memory.
func (r *RedisStore) memoryKey(id string) string {
	return "memory:" + id
}

// SaveMemory persists a memory entry to Redis and index.
func (r *RedisStore) SaveMemory(ctx context.Context, mem *types.Memory) error {
	data, err := json.Marshal(mem)
	if err != nil {
		return fmt.Errorf("failed to serialize memory: %w", err)
	}

	err = r.client.Set(ctx, r.memoryKey(mem.ID), data, 0).Err()
	if err != nil {
		return fmt.Errorf("failed to save memory to redis: %w", err)
	}

	// Add to memories:index ZSET
	err = r.client.ZAdd(ctx, "memories:index", redis.Z{
		Score:  float64(mem.Timestamp.UnixNano()),
		Member: mem.ID,
	}).Err()
	if err != nil {
		return fmt.Errorf("failed to index memory in redis: %w", err)
	}

	return nil
}

// QueryMemories retrieves and ranks memories based on vector similarity or keyword match in Redis.
// Loads up to store.memory_candidate_limit candidates from memories:index ZSET
// so the in-process ranking loop does not blow up memory on a large memory key space.
func (r *RedisStore) QueryMemories(ctx context.Context, query string, embedding []float32, limit int) ([]*types.Memory, error) {
	ctx, span := tracer.Start(ctx, "store.redis.query_memories")
	defer span.End()

	candidateLimit := resolveMemoryCandidateLimit()
	span.SetAttributes(
		attribute.Bool("agent.query.has_embedding", len(embedding) > 0),
		attribute.Int("agent.query.embedding_dim", len(embedding)),
		attribute.Int("agent.query.limit", limit),
		attribute.Int("agent.store.memory_candidate_limit", candidateLimit),
	)

	// 1. Get recent memory IDs from ZSET index (newest first)
	ids, err := r.client.ZRevRange(ctx, "memories:index", 0, int64(candidateLimit-1)).Result()
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "query memory index failed")
		return nil, err
	}

	if len(ids) == 0 {
		span.SetAttributes(
			attribute.Int("agent.store.memory_candidate_count", 0),
			attribute.Int("agent.store.memory_count", 0),
		)
		return []*types.Memory{}, nil
	}

	// 2. Fetch payloads in bulk using MGET
	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = r.memoryKey(id)
	}

	vals, err := r.client.MGet(ctx, keys...).Result()
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "load memories failed")
		return nil, err
	}

	decayRate := 0.0
	if cfg := config.Get(); cfg != nil {
		decayRate = cfg.Store.MemoryDecayRate
	}
	now := time.Now()

	type rankResult struct {
		mem   *types.Memory
		score float32
	}
	var ranked []rankResult

	for _, valVal := range vals {
		if valVal == nil {
			continue
		}
		valStr, ok := valVal.(string)
		if !ok {
			continue
		}
		var mem types.Memory
		if err := json.Unmarshal([]byte(valStr), &mem); err != nil {
			continue
		}

		var score float32
		if len(embedding) > 0 && len(mem.Embedding) > 0 {
			score = memory.CosineSimilarity(embedding, mem.Embedding)
		} else {
			// Fallback: simple case-insensitive term-match score
			qWords := strings.Fields(strings.ToLower(query))
			tWords := strings.ToLower(mem.Goal + " " + mem.KeyFindings + " " + mem.FinalAnswer)
			if len(qWords) > 0 {
				var matches float32
				for _, qw := range qWords {
					qw = strings.Trim(qw, ".,!?;:()[]{}'\"-")
					if len(qw) > 2 && strings.Contains(tWords, qw) {
						matches += 1.0
					}
				}
				score = matches / float32(len(qWords))
			}
		}
		score = memory.ApplyTimeDecay(score, mem.Timestamp, now, decayRate)
		ranked = append(ranked, rankResult{mem: &mem, score: score})
	}

	sort.Slice(ranked, func(i, j int) bool {
		return ranked[i].score > ranked[j].score
	})

	if limit > len(ranked) {
		limit = len(ranked)
	}

	res := make([]*types.Memory, 0, limit)
	for i := 0; i < limit; i++ {
		res = append(res, ranked[i].mem)
	}
	span.SetAttributes(
		attribute.Int("agent.store.memory_candidate_count", len(ranked)),
		attribute.Int("agent.store.memory_count", len(res)),
	)
	return res, nil
}

var transitionScript = redis.NewScript(`
	local taskKey = KEYS[1]
	local toStatus = ARGV[1]
	local statusPrefix = ARGV[2]

	local val = redis.call('GET', taskKey)
	if not val then
		return -1
	end

	local task = cjson.decode(val)
	local matched = false
	for i = 3, #ARGV do
		if task["status"] == ARGV[i] then
			matched = true
			break
		end
	end

	if not matched then
		return 0
	end

	local oldStatus = task["status"]
	task["status"] = toStatus
	redis.call('SET', taskKey, cjson.encode(task))
	redis.call('ZREM', statusPrefix .. oldStatus, task["id"])
	redis.call('ZADD', statusPrefix .. toStatus, 0, task["id"])
	return 1
`)

var acquireLeaseScript = redis.NewScript(`
	local key = KEYS[1]
	local owner = ARGV[1]
	local ttl = tonumber(ARGV[2])
	local current = redis.call('GET', key)
	if not current or current == owner then
		redis.call('SET', key, owner, 'PX', ttl)
		return 1
	end
	return 0
`)

var releaseLeaseScript = redis.NewScript(`
	local key = KEYS[1]
	if redis.call('GET', key) == ARGV[1] then
		return redis.call('DEL', key)
	end
	return 0
`)

// TryTransitionTaskStatus atomically attempts to transition a task's status from one of the allowed 'from' statuses to a target status.
// It returns (true, nil) if the transition succeeded, or (false, nil) if the status did not match.
func (r *RedisStore) TryTransitionTaskStatus(ctx context.Context, id string, from []types.TaskStatus, to types.TaskStatus) (bool, error) {
	ctx, span := tracer.Start(ctx, "store.redis.try_transition_task_status")
	defer span.End()

	args := make([]any, 0, len(from)+2)
	args = append(args, string(to))
	args = append(args, taskStatusIndexBase)
	for _, f := range from {
		args = append(args, string(f))
	}

	res, err := transitionScript.Run(ctx, r.client, []string{r.taskKey(id)}, args...).Int64()
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "redis transition script failed")
		return false, err
	}

	if res == -1 {
		return false, sql.ErrNoRows
	}
	return res == 1, nil
}

func (r *RedisStore) AcquireTaskLease(ctx context.Context, id, owner string, ttl time.Duration) (bool, error) {
	if owner == "" || ttl <= 0 {
		return false, nil
	}
	res, err := acquireLeaseScript.Run(
		ctx,
		r.client,
		[]string{"task:lease:" + id},
		owner,
		ttl.Milliseconds(),
	).Int64()
	if err != nil {
		return false, err
	}
	return res == 1, nil
}

func (r *RedisStore) ReleaseTaskLease(ctx context.Context, id, owner string) error {
	return releaseLeaseScript.Run(ctx, r.client, []string{"task:lease:" + id}, owner).Err()
}
