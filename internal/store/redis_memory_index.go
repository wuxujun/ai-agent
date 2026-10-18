package store

import (
	"context"
	"encoding/json"

	"github.com/redis/go-redis/v9"
	"github.com/wuxujun/ai-agent/internal/types"
)

var backfillMemoryTaskScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) ~= ARGV[1] then return 0 end
redis.call('ZADD', KEYS[2], ARGV[2], ARGV[3])
return 1
`)

// Old deployments did not index memories by task. Compare each source value
// inside Lua so a concurrent overwrite or deletion cannot resurrect its index.
func (r *RedisStore) ensureMemoryTaskIndexes(ctx context.Context) error {
	const marker = "memories:task-indexed:v1"
	if n, err := r.client.Exists(ctx, marker).Result(); err != nil || n != 0 {
		return err
	}
	var cursor uint64
	for {
		keys, next, err := r.client.Scan(ctx, cursor, "memory:*", 200).Result()
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
			var mem types.Memory
			if err := json.Unmarshal(raw, &mem); err != nil {
				return err
			}
			if mem.TaskID != "" {
				if err := backfillMemoryTaskScript.Run(ctx, r.client, []string{key, "memories:task:" + mem.TaskID}, raw, mem.Timestamp.UnixNano(), mem.ID).Err(); err != nil {
					return err
				}
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return r.client.Set(ctx, marker, "1", 0).Err()
}
