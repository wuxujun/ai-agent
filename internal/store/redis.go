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
	"github.com/wuxujun/ai-agent/internal/memory"
	"github.com/wuxujun/ai-agent/internal/types"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// RedisStore implements Store using a Redis database.
type RedisStore struct {
	client *redis.Client
}

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

	err = r.client.Set(ctx, r.taskKey(task.ID), data, 0).Err()
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "redis set failed")
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

// ListTasks returns tasks stored in Redis using SCAN. Note: this may be slow for large datasets.
// Status filtering and pagination are applied in-process after fetching.
func (r *RedisStore) ListTasks(ctx context.Context, f ListFilter) ([]*types.Task, error) {
	ctx, span := tracer.Start(ctx, "store.list_tasks")
	defer span.End()

	var cursor uint64
	var keys []string
	for {
		var batch []string
		var err error
		batch, cursor, err = r.client.Scan(ctx, cursor, "task:*", 100).Result()
		if err != nil {
			span.RecordError(err)
			return nil, err
		}
		keys = append(keys, batch...)
		if cursor == 0 || len(keys) >= 500 {
			break
		}
	}
	if len(keys) > 500 {
		keys = keys[:500]
	}

	tasks := make([]*types.Task, 0, len(keys))
	for _, key := range keys {
		val, err := r.client.Get(ctx, key).Result()
		if err != nil {
			continue // skip missing/expired keys
		}
		var t types.Task
		if err := json.Unmarshal([]byte(val), &t); err != nil {
			continue
		}
		if f.Status != "" && t.Status != f.Status {
			continue
		}
		tasks = append(tasks, &t)
	}

	// Apply pagination
	limit := resolveLimit(f.Limit, 50, 500)
	offset := f.Offset
	if offset >= len(tasks) {
		span.SetAttributes(attribute.Int("agent.store.task_count", 0))
		return []*types.Task{}, nil
	}
	tasks = tasks[offset:]
	if len(tasks) > limit {
		tasks = tasks[:limit]
	}
	span.SetAttributes(attribute.Int("agent.store.task_count", len(tasks)))
	return tasks, nil
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

// SaveMemory persists a memory entry to Redis.
func (r *RedisStore) SaveMemory(ctx context.Context, mem *types.Memory) error {
	data, err := json.Marshal(mem)
	if err != nil {
		return fmt.Errorf("failed to serialize memory: %w", err)
	}

	err = r.client.Set(ctx, r.memoryKey(mem.ID), data, 0).Err()
	if err != nil {
		return fmt.Errorf("failed to save memory to redis: %w", err)
	}

	return nil
}

// QueryMemories retrieves and ranks memories based on vector similarity or keyword match in Redis.
// Caps the SCAN at store.memory_candidate_limit keys (default 200) so the
// in-process ranking loop does not blow up memory on a large memory key space.
// Logs a warning when the cap is hit so operators can raise it when older
// memories matter for recall.
func (r *RedisStore) QueryMemories(ctx context.Context, query string, embedding []float32, limit int) ([]*types.Memory, error) {
	candidateLimit := resolveMemoryCandidateLimit()
	var cursor uint64
	var keys []string
	for {
		var batch []string
		var err error
		batch, cursor, err = r.client.Scan(ctx, cursor, "memory:*", 100).Result()
		if err != nil {
			return nil, err
		}
		keys = append(keys, batch...)
		if cursor == 0 || len(keys) >= candidateLimit {
			break
		}
	}
	if len(keys) > candidateLimit {
		keys = keys[:candidateLimit]
	}

	type rankResult struct {
		mem   *types.Memory
		score float32
	}
	var ranked []rankResult

	for _, key := range keys {
		val, err := r.client.Get(ctx, key).Result()
		if err != nil {
			continue
		}
		var mem types.Memory
		if err := json.Unmarshal([]byte(val), &mem); err != nil {
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
		ranked = append(ranked, rankResult{mem: &mem, score: score})
	}

	if len(ranked) >= candidateLimit {
		log.Warn("memory candidate scan hit store.memory_candidate_limit; older rows excluded from ranking",
			"candidate_limit", candidateLimit,
			"backend", "redis",
		)
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
	return res, nil
}

var transitionScript = redis.NewScript(`
	local taskKey = KEYS[1]
	local toStatus = ARGV[1]
	
	local val = redis.call('GET', taskKey)
	if not val then
		return -1
	end
	
	local task = cjson.decode(val)
	local matched = false
	for i = 2, #ARGV do
		if task["status"] == ARGV[i] then
			matched = true
			break
		end
	end
	
	if not matched then
		return 0
	end
	
	task["status"] = toStatus
	redis.call('SET', taskKey, cjson.encode(task))
	return 1
`)

// TryTransitionTaskStatus atomically attempts to transition a task's status from one of the allowed 'from' statuses to a target status.
// It returns (true, nil) if the transition succeeded, or (false, nil) if the status did not match.
func (r *RedisStore) TryTransitionTaskStatus(ctx context.Context, id string, from []types.TaskStatus, to types.TaskStatus) (bool, error) {
	ctx, span := tracer.Start(ctx, "store.redis.try_transition_task_status")
	defer span.End()

	args := make([]any, 0, len(from)+1)
	args = append(args, string(to))
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

