package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/wuxujun/ai-agent/internal/types"
)

// TaskStreamSnapshot is a single authoritative observation without Trace bodies.
// Counts and totals are derived from persisted Trace, not execution StepCount.
type TaskStreamSnapshot struct {
	ID           string           `json:"id"`
	Status       types.TaskStatus `json:"status"`
	StepCount    int              `json:"step_count"`
	UpdatedAt    time.Time        `json:"updated_at"`
	FinalAnswer  string           `json:"final_answer"`
	ErrorCode    string           `json:"error_code"`
	ErrorMessage string           `json:"error_message"`
	TraceCount   int              `json:"trace_count"`
	TokenUsage   types.TokenUsage `json:"token_usage"`
}

// TaskStreamSnapshotStore is optional for custom stores. Built-in stores provide
// it; the API falls back to one complete GetTask for legacy/custom adapters.
type TaskStreamSnapshotStore interface {
	GetTaskStreamSnapshot(context.Context, string) (TaskStreamSnapshot, error)
}

func StreamSnapshotFromTask(task *types.Task) TaskStreamSnapshot {
	if task == nil {
		return TaskStreamSnapshot{}
	}
	s := TaskStreamSnapshot{ID: task.ID, Status: task.Status, StepCount: task.StepCount, UpdatedAt: task.UpdatedAt, FinalAnswer: task.FinalAnswer, ErrorCode: task.ErrorCode, ErrorMessage: task.ErrorMessage, TraceCount: len(task.Trace)}
	for _, tr := range task.Trace {
		s.TokenUsage.PromptTokens += tr.TokenUsage.PromptTokens
		s.TokenUsage.CompletionTokens += tr.TokenUsage.CompletionTokens
		s.TokenUsage.TotalTokens += tr.TokenUsage.TotalTokens
	}
	return s
}
func (m *MemoryStore) GetTaskStreamSnapshot(ctx context.Context, id string) (TaskStreamSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return TaskStreamSnapshot{}, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	task := m.tasks[id]
	if task == nil {
		return TaskStreamSnapshot{}, sql.ErrNoRows
	}
	return StreamSnapshotFromTask(task), ctx.Err()
}
func (s *SQLiteStore) GetTaskStreamSnapshot(ctx context.Context, id string) (TaskStreamSnapshot, error) {
	return sqlTaskStreamSnapshot(ctx, s.db, id, false)
}
func (p *PostgresStore) GetTaskStreamSnapshot(ctx context.Context, id string) (TaskStreamSnapshot, error) {
	return sqlTaskStreamSnapshot(ctx, p.db, id, true)
}
func sqlTaskStreamSnapshot(ctx context.Context, db *sql.DB, id string, postgres bool) (TaskStreamSnapshot, error) {
	traceMark, taskMark := "?", "?"
	if postgres {
		traceMark, taskMark = "$1", "$2"
	}
	// One SQL statement observes task state and numeric Trace totals together.
	// Only the aggregate traverses Trace rows; no evidence/observation is decoded.
	query := `SELECT t.id,t.status,t.step_count,t.updated_at,t.final_answer,t.error_code,t.error_message,
 COALESCE(s.trace_count,0),COALESCE(s.prompt_tokens,0),COALESCE(s.completion_tokens,0),COALESCE(s.total_tokens,0)
 FROM tasks t LEFT JOIN (SELECT task_id,COUNT(*) AS trace_count,SUM(prompt_tokens) AS prompt_tokens,
 SUM(completion_tokens) AS completion_tokens,SUM(total_tokens) AS total_tokens
 FROM traces WHERE task_id = ` + traceMark + ` GROUP BY task_id) s ON s.task_id=t.id WHERE t.id = ` + taskMark
	var s TaskStreamSnapshot
	err := db.QueryRowContext(ctx, query, id, id).Scan(&s.ID, &s.Status, &s.StepCount, &s.UpdatedAt, &s.FinalAnswer, &s.ErrorCode, &s.ErrorMessage, &s.TraceCount, &s.TokenUsage.PromptTokens, &s.TokenUsage.CompletionTokens, &s.TokenUsage.TotalTokens)
	return s, err
}

type redisStreamStats struct {
	Version    int              `json:"version"`
	TraceCount int              `json:"trace_count"`
	TokenUsage types.TokenUsage `json:"token_usage"`
}

func (r *RedisStore) GetTaskStreamSnapshot(ctx context.Context, id string) (TaskStreamSnapshot, error) {
	raw, err := r.client.Get(ctx, r.taskMetadataKey(id)).Bytes()
	if err != nil && !errors.Is(err, redis.Nil) {
		return TaskStreamSnapshot{}, err
	}
	if err == nil {
		var metadata struct {
			TaskStreamSnapshot
			StreamStats *redisStreamStats `json:"stream_stats"`
		}
		if err := json.Unmarshal(raw, &metadata); err != nil {
			return TaskStreamSnapshot{}, err
		}
		if metadata.StreamStats != nil && metadata.StreamStats.Version == 1 {
			if metadata.ID != id || metadata.StreamStats.TraceCount < 0 {
				return TaskStreamSnapshot{}, errors.New("invalid task stream metadata")
			}
			metadata.TraceCount = metadata.StreamStats.TraceCount
			metadata.TokenUsage = metadata.StreamStats.TokenUsage
			return metadata.TaskStreamSnapshot, nil
		}
	}
	// Legacy metadata has no aggregate. Read one authoritative full JSON rather
	// than combining a metadata read and a separate Trace read across versions.
	task, err := r.GetTask(ctx, id)
	if err != nil {
		return TaskStreamSnapshot{}, err
	}
	return StreamSnapshotFromTask(task), nil
}
