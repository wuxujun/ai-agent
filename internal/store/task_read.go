package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/redis/go-redis/v9"
	"github.com/wuxujun/ai-agent/internal/types"
)

func summaryOf(task *types.Task) TaskSummary {
	return TaskSummary{
		ID: task.ID, TenantID: task.TenantID, SessionID: task.SessionID,
		Goal: task.Goal, Status: task.Status, Mode: task.Mode, Team: task.Team,
		MaxSteps: task.MaxSteps, StepCount: task.StepCount, LLMCalls: task.LLMCalls,
		LLMEstimatedCostUSD: task.LLMEstimatedCostUSD,
		CreatedAt:           task.CreatedAt, UpdatedAt: task.UpdatedAt,
	}
}

func summaryBefore(item TaskSummary, filter TaskSummaryFilter) bool {
	return filter.BeforeID == "" || item.CreatedAt.Before(filter.BeforeCreatedAt) ||
		(item.CreatedAt.Equal(filter.BeforeCreatedAt) && item.ID < filter.BeforeID)
}

func sortSummaries(items []TaskSummary) {
	sort.Slice(items, func(i, j int) bool {
		if items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].ID > items[j].ID
		}
		return items[i].CreatedAt.After(items[j].CreatedAt)
	})
}

func (m *MemoryStore) ListTaskSummaries(ctx context.Context, filter TaskSummaryFilter) ([]TaskSummary, error) {
	filter.Limit = resolveLimit(filter.Limit, 20, 101)
	m.mu.RLock()
	defer m.mu.RUnlock()
	items := make([]TaskSummary, 0, len(m.tasks))
	for _, task := range m.tasks {
		if filter.TenantID != "" && task.TenantID != filter.TenantID ||
			filter.SessionID != "" && task.SessionID != filter.SessionID ||
			filter.Status != "" && task.Status != filter.Status {
			continue
		}
		item := summaryOf(task)
		if summaryBefore(item, filter) {
			items = append(items, item)
		}
	}
	sortSummaries(items)
	if len(items) > filter.Limit {
		items = items[:filter.Limit]
	}
	return items, ctx.Err()
}

func (m *MemoryStore) GetTaskWithoutTrace(ctx context.Context, id string) (*types.Task, error) {
	task, err := m.GetTask(ctx, id)
	if err == nil {
		task.Trace = nil
	}
	return task, err
}

func (m *MemoryStore) ListTaskTraces(ctx context.Context, id string, afterSequence int64, limit int) ([]TaskTraceEvent, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	task := m.tasks[id]
	if task == nil {
		return nil, sql.ErrNoRows
	}
	items := make([]TaskTraceEvent, 0, limit)
	for i := afterSequence; i < int64(len(task.Trace)) && len(items) < limit; i++ {
		trace := task.Trace[i]
		if trace.OccurredAt != nil {
			at := *trace.OccurredAt
			trace.OccurredAt = &at
		}
		if trace.DurationMS != nil {
			duration := *trace.DurationMS
			trace.DurationMS = &duration
		}
		if trace.Evidence != nil {
			trace.Evidence = append([]types.Evidence(nil), trace.Evidence...)
			for j := range trace.Evidence {
				trace.Evidence[j].Lines = append([]string(nil), trace.Evidence[j].Lines...)
			}
		}
		item := TaskTraceEvent{Sequence: i + 1, Trace: trace}
		if times := m.traceTimes[id]; i < int64(len(times)) && !times[i].IsZero() {
			recorded := times[i]
			item.RecordedAt = &recorded
		}
		items = append(items, item)
	}
	return items, ctx.Err()
}

func (s *SQLiteStore) ListTaskSummaries(ctx context.Context, filter TaskSummaryFilter) ([]TaskSummary, error) {
	return listSQLTaskSummaries(ctx, s.db, filter, false)
}

func (p *PostgresStore) ListTaskSummaries(ctx context.Context, filter TaskSummaryFilter) ([]TaskSummary, error) {
	return listSQLTaskSummaries(ctx, p.db, filter, true)
}

func listSQLTaskSummaries(ctx context.Context, db *sql.DB, filter TaskSummaryFilter, postgres bool) ([]TaskSummary, error) {
	filter.Limit = resolveLimit(filter.Limit, 20, 101)
	args := make([]any, 0, 6)
	conditions := make([]string, 0, 4)
	mark := func() string {
		if postgres {
			return fmt.Sprintf("$%d", len(args)+1)
		}
		return "?"
	}
	if filter.TenantID != "" {
		conditions = append(conditions, "tenant_id = "+mark())
		args = append(args, filter.TenantID)
	}
	if filter.SessionID != "" {
		conditions = append(conditions, "session_id = "+mark())
		args = append(args, filter.SessionID)
	}
	if filter.Status != "" {
		conditions = append(conditions, "status = "+mark())
		args = append(args, filter.Status)
	}
	if filter.BeforeID != "" {
		first := mark()
		args = append(args, filter.BeforeCreatedAt)
		second := mark()
		args = append(args, filter.BeforeCreatedAt)
		third := mark()
		args = append(args, filter.BeforeID)
		conditions = append(conditions, "(created_at < "+first+" OR (created_at = "+second+" AND id < "+third+"))")
	}
	query := `SELECT id, tenant_id, session_id, goal, status, execution_mode, team_name,
		max_steps, step_count, llm_calls, llm_estimated_cost_usd, created_at, updated_at FROM tasks`
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += " ORDER BY created_at DESC, id DESC LIMIT " + mark()
	args = append(args, filter.Limit)
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]TaskSummary, 0, filter.Limit)
	for rows.Next() {
		var item TaskSummary
		if err := rows.Scan(&item.ID, &item.TenantID, &item.SessionID, &item.Goal,
			&item.Status, &item.Mode, &item.Team, &item.MaxSteps, &item.StepCount,
			&item.LLMCalls, &item.LLMEstimatedCostUSD, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *SQLiteStore) ListTaskTraces(ctx context.Context, id string, afterSequence int64, limit int) ([]TaskTraceEvent, error) {
	return listSQLTaskTraces(ctx, s.db, id, afterSequence, limit, false)
}

func (p *PostgresStore) ListTaskTraces(ctx context.Context, id string, afterSequence int64, limit int) ([]TaskTraceEvent, error) {
	return listSQLTaskTraces(ctx, p.db, id, afterSequence, limit, true)
}

func listSQLTaskTraces(ctx context.Context, db *sql.DB, id string, afterSequence int64, limit int, postgres bool) ([]TaskTraceEvent, error) {
	taskMarker, stepMarker, limitMarker := "?", "?", "?"
	if postgres {
		taskMarker, stepMarker, limitMarker = "$1", "$2", "$3"
	}
	query := `SELECT step, COALESCE(execution_step, step), goal, action, query, observation,
		evidence_json, agent_role, error_text, prompt_tokens, completion_tokens, total_tokens, occurred_at, duration_ms, recorded_at
		FROM traces WHERE task_id = ` + taskMarker + ` AND step > ` + stepMarker +
		` ORDER BY step ASC LIMIT ` + limitMarker
	rows, err := db.QueryContext(ctx, query, id, afterSequence, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]TaskTraceEvent, 0, limit)
	for rows.Next() {
		var item TaskTraceEvent
		var evidenceJSON, agentRole string
		var occurredAt sql.NullTime
		var durationMS sql.NullInt64
		var recordedAt sql.NullTime
		tr := &item.Trace
		if err := rows.Scan(&item.Sequence, &tr.Step, &tr.Goal, &tr.Action, &tr.Query,
			&tr.Observation, &evidenceJSON, &agentRole, &tr.Error,
			&tr.TokenUsage.PromptTokens, &tr.TokenUsage.CompletionTokens, &tr.TokenUsage.TotalTokens,
			&occurredAt, &durationMS, &recordedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(evidenceJSON), &tr.Evidence); err != nil {
			return nil, err
		}
		tr.AgentRole = types.AgentRole(agentRole)
		if occurredAt.Valid {
			at := occurredAt.Time.UTC()
			tr.OccurredAt = &at
		}
		if durationMS.Valid {
			duration := durationMS.Int64
			tr.DurationMS = &duration
		}
		if recordedAt.Valid {
			at := recordedAt.Time.UTC()
			item.RecordedAt = &at
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *RedisStore) GetTaskWithoutTrace(ctx context.Context, id string) (*types.Task, error) {
	task, err := r.readTaskMetadata(ctx, id)
	if err == sql.ErrNoRows {
		// Tasks written by an older binary acquire the read model during the
		// summary index migration; direct detail reads remain compatible.
		task, err = r.GetTask(ctx, id)
		if err == nil {
			task.Trace = nil
		}
	}
	return task, err
}

func (r *RedisStore) ListTaskTraces(ctx context.Context, id string, afterSequence int64, limit int) ([]TaskTraceEvent, error) {
	limit = resolveLimit(limit, 100, 201)
	_, err := r.readTaskMetadata(ctx, id)
	if err == nil {
		return r.readTaskTracePage(ctx, id, afterSequence, limit)
	}
	if err != sql.ErrNoRows {
		return nil, err
	}
	task, err := r.GetTask(ctx, id)
	if err != nil {
		return nil, err
	}
	items := make([]TaskTraceEvent, 0, limit)
	for i := afterSequence; i < int64(len(task.Trace)) && len(items) < limit; i++ {
		items = append(items, TaskTraceEvent{Sequence: i + 1, Trace: task.Trace[i]})
	}
	return items, nil
}

func (r *RedisStore) ListTaskSummaries(ctx context.Context, filter TaskSummaryFilter) ([]TaskSummary, error) {
	filter.Limit = resolveLimit(filter.Limit, 20, 101)
	if err := r.ensureTaskSummaryIndexes(ctx); err != nil {
		return nil, err
	}
	index := taskSummaryIndex
	if filter.TenantID != "" && filter.Status != "" {
		index = taskSummaryTenantStatusBase + filter.TenantID + ":" + string(filter.Status)
	} else if filter.TenantID != "" {
		index = taskSummaryTenantBase + filter.TenantID
	} else if filter.Status != "" {
		index = taskSummaryStatusBase + string(filter.Status)
	}
	items := make([]TaskSummary, 0, filter.Limit)
	max := "+"
	if filter.BeforeID != "" {
		max = "(" + taskSummaryMember(filter.BeforeCreatedAt, filter.BeforeID)
	}
	for len(items) < filter.Limit {
		members, err := r.client.ZRevRangeByLex(ctx, index, &redis.ZRangeBy{Max: max, Min: "-", Count: 200}).Result()
		if err != nil {
			return nil, err
		}
		if len(members) == 0 {
			break
		}
		keys := make([]string, len(members))
		for i, member := range members {
			id, err := summaryIDFromMember(member)
			if err != nil {
				return nil, err
			}
			keys[i] = r.taskSummaryKey(id)
		}
		values, err := r.client.MGet(ctx, keys...).Result()
		if err != nil {
			return nil, err
		}
		for _, value := range values {
			raw, ok := value.(string)
			if !ok {
				continue
			}
			var summary redisTaskSummary
			if err := json.Unmarshal([]byte(raw), &summary); err != nil {
				return nil, err
			}
			if filter.TenantID != "" && summary.TenantID != filter.TenantID ||
				filter.SessionID != "" && summary.SessionID != filter.SessionID ||
				filter.Status != "" && summary.Status != filter.Status {
				continue
			}
			if summaryBefore(summary.TaskSummary, filter) {
				items = append(items, summary.TaskSummary)
				if len(items) == filter.Limit {
					break
				}
			}
		}
		max = "(" + members[len(members)-1]
		if len(members) < 200 {
			break
		}
	}
	return items, nil
}
