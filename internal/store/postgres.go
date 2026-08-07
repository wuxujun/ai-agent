package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/lib/pq"
	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/memory"
	"github.com/wuxujun/ai-agent/internal/types"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// PostgresStore implements Store using a PostgreSQL database.
type PostgresStore struct {
	db              *sql.DB
	pgvectorMu      sync.Mutex
	pgvectorReady   bool
	pgvectorIdxDim  int
	memoryIndexGate memoryIndexGate
}

// NewPostgresStore creates and initializes a PostgresStore.
func NewPostgresStore(dsn string) (*PostgresStore, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}

	p := &PostgresStore{db: db}
	if err := p.init(); err != nil {
		db.Close()
		return nil, err
	}
	if usePostgresPGVector() {
		if err := p.ensurePGVector(context.Background()); err != nil {
			db.Close()
			return nil, err
		}
	}
	return p, nil
}

func (p *PostgresStore) init() error {
	schema := `
CREATE TABLE IF NOT EXISTS sessions (
	id VARCHAR(255) PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	title TEXT NOT NULL,
	status VARCHAR(50) NOT NULL,
	next_sequence BIGINT NOT NULL DEFAULT 0,
	created_at TIMESTAMP NOT NULL,
	updated_at TIMESTAMP NOT NULL
);

CREATE TABLE IF NOT EXISTS tasks (
	id VARCHAR(255) PRIMARY KEY,
	tenant_id TEXT NOT NULL DEFAULT '',
	session_id TEXT NOT NULL DEFAULT '',
	sequence_no BIGINT NOT NULL DEFAULT 0,
	created_at TIMESTAMP,
	updated_at TIMESTAMP,
	goal TEXT NOT NULL,
	status VARCHAR(50) NOT NULL,
	max_steps INT NOT NULL,
	step_count INT NOT NULL,
	workspace TEXT NOT NULL,
	hypothesis TEXT NOT NULL,
	unresolved_json TEXT NOT NULL,
	tool_budget INT NOT NULL,
	token_budget INT NOT NULL DEFAULT 0,
	llm_call_budget INT NOT NULL DEFAULT 0,
	llm_cost_budget_usd DOUBLE PRECISION NOT NULL DEFAULT 0,
	llm_calls INT NOT NULL DEFAULT 0,
	llm_estimated_cost_usd DOUBLE PRECISION NOT NULL DEFAULT 0,
	memories_json TEXT NOT NULL DEFAULT '[]',
	answer_audit_json JSONB NOT NULL DEFAULT '{}'::jsonb,
	final_answer TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS traces (
	id SERIAL PRIMARY KEY,
	task_id VARCHAR(255) NOT NULL,
	step INT NOT NULL,
	goal TEXT NOT NULL,
	action TEXT NOT NULL,
	query TEXT NOT NULL,
	observation TEXT NOT NULL,
		evidence_json TEXT NOT NULL,
		agent_role TEXT NOT NULL DEFAULT '',
		error_text TEXT NOT NULL DEFAULT '',
		prompt_tokens INT NOT NULL DEFAULT 0,
		completion_tokens INT NOT NULL DEFAULT 0,
		total_tokens INT NOT NULL DEFAULT 0,
		FOREIGN KEY(task_id) REFERENCES tasks(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS memories (
	id VARCHAR(255) PRIMARY KEY,
	tenant_id TEXT NOT NULL DEFAULT '',
	session_id TEXT NOT NULL DEFAULT '',
	task_id VARCHAR(255) NOT NULL,
	goal TEXT NOT NULL,
	final_answer TEXT NOT NULL,
	key_findings TEXT NOT NULL,
	timestamp TIMESTAMP NOT NULL,
	embedding_json TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS task_leases (
	task_id VARCHAR(255) PRIMARY KEY,
	owner TEXT NOT NULL,
	expires_at BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS tenant_llm_usage (
	tenant_id TEXT NOT NULL,
	period_start DATE NOT NULL,
	llm_calls INT NOT NULL DEFAULT 0,
	estimated_cost_usd DOUBLE PRECISION NOT NULL DEFAULT 0,
	PRIMARY KEY (tenant_id, period_start)
);
`
	_, err := p.db.Exec(schema)
	if err != nil {
		return err
	}
	// Idempotent migrations for existing databases that predate these columns.
	// Postgres supports IF NOT EXISTS on ADD COLUMN since 9.6.
	_, _ = p.db.Exec(`ALTER TABLE tasks ADD COLUMN IF NOT EXISTS token_budget INT NOT NULL DEFAULT 0`)
	_, _ = p.db.Exec(`ALTER TABLE tasks ADD COLUMN IF NOT EXISTS tenant_id TEXT NOT NULL DEFAULT ''`)
	_, _ = p.db.Exec(`ALTER TABLE tasks ADD COLUMN IF NOT EXISTS session_id TEXT NOT NULL DEFAULT ''`)
	_, _ = p.db.Exec(`ALTER TABLE tasks ADD COLUMN IF NOT EXISTS sequence_no BIGINT NOT NULL DEFAULT 0`)
	_, _ = p.db.Exec(`ALTER TABLE tasks ADD COLUMN IF NOT EXISTS created_at TIMESTAMP`)
	_, _ = p.db.Exec(`ALTER TABLE tasks ADD COLUMN IF NOT EXISTS updated_at TIMESTAMP`)
	_, _ = p.db.Exec(`UPDATE tasks SET created_at = CURRENT_TIMESTAMP WHERE created_at IS NULL`)
	_, _ = p.db.Exec(`UPDATE tasks SET updated_at = created_at WHERE updated_at IS NULL`)
	_, _ = p.db.Exec(`ALTER TABLE tasks ADD COLUMN IF NOT EXISTS llm_call_budget INT NOT NULL DEFAULT 0`)
	_, _ = p.db.Exec(`ALTER TABLE tasks ADD COLUMN IF NOT EXISTS llm_cost_budget_usd DOUBLE PRECISION NOT NULL DEFAULT 0`)
	_, _ = p.db.Exec(`ALTER TABLE tasks ADD COLUMN IF NOT EXISTS llm_calls INT NOT NULL DEFAULT 0`)
	_, _ = p.db.Exec(`ALTER TABLE tasks ADD COLUMN IF NOT EXISTS llm_estimated_cost_usd DOUBLE PRECISION NOT NULL DEFAULT 0`)
	_, _ = p.db.Exec(`ALTER TABLE tasks ADD COLUMN IF NOT EXISTS memories_json TEXT NOT NULL DEFAULT '[]'`)
	_, _ = p.db.Exec(`ALTER TABLE tasks ADD COLUMN IF NOT EXISTS answer_audit_json JSONB NOT NULL DEFAULT '{}'::jsonb`)
	_, _ = p.db.Exec(`ALTER TABLE memories ADD COLUMN IF NOT EXISTS tenant_id TEXT NOT NULL DEFAULT ''`)
	_, _ = p.db.Exec(`ALTER TABLE memories ADD COLUMN IF NOT EXISTS session_id TEXT NOT NULL DEFAULT ''`)
	_, _ = p.db.Exec(`ALTER TABLE traces ADD COLUMN IF NOT EXISTS error_text TEXT NOT NULL DEFAULT ''`)
	_, _ = p.db.Exec(`ALTER TABLE traces ADD COLUMN IF NOT EXISTS prompt_tokens INT NOT NULL DEFAULT 0`)
	_, _ = p.db.Exec(`ALTER TABLE traces ADD COLUMN IF NOT EXISTS completion_tokens INT NOT NULL DEFAULT 0`)
	_, _ = p.db.Exec(`ALTER TABLE traces ADD COLUMN IF NOT EXISTS total_tokens INT NOT NULL DEFAULT 0`)
	_, _ = p.db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_traces_task_id_step ON traces(task_id, step)`)
	_, _ = p.db.Exec(`CREATE INDEX IF NOT EXISTS idx_sessions_tenant_updated ON sessions(tenant_id, updated_at DESC)`)
	_, _ = p.db.Exec(`CREATE INDEX IF NOT EXISTS idx_tasks_session_sequence ON tasks(tenant_id, session_id, sequence_no)`)
	_, _ = p.db.Exec(`CREATE INDEX IF NOT EXISTS idx_memories_session_timestamp ON memories(tenant_id, session_id, timestamp DESC)`)
	return nil
}

func (p *PostgresStore) ReserveTenantLLMCall(ctx context.Context, tenantID string, periodStart time.Time, budget types.TenantLLMBudget) (types.TenantLLMUsage, bool, error) {
	period := periodStart.UTC().Format("2006-01-02")
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return types.TenantLLMUsage{}, false, err
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.ExecContext(ctx, `INSERT INTO tenant_llm_usage (tenant_id, period_start) VALUES ($1, $2) ON CONFLICT DO NOTHING`, tenantID, period); err != nil {
		return types.TenantLLMUsage{}, false, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE tenant_llm_usage SET llm_calls = llm_calls + 1 WHERE tenant_id = $1 AND period_start = $2 AND ($3 <= 0 OR llm_calls < $3) AND ($4 <= 0 OR estimated_cost_usd < $4)`, tenantID, period, budget.MaxCalls, budget.MaxEstimatedCostUSD)
	if err != nil {
		return types.TenantLLMUsage{}, false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return types.TenantLLMUsage{}, false, err
	}
	var usage types.TenantLLMUsage
	if err := tx.QueryRowContext(ctx, `SELECT llm_calls, estimated_cost_usd FROM tenant_llm_usage WHERE tenant_id = $1 AND period_start = $2`, tenantID, period).Scan(&usage.Calls, &usage.EstimatedCostUSD); err != nil {
		return types.TenantLLMUsage{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return types.TenantLLMUsage{}, false, err
	}
	return usage, rows == 1, nil
}

func (p *PostgresStore) AddTenantLLMCost(ctx context.Context, tenantID string, periodStart time.Time, estimatedCostUSD float64) error {
	if estimatedCostUSD <= 0 {
		return nil
	}
	period := periodStart.UTC().Format("2006-01-02")
	_, err := p.db.ExecContext(ctx, `INSERT INTO tenant_llm_usage (tenant_id, period_start, estimated_cost_usd) VALUES ($1, $2, $3) ON CONFLICT(tenant_id, period_start) DO UPDATE SET estimated_cost_usd = tenant_llm_usage.estimated_cost_usd + EXCLUDED.estimated_cost_usd`, tenantID, period, estimatedCostUSD)
	return err
}

func (p *PostgresStore) GetTenantLLMUsage(ctx context.Context, tenantID string, periodStart time.Time) (types.TenantLLMUsage, error) {
	period := periodStart.UTC().Format("2006-01-02")
	var usage types.TenantLLMUsage
	err := p.db.QueryRowContext(ctx, `SELECT llm_calls, estimated_cost_usd FROM tenant_llm_usage WHERE tenant_id = $1 AND period_start = $2`, tenantID, period).Scan(&usage.Calls, &usage.EstimatedCostUSD)
	if errors.Is(err, sql.ErrNoRows) {
		return types.TenantLLMUsage{}, nil
	}
	return usage, err
}

// Close closes the database connection.
func (p *PostgresStore) Close() error {
	return p.db.Close()
}

// SaveTask inserts or updates task metadata.
func (p *PostgresStore) SaveTask(ctx context.Context, task *types.Task) error {
	normalizeTaskTimestamps(task)
	unresolved, err := json.Marshal(task.Unresolved)
	if err != nil {
		return err
	}

	memoriesJSON, err := json.Marshal(memoriesForPersistence(task.Memories))
	if err != nil {
		return err
	}
	auditJSON, err := json.Marshal(task.AnswerAudit)
	if err != nil {
		return err
	}

	_, err = p.db.ExecContext(ctx, `
INSERT INTO tasks (id, tenant_id, session_id, sequence_no, created_at, updated_at, goal, status, max_steps, step_count, workspace, hypothesis, unresolved_json, tool_budget, token_budget, llm_call_budget, llm_cost_budget_usd, llm_calls, llm_estimated_cost_usd, memories_json, answer_audit_json, final_answer)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22)
ON CONFLICT(id) DO UPDATE SET
goal=EXCLUDED.goal,
tenant_id=EXCLUDED.tenant_id,
session_id=EXCLUDED.session_id,
sequence_no=EXCLUDED.sequence_no,
created_at=EXCLUDED.created_at,
updated_at=EXCLUDED.updated_at,
status=EXCLUDED.status,
max_steps=EXCLUDED.max_steps,
step_count=EXCLUDED.step_count,
workspace=EXCLUDED.workspace,
hypothesis=EXCLUDED.hypothesis,
unresolved_json=EXCLUDED.unresolved_json,
tool_budget=EXCLUDED.tool_budget,
token_budget=EXCLUDED.token_budget,
llm_call_budget=EXCLUDED.llm_call_budget,
llm_cost_budget_usd=EXCLUDED.llm_cost_budget_usd,
llm_calls=EXCLUDED.llm_calls,
llm_estimated_cost_usd=EXCLUDED.llm_estimated_cost_usd,
memories_json=EXCLUDED.memories_json,
answer_audit_json=EXCLUDED.answer_audit_json,
final_answer=EXCLUDED.final_answer
`,
		task.ID, task.TenantID, task.SessionID, task.SequenceNo, task.CreatedAt, task.UpdatedAt, task.Goal, string(task.Status), task.MaxSteps, task.StepCount,
		task.Workspace, task.Hypothesis, string(unresolved), task.ToolBudget, task.TokenBudget, task.LLMCallBudget, task.LLMCostBudgetUSD, task.LLMCalls, task.LLMEstimatedCostUSD, string(memoriesJSON), string(auditJSON), task.FinalAnswer,
	)
	return err
}

// ReplaceTraces persists step traces for a task using an append-only strategy to
// avoid write amplification.
//
// Strategy:
//  1. Query the highest step number already persisted in the DB (maxPersistedStep).
//  2. Only INSERT traces whose step > maxPersistedStep — historical traces are
//     never re-written, reducing per-save writes from O(N) to O(K) where K is the
//     number of new steps added since the last save (typically 1).
//  3. Handle the truncation/reset case (traces shortened) by deleting rows whose
//     step > len(traces) before inserting new ones.
//  4. INSERT ... ON CONFLICT DO NOTHING provides idempotency for retries.
//
// Overall complexity across a full task lifetime drops from O(N²) to O(N).
func (p *PostgresStore) ReplaceTraces(ctx context.Context, taskID string, traces []types.StepTrace) error {
	if len(traces) == 0 {
		// Nothing to write; clean up any orphaned rows from a prior reset.
		_, err := p.db.ExecContext(ctx, `DELETE FROM traces WHERE task_id = $1`, taskID)
		return err
	}

	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	// Step 1: Find the highest step already persisted for this task.
	var maxPersistedStep int
	row := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(step), 0) FROM traces WHERE task_id = $1`, taskID)
	if err := row.Scan(&maxPersistedStep); err != nil {
		return err
	}

	// Step 2: Handle truncation — delete rows beyond the new trace length.
	// This covers task-reset or step-rollback scenarios.
	if maxPersistedStep > len(traces) {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM traces WHERE task_id = $1 AND step > $2`, taskID, len(traces),
		); err != nil {
			return err
		}
		// Re-read maxPersistedStep after truncation so we don't skip re-inserting
		// rows that were just deleted (edge case: truncate then immediately append).
		maxPersistedStep = len(traces)
	}

	// Step 3: INSERT only the truly new traces (step > maxPersistedStep).
	// ON CONFLICT DO NOTHING makes concurrent or retry calls safe: a row that
	// already exists at (task_id, step) is silently skipped without error.
	for _, tr := range traces {
		if tr.Step <= maxPersistedStep {
			// Already persisted in a previous save — skip to avoid redundant writes.
			continue
		}
		ev, err := json.Marshal(tr.Evidence)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO traces
					(task_id, step, goal, action, query, observation, evidence_json, agent_role,
					 error_text, prompt_tokens, completion_tokens, total_tokens)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
				ON CONFLICT (task_id, step) DO NOTHING`,
			taskID, tr.Step, tr.Goal, tr.Action, tr.Query, tr.Observation, string(ev), string(tr.AgentRole),
			tr.Error, tr.TokenUsage.PromptTokens, tr.TokenUsage.CompletionTokens, tr.TokenUsage.TotalTokens,
		); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// SaveFullTask saves the task metadata and all associated traces inside a telemetry transaction.
func (p *PostgresStore) SaveFullTask(ctx context.Context, task *types.Task) error {
	normalizeTaskTimestamps(task)
	ctx, span := tracer.Start(ctx, "store.postgres.save_full_task")
	defer span.End()

	span.SetAttributes(
		attribute.String("agent.task.id", task.ID),
		attribute.Int("agent.task.trace_count", len(task.Trace)),
	)

	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "begin tx failed")
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	// 1. Save Task in transaction
	unresolved, err := json.Marshal(task.Unresolved)
	if err != nil {
		span.RecordError(err)
		return err
	}
	memoriesJSON, err := json.Marshal(memoriesForPersistence(task.Memories))
	if err != nil {
		span.RecordError(err)
		return err
	}
	auditJSON, err := json.Marshal(task.AnswerAudit)
	if err != nil {
		span.RecordError(err)
		return err
	}

	_, err = tx.ExecContext(ctx, `
INSERT INTO tasks (id, tenant_id, session_id, sequence_no, created_at, updated_at, goal, status, max_steps, step_count, workspace, hypothesis, unresolved_json, tool_budget, token_budget, llm_call_budget, llm_cost_budget_usd, llm_calls, llm_estimated_cost_usd, memories_json, answer_audit_json, final_answer)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22)
ON CONFLICT(id) DO UPDATE SET
goal=EXCLUDED.goal,
tenant_id=EXCLUDED.tenant_id,
session_id=EXCLUDED.session_id,
sequence_no=EXCLUDED.sequence_no,
created_at=EXCLUDED.created_at,
updated_at=EXCLUDED.updated_at,
status=EXCLUDED.status,
max_steps=EXCLUDED.max_steps,
step_count=EXCLUDED.step_count,
workspace=EXCLUDED.workspace,
hypothesis=EXCLUDED.hypothesis,
unresolved_json=EXCLUDED.unresolved_json,
tool_budget=EXCLUDED.tool_budget,
token_budget=EXCLUDED.token_budget,
llm_call_budget=EXCLUDED.llm_call_budget,
llm_cost_budget_usd=EXCLUDED.llm_cost_budget_usd,
llm_calls=EXCLUDED.llm_calls,
llm_estimated_cost_usd=EXCLUDED.llm_estimated_cost_usd,
memories_json=EXCLUDED.memories_json,
answer_audit_json=EXCLUDED.answer_audit_json,
final_answer=EXCLUDED.final_answer`,
		task.ID, task.TenantID, task.SessionID, task.SequenceNo, task.CreatedAt, task.UpdatedAt, task.Goal, task.Status, task.MaxSteps, task.StepCount,
		task.Workspace, task.Hypothesis, string(unresolved), task.ToolBudget, task.TokenBudget, task.LLMCallBudget, task.LLMCostBudgetUSD, task.LLMCalls, task.LLMEstimatedCostUSD, string(memoriesJSON), string(auditJSON), task.FinalAnswer,
	)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "save task in tx failed")
		return err
	}

	// 2. Replace Traces in same transaction
	if len(task.Trace) > 0 {
		var maxPersistedStep int
		row := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(step), 0) FROM traces WHERE task_id = $1`, task.ID)
		if err := row.Scan(&maxPersistedStep); err != nil {
			span.RecordError(err)
			return err
		}

		if maxPersistedStep > len(task.Trace) {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM traces WHERE task_id = $1 AND step > $2`, task.ID, len(task.Trace),
			); err != nil {
				span.RecordError(err)
				return err
			}
			maxPersistedStep = len(task.Trace)
		}

		for _, tr := range task.Trace {
			if tr.Step <= maxPersistedStep {
				continue
			}
			ev, err := json.Marshal(tr.Evidence)
			if err != nil {
				span.RecordError(err)
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO traces
						(task_id, step, goal, action, query, observation, evidence_json, agent_role,
						 error_text, prompt_tokens, completion_tokens, total_tokens)
					VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
					ON CONFLICT (task_id, step) DO NOTHING`,
				task.ID, tr.Step, tr.Goal, tr.Action, tr.Query, tr.Observation, string(ev), string(tr.AgentRole),
				tr.Error, tr.TokenUsage.PromptTokens, tr.TokenUsage.CompletionTokens, tr.TokenUsage.TotalTokens,
			); err != nil {
				span.RecordError(err)
				return err
			}
		}
	} else {
		// Clean up traces if empty
		if _, err := tx.ExecContext(ctx, `DELETE FROM traces WHERE task_id = $1`, task.ID); err != nil {
			span.RecordError(err)
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "commit tx failed")
		return err
	}

	// 3. Asynchronously index memory if task is completed
	if task.Status == types.StatusCompleted {
		memoryID := memory.TaskMemoryID(task)
		if !p.memoryIndexGate.tryStart(memoryID) {
			return nil
		}
		var exists int
		err := p.db.QueryRowContext(ctx, `SELECT 1 FROM memories WHERE id = $1`, memoryID).Scan(&exists)
		if err == sql.ErrNoRows {
			taskSnap := types.CloneTask(task)

			go func() {
				defer p.memoryIndexGate.done(memoryID)
				asyncCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
				defer cancel()
				mem, err := memory.CreateMemoryFromTask(asyncCtx, taskSnap)
				if err != nil {
					log.Warn("failed to create memory for task in postgres store", "task_id", taskSnap.ID, "error", err)
					return
				}
				if err := p.SaveMemory(asyncCtx, mem); err != nil {
					log.Warn("failed to save memory for task in postgres store", "task_id", taskSnap.ID, "error", err)
				}
			}()
		} else {
			p.memoryIndexGate.done(memoryID)
		}
	}

	return nil
}

// GetTask retrieves a task and its traces. Returns sql.ErrNoRows if not found.
func (p *PostgresStore) GetTask(ctx context.Context, id string) (*types.Task, error) {
	ctx, span := tracer.Start(ctx, "store.postgres.get_task")
	defer span.End()

	span.SetAttributes(attribute.String("agent.task.id", id))

	row := p.db.QueryRowContext(ctx, `
SELECT id, tenant_id, session_id, sequence_no, created_at, updated_at, goal, status, max_steps, step_count, workspace, hypothesis, unresolved_json, tool_budget, token_budget, llm_call_budget, llm_cost_budget_usd, llm_calls, llm_estimated_cost_usd, memories_json, answer_audit_json, final_answer
FROM tasks WHERE id = $1
`, id)

	var task types.Task
	var unresolvedJSON string
	var memoriesJSON string
	var auditJSON string

	err := row.Scan(
		&task.ID, &task.TenantID, &task.SessionID, &task.SequenceNo, &task.CreatedAt, &task.UpdatedAt, &task.Goal, &task.Status, &task.MaxSteps, &task.StepCount,
		&task.Workspace, &task.Hypothesis, &unresolvedJSON, &task.ToolBudget, &task.TokenBudget, &task.LLMCallBudget, &task.LLMCostBudgetUSD, &task.LLMCalls, &task.LLMEstimatedCostUSD, &memoriesJSON, &auditJSON, &task.FinalAnswer,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			span.SetAttributes(attribute.Bool("agent.store.found", false))
			return nil, sql.ErrNoRows
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "get task failed")
		return nil, err
	}
	span.SetAttributes(attribute.Bool("agent.store.found", true))

	if err := json.Unmarshal([]byte(unresolvedJSON), &task.Unresolved); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(memoriesJSON), &task.Memories); err != nil {
		return nil, err
	}
	if auditJSON != "" && auditJSON != "{}" && auditJSON != "null" {
		if err := json.Unmarshal([]byte(auditJSON), &task.AnswerAudit); err != nil {
			return nil, err
		}
	}

	rows, err := p.db.QueryContext(ctx, `
	SELECT step, goal, action, query, observation, evidence_json, agent_role,
	       error_text, prompt_tokens, completion_tokens, total_tokens
FROM traces
WHERE task_id = $1
ORDER BY step ASC, id ASC
`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var tr types.StepTrace
		var evidenceJSON, agentRole string
		if err := rows.Scan(
			&tr.Step, &tr.Goal, &tr.Action, &tr.Query, &tr.Observation, &evidenceJSON, &agentRole,
			&tr.Error, &tr.TokenUsage.PromptTokens, &tr.TokenUsage.CompletionTokens, &tr.TokenUsage.TotalTokens,
		); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(evidenceJSON), &tr.Evidence); err != nil {
			return nil, err
		}
		tr.AgentRole = types.AgentRole(agentRole)
		task.Trace = append(task.Trace, tr)
	}

	return &task, rows.Err()
}

// ListTasks returns tasks matching f, ordered by id ASC.
func (p *PostgresStore) ListTasks(ctx context.Context, f ListFilter) ([]*types.Task, error) {
	limit := resolveLimit(f.Limit, 50, 500)
	args := []any{}
	var conditions []string
	if f.TenantID != "" {
		args = append(args, f.TenantID)
		conditions = append(conditions, fmt.Sprintf("tenant_id = $%d", len(args)))
	}
	if f.Status != "" {
		args = append(args, string(f.Status))
		conditions = append(conditions, fmt.Sprintf("status = $%d", len(args)))
	}
	if f.SessionID != "" {
		args = append(args, f.SessionID)
		conditions = append(conditions, fmt.Sprintf("session_id = $%d", len(args)))
	}
	where := ""
	if len(conditions) > 0 {
		where = "WHERE " + strings.Join(conditions, " AND ")
	}
	offsetArg := len(args) + 1
	limitArg := len(args) + 2
	args = append(args, f.Offset, limit)

	orderBy := "id ASC"
	if f.SessionID != "" {
		orderBy = "sequence_no ASC, id ASC"
	}
	query := fmt.Sprintf(`
	SELECT id, tenant_id, session_id, sequence_no, created_at, updated_at, goal, status, max_steps, step_count, workspace, hypothesis, unresolved_json, tool_budget, token_budget, llm_call_budget, llm_cost_budget_usd, llm_calls, llm_estimated_cost_usd, memories_json, answer_audit_json, final_answer
FROM tasks
%s
ORDER BY %s
LIMIT $%d OFFSET $%d
`, where, orderBy, limitArg, offsetArg)

	rows, err := p.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tasks []*types.Task
	for rows.Next() {
		var t types.Task
		var unresolvedJSON string
		var memoriesJSON string
		var auditJSON string
		if err := rows.Scan(
			&t.ID, &t.TenantID, &t.SessionID, &t.SequenceNo, &t.CreatedAt, &t.UpdatedAt, &t.Goal, &t.Status, &t.MaxSteps, &t.StepCount,
			&t.Workspace, &t.Hypothesis, &unresolvedJSON, &t.ToolBudget, &t.TokenBudget, &t.LLMCallBudget, &t.LLMCostBudgetUSD, &t.LLMCalls, &t.LLMEstimatedCostUSD, &memoriesJSON, &auditJSON, &t.FinalAnswer,
		); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(unresolvedJSON), &t.Unresolved); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(memoriesJSON), &t.Memories); err != nil {
			return nil, err
		}
		if auditJSON != "" && auditJSON != "{}" && auditJSON != "null" {
			if err := json.Unmarshal([]byte(auditJSON), &t.AnswerAudit); err != nil {
				return nil, err
			}
		}
		tasks = append(tasks, &t)
	}
	return tasks, rows.Err()
}

// ExistsTask returns true if a task with the given id already exists.
func (p *PostgresStore) ExistsTask(ctx context.Context, id string) (bool, error) {
	var count int
	err := p.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM tasks WHERE id = $1`, id).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

func (p *PostgresStore) CreateSession(ctx context.Context, session *types.Session) error {
	now := time.Now().UTC()
	if session.Status == "" {
		session.Status = types.SessionStatusActive
	}
	if session.CreatedAt.IsZero() {
		session.CreatedAt = now
	}
	session.UpdatedAt = now
	_, err := p.db.ExecContext(ctx, `INSERT INTO sessions (id, tenant_id, title, status, next_sequence, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6, $7)`, session.ID, session.TenantID, session.Title, session.Status, session.NextSequence, session.CreatedAt, session.UpdatedAt)
	return err
}

func (p *PostgresStore) GetSession(ctx context.Context, id, tenantID string) (*types.Session, error) {
	var session types.Session
	err := p.db.QueryRowContext(ctx, `SELECT id, tenant_id, title, status, next_sequence, created_at, updated_at FROM sessions WHERE id = $1 AND tenant_id = $2`, id, tenantID).Scan(&session.ID, &session.TenantID, &session.Title, &session.Status, &session.NextSequence, &session.CreatedAt, &session.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSessionNotFound
	}
	return &session, err
}

func (p *PostgresStore) ListSessions(ctx context.Context, filter ListSessionFilter) ([]*types.Session, error) {
	query := `SELECT id, tenant_id, title, status, next_sequence, created_at, updated_at FROM sessions`
	args := []any{}
	conditions := []string{}
	if filter.TenantID != "" {
		args = append(args, filter.TenantID)
		conditions = append(conditions, fmt.Sprintf(`tenant_id = $%d`, len(args)))
	}
	if filter.Status != "" {
		args = append(args, filter.Status)
		conditions = append(conditions, fmt.Sprintf(`status = $%d`, len(args)))
	}
	if len(conditions) > 0 {
		query += ` WHERE ` + strings.Join(conditions, ` AND `)
	}
	args = append(args, resolveLimit(filter.Limit, 50, 500), filter.Offset)
	query += fmt.Sprintf(` ORDER BY updated_at DESC, id ASC LIMIT $%d OFFSET $%d`, len(args)-1, len(args))
	rows, err := p.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []*types.Session{}
	for rows.Next() {
		var session types.Session
		if err := rows.Scan(&session.ID, &session.TenantID, &session.Title, &session.Status, &session.NextSequence, &session.CreatedAt, &session.UpdatedAt); err != nil {
			return nil, err
		}
		items = append(items, &session)
	}
	return items, rows.Err()
}

func (p *PostgresStore) UpdateSession(ctx context.Context, session *types.Session) error {
	session.UpdatedAt = time.Now().UTC()
	result, err := p.db.ExecContext(ctx, `UPDATE sessions SET title = $1, status = $2, updated_at = $3 WHERE id = $4 AND tenant_id = $5`, session.Title, session.Status, session.UpdatedAt, session.ID, session.TenantID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrSessionNotFound
	}
	return nil
}

func (p *PostgresStore) NextSessionTaskSequence(ctx context.Context, id, tenantID string) (int64, error) {
	var sequence int64
	err := p.db.QueryRowContext(ctx, `UPDATE sessions SET next_sequence = next_sequence + 1, updated_at = $1 WHERE id = $2 AND tenant_id = $3 AND status = $4 RETURNING next_sequence`, time.Now().UTC(), id, tenantID, types.SessionStatusActive).Scan(&sequence)
	if err == nil {
		return sequence, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	var status types.SessionStatus
	err = p.db.QueryRowContext(ctx, `SELECT status FROM sessions WHERE id = $1 AND tenant_id = $2`, id, tenantID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrSessionNotFound
	}
	if err != nil {
		return 0, err
	}
	return 0, ErrSessionArchived
}

func (p *PostgresStore) DeleteTask(ctx context.Context, id string) (bool, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	for _, query := range []string{
		`DELETE FROM traces WHERE task_id = $1`,
		`DELETE FROM memories WHERE task_id = $1`,
		`DELETE FROM task_leases WHERE task_id = $1`,
	} {
		if _, err := tx.ExecContext(ctx, query, id); err != nil {
			return false, err
		}
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM tasks WHERE id = $1`, id)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return count > 0, nil
}

func (p *PostgresStore) DeleteAllTasks(ctx context.Context) (int64, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	for _, query := range []string{`DELETE FROM traces`, `DELETE FROM memories WHERE task_id IN (SELECT id FROM tasks)`, `DELETE FROM task_leases`} {
		if _, err := tx.ExecContext(ctx, query); err != nil {
			return 0, err
		}
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM tasks`)
	if err != nil {
		return 0, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}

// SaveMemory persists a memory entry to Postgres.
func (p *PostgresStore) SaveMemory(ctx context.Context, mem *types.Memory) error {
	embJSON, err := json.Marshal(mem.Embedding)
	if err != nil {
		return err
	}

	if usePostgresPGVector() {
		if err := p.ensurePGVector(ctx); err != nil {
			return err
		}
		vecValue := any(nil)
		if len(mem.Embedding) > 0 {
			vecValue = pgVectorLiteral(mem.Embedding)
		}
		_, err = p.db.ExecContext(ctx, `
INSERT INTO memories (id, tenant_id, session_id, task_id, goal, final_answer, key_findings, timestamp, embedding_json, embedding_vector)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::vector)
ON CONFLICT(id) DO UPDATE SET
task_id=EXCLUDED.task_id,
tenant_id=EXCLUDED.tenant_id,
session_id=EXCLUDED.session_id,
goal=EXCLUDED.goal,
final_answer=EXCLUDED.final_answer,
key_findings=EXCLUDED.key_findings,
timestamp=EXCLUDED.timestamp,
embedding_json=EXCLUDED.embedding_json,
embedding_vector=EXCLUDED.embedding_vector
`,
			mem.ID, mem.TenantID, mem.SessionID, mem.TaskID, mem.Goal, mem.FinalAnswer, mem.KeyFindings, mem.Timestamp, string(embJSON), vecValue,
		)
		return err
	}

	_, err = p.db.ExecContext(ctx, `
INSERT INTO memories (id, tenant_id, session_id, task_id, goal, final_answer, key_findings, timestamp, embedding_json)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT(id) DO UPDATE SET
task_id=EXCLUDED.task_id,
tenant_id=EXCLUDED.tenant_id,
session_id=EXCLUDED.session_id,
goal=EXCLUDED.goal,
final_answer=EXCLUDED.final_answer,
key_findings=EXCLUDED.key_findings,
timestamp=EXCLUDED.timestamp,
embedding_json=EXCLUDED.embedding_json
`,
		mem.ID, mem.TenantID, mem.SessionID, mem.TaskID, mem.Goal, mem.FinalAnswer, mem.KeyFindings, mem.Timestamp, string(embJSON),
	)
	return err
}

func (p *PostgresStore) ListMemories(ctx context.Context, filter ListMemoryFilter) ([]*types.Memory, error) {
	query := `SELECT id, tenant_id, session_id, task_id, goal, final_answer, key_findings, timestamp, embedding_json FROM memories`
	args := []any{}
	conditions := []string{}
	if filter.TenantID != "" {
		args = append(args, filter.TenantID)
		conditions = append(conditions, fmt.Sprintf(`(tenant_id = $%d OR ($%d = 'default' AND tenant_id = ''))`, len(args), len(args)))
	}
	if filter.SessionID != "" {
		args = append(args, filter.SessionID)
		conditions = append(conditions, fmt.Sprintf(`session_id = $%d`, len(args)))
	}
	if len(conditions) > 0 {
		query += ` WHERE ` + strings.Join(conditions, ` AND `)
	}
	args = append(args, resolveLimit(filter.Limit, 50, 500), filter.Offset)
	query += fmt.Sprintf(` ORDER BY timestamp DESC, id ASC LIMIT $%d OFFSET $%d`, len(args)-1, len(args))
	rows, err := p.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []*types.Memory
	for rows.Next() {
		var mem types.Memory
		var embeddingJSON string
		if err := rows.Scan(&mem.ID, &mem.TenantID, &mem.SessionID, &mem.TaskID, &mem.Goal, &mem.FinalAnswer, &mem.KeyFindings, &mem.Timestamp, &embeddingJSON); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(embeddingJSON), &mem.Embedding); err != nil {
			return nil, err
		}
		items = append(items, &mem)
	}
	return items, rows.Err()
}

func (p *PostgresStore) DeleteMemory(ctx context.Context, id, tenantID string) (bool, error) {
	query := `DELETE FROM memories WHERE id = $1`
	args := []any{id}
	if tenantID != "" {
		query += ` AND (tenant_id = $2 OR ($2 = 'default' AND tenant_id = ''))`
		args = append(args, tenantID)
	}
	result, err := p.db.ExecContext(ctx, query, args...)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count > 0, err
}

func (p *PostgresStore) DeleteAllMemories(ctx context.Context, tenantID string) (int64, error) {
	query := `DELETE FROM memories`
	args := []any{}
	if tenantID != "" {
		query += ` WHERE tenant_id = $1 OR ($1 = 'default' AND tenant_id = '')`
		args = append(args, tenantID)
	}
	result, err := p.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (p *PostgresStore) ensurePGVector(ctx context.Context) error {
	dim := configuredPGVectorDimension()

	p.pgvectorMu.Lock()
	defer p.pgvectorMu.Unlock()

	if p.pgvectorReady && p.pgvectorIdxDim == dim {
		return nil
	}

	if _, err := p.db.ExecContext(ctx, `CREATE EXTENSION IF NOT EXISTS vector`); err != nil {
		return fmt.Errorf("enable pgvector extension: %w", err)
	}
	if _, err := p.db.ExecContext(ctx, `ALTER TABLE memories ADD COLUMN IF NOT EXISTS embedding_vector vector`); err != nil {
		return fmt.Errorf("add pgvector memory column: %w", err)
	}
	if err := p.backfillPGVectorEmbeddings(ctx); err != nil {
		return err
	}
	if dim > 0 && p.pgvectorIdxDim != dim {
		indexSQL := fmt.Sprintf(`
CREATE INDEX IF NOT EXISTS idx_memories_embedding_vector_hnsw_%d
ON memories USING hnsw ((embedding_vector::vector(%d)) vector_cosine_ops)
WHERE embedding_vector IS NOT NULL AND vector_dims(embedding_vector) = %d
`, dim, dim, dim)
		if _, err := p.db.ExecContext(ctx, indexSQL); err != nil {
			log.Warn("failed to create pgvector HNSW index; exact pgvector scan remains enabled",
				"dimension", dim,
				"error", err,
			)
		}
	}

	p.pgvectorReady = true
	p.pgvectorIdxDim = dim
	return nil
}

func (p *PostgresStore) backfillPGVectorEmbeddings(ctx context.Context) error {
	rows, err := p.db.QueryContext(ctx, `
SELECT id, embedding_json
FROM memories
WHERE embedding_vector IS NULL
`)
	if err != nil {
		return fmt.Errorf("query pgvector backfill candidates: %w", err)
	}
	defer rows.Close()

	type candidate struct {
		id        string
		embedding []float32
	}
	var candidates []candidate
	for rows.Next() {
		var id, embJSON string
		if err := rows.Scan(&id, &embJSON); err != nil {
			return fmt.Errorf("scan pgvector backfill candidate: %w", err)
		}
		var embedding []float32
		if err := json.Unmarshal([]byte(embJSON), &embedding); err != nil {
			log.Warn("skipping malformed memory embedding during pgvector backfill", "id", id, "error", err)
			continue
		}
		if len(embedding) == 0 {
			continue
		}
		candidates = append(candidates, candidate{id: id, embedding: embedding})
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate pgvector backfill candidates: %w", err)
	}

	for _, c := range candidates {
		if _, err := p.db.ExecContext(ctx, `
UPDATE memories
SET embedding_vector = $1::vector
WHERE id = $2
`, pgVectorLiteral(c.embedding), c.id); err != nil {
			log.Warn("failed to backfill pgvector memory embedding", "id", c.id, "error", err)
		}
	}
	if len(candidates) > 0 {
		log.Info("pgvector memory backfill complete", "count", len(candidates))
	}
	return nil
}

func (p *PostgresStore) queryMemoriesPGVector(ctx context.Context, embedding []float32, limit int) ([]*types.Memory, error) {
	if limit <= 0 {
		return []*types.Memory{}, nil
	}
	if err := p.ensurePGVector(ctx); err != nil {
		return nil, err
	}

	dim := len(embedding)
	vectorValue := pgVectorLiteral(embedding)
	indexDim := configuredPGVectorDimension()

	args := []any{vectorValue}
	conditions := []string{`embedding_vector IS NOT NULL`}
	distance := `embedding_vector <=> $1::vector`
	if indexDim > 0 && dim == indexDim {
		conditions = append(conditions, fmt.Sprintf(`vector_dims(embedding_vector) = %d`, indexDim))
		distance = fmt.Sprintf(`(embedding_vector::vector(%d)) <=> $1::vector(%d)`, indexDim, indexDim)
	} else {
		args = append(args, dim)
		conditions = append(conditions, fmt.Sprintf(`vector_dims(embedding_vector) = $%d`, len(args)))
	}
	if scopedTenant, scoped := tenantScope(ctx); scoped {
		args = append(args, scopedTenant)
		conditions = append(conditions, fmt.Sprintf(`(tenant_id = $%d OR ($%d = 'default' AND tenant_id = ''))`, len(args), len(args)))
	}
	if scopedSession, scoped := sessionScope(ctx); scoped {
		args = append(args, scopedSession)
		conditions = append(conditions, fmt.Sprintf(`session_id = $%d`, len(args)))
	}
	args = append(args, limit)
	query := fmt.Sprintf(`
SELECT id, tenant_id, session_id, task_id, goal, final_answer, key_findings, timestamp, embedding_json
FROM memories
WHERE %s
ORDER BY %s
LIMIT $%d
`, strings.Join(conditions, ` AND `), distance, len(args))
	rows, err := p.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var res []*types.Memory
	for rows.Next() {
		var mem types.Memory
		var embJSON string
		if err := rows.Scan(&mem.ID, &mem.TenantID, &mem.SessionID, &mem.TaskID, &mem.Goal, &mem.FinalAnswer, &mem.KeyFindings, &mem.Timestamp, &embJSON); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(embJSON), &mem.Embedding); err != nil {
			continue
		}
		res = append(res, &mem)
	}
	return res, rows.Err()
}

func usePostgresPGVector() bool {
	cfg := config.Get()
	return cfg != nil && strings.EqualFold(cfg.Store.VectorSearch, "pgvector")
}

func configuredPGVectorDimension() int {
	if cfg := config.Get(); cfg != nil && cfg.Store.PGVectorDimensions > 0 {
		return cfg.Store.PGVectorDimensions
	}
	return 0
}

func pgVectorLiteral(embedding []float32) string {
	var b strings.Builder
	b.Grow(len(embedding) * 10)
	b.WriteByte('[')
	for i, v := range embedding {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(v), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}

// QueryMemories retrieves and ranks memories based on vector similarity or keyword match in Postgres.
// When store.vector_search is "pgvector" and an embedding is available, ranking
// is pushed down to PostgreSQL via pgvector cosine distance. Otherwise it falls
// back to the in-process candidate scan used by the SQLite backend.
func (p *PostgresStore) QueryMemories(ctx context.Context, query string, embedding []float32, limit int) ([]*types.Memory, error) {
	ctx, span := tracer.Start(ctx, "store.postgres.query_memories")
	defer span.End()

	span.SetAttributes(
		attribute.Bool("agent.query.has_embedding", len(embedding) > 0),
		attribute.Int("agent.query.embedding_dim", len(embedding)),
		attribute.Int("agent.query.limit", limit),
		attribute.String("agent.store.vector_search", config.Get().Store.VectorSearch),
	)

	decayRate := 0.0
	if cfg := config.Get(); cfg != nil {
		decayRate = cfg.Store.MemoryDecayRate
	}

	if usePostgresPGVector() && len(embedding) > 0 {
		fetchLimit := limit
		if decayRate > 0 {
			fetchLimit = resolveMemoryCandidateLimit()
		}
		mems, err := p.queryMemoriesPGVector(ctx, embedding, fetchLimit)
		if err == nil {
			if decayRate > 0 {
				now := time.Now()
				type rankResult struct {
					mem   *types.Memory
					score float32
				}
				var ranked []rankResult
				for _, mem := range mems {
					score := memory.CosineSimilarity(embedding, mem.Embedding)
					score = memory.ApplyTimeDecay(score, mem.Timestamp, now, decayRate)
					ranked = append(ranked, rankResult{mem: mem, score: score})
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

			span.SetAttributes(
				attribute.Bool("agent.store.pgvector_used", true),
				attribute.Int("agent.store.memory_count", len(mems)),
			)
			return mems, nil
		}
		span.RecordError(err)
		span.SetAttributes(attribute.Bool("agent.store.pgvector_fallback", true))
		log.Warn("pgvector memory query failed; falling back to in-process ranking", "error", err)
	}

	candidateLimit := resolveMemoryCandidateLimit()
	span.SetAttributes(
		attribute.Bool("agent.store.pgvector_used", false),
		attribute.Int("agent.store.memory_candidate_limit", candidateLimit),
	)
	querySQL := `SELECT id, tenant_id, session_id, task_id, goal, final_answer, key_findings, timestamp, embedding_json FROM memories`
	args := []any{}
	conditions := []string{}
	if scopedTenant, scoped := tenantScope(ctx); scoped {
		args = append(args, scopedTenant)
		conditions = append(conditions, fmt.Sprintf(`(tenant_id = $%d OR ($%d = 'default' AND tenant_id = ''))`, len(args), len(args)))
	}
	if scopedSession, scoped := sessionScope(ctx); scoped {
		args = append(args, scopedSession)
		conditions = append(conditions, fmt.Sprintf(`session_id = $%d`, len(args)))
	}
	if len(conditions) > 0 {
		querySQL += ` WHERE ` + strings.Join(conditions, ` AND `)
	}
	args = append(args, candidateLimit)
	querySQL += fmt.Sprintf(` ORDER BY timestamp DESC LIMIT $%d`, len(args))
	rows, err := p.db.QueryContext(ctx, querySQL, args...)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "query memories failed")
		return nil, err
	}
	defer rows.Close()

	now := time.Now()
	type rankResult struct {
		mem   *types.Memory
		score float32
	}
	var ranked []rankResult
	mismatchedEmbeddings := 0

	for rows.Next() {
		var mem types.Memory
		var embJSON string
		if err := rows.Scan(&mem.ID, &mem.TenantID, &mem.SessionID, &mem.TaskID, &mem.Goal, &mem.FinalAnswer, &mem.KeyFindings, &mem.Timestamp, &embJSON); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "scan memory failed")
			return nil, err
		}

		if err := json.Unmarshal([]byte(embJSON), &mem.Embedding); err != nil {
			continue
		}

		score, mismatch := memoryRelevanceScore(query, embedding, &mem)
		if mismatch {
			mismatchedEmbeddings++
		}
		score = memory.ApplyTimeDecay(score, mem.Timestamp, now, decayRate)
		ranked = append(ranked, rankResult{mem: &mem, score: score})
	}

	if len(ranked) >= candidateLimit {
		warnMemoryCandidateLimitReached("postgres", candidateLimit)
	}
	span.SetAttributes(attribute.Int("agent.store.embedding_dimension_mismatch_count", mismatchedEmbeddings))

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
	if err := rows.Err(); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "iterate memories failed")
		return nil, err
	}
	span.SetAttributes(
		attribute.Int("agent.store.memory_candidate_count", len(ranked)),
		attribute.Int("agent.store.memory_count", len(res)),
	)
	return res, nil
}

// TryTransitionTaskStatus atomically attempts to transition a task's status from one of the allowed 'from' statuses to a target status.
// It returns (true, nil) if the transition succeeded, or (false, nil) if the status did not match.
func (p *PostgresStore) TryTransitionTaskStatus(ctx context.Context, id string, from []types.TaskStatus, to types.TaskStatus) (bool, error) {
	if len(from) == 0 {
		return false, nil
	}
	placeholders := make([]string, len(from))
	args := make([]any, 0, len(from)+2)
	args = append(args, to, id)
	for i, f := range from {
		placeholders[i] = fmt.Sprintf("$%d", i+3)
		args = append(args, f)
	}
	query := fmt.Sprintf("UPDATE tasks SET status = $1 WHERE id = $2 AND status IN (%s)", strings.Join(placeholders, ","))

	res, err := p.db.ExecContext(ctx, query, args...)
	if err != nil {
		return false, err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if rows == 0 {
		var exists bool
		err = p.db.QueryRowContext(ctx, `SELECT 1 FROM tasks WHERE id = $1`, id).Scan(&exists)
		if err == sql.ErrNoRows {
			return false, sql.ErrNoRows
		}
		return false, nil
	}
	return true, nil
}

func (p *PostgresStore) AcquireTaskLease(ctx context.Context, id, owner string, ttl time.Duration) (bool, error) {
	if owner == "" || ttl <= 0 {
		return false, nil
	}
	now := time.Now().UnixNano()
	expiresAt := time.Now().Add(ttl).UnixNano()
	res, err := p.db.ExecContext(ctx, `
INSERT INTO task_leases (task_id, owner, expires_at)
VALUES ($1, $2, $3)
ON CONFLICT(task_id) DO UPDATE SET
	owner=EXCLUDED.owner,
	expires_at=EXCLUDED.expires_at
WHERE task_leases.expires_at <= $4 OR task_leases.owner = EXCLUDED.owner
`, id, owner, expiresAt, now)
	if err != nil {
		return false, err
	}
	rows, err := res.RowsAffected()
	return rows > 0, err
}

func (p *PostgresStore) ReleaseTaskLease(ctx context.Context, id, owner string) error {
	_, err := p.db.ExecContext(ctx, `DELETE FROM task_leases WHERE task_id = $1 AND owner = $2`, id, owner)
	return err
}
