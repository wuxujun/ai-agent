package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/redis/go-redis/v9"
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

	data, err := json.Marshal(task)
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
