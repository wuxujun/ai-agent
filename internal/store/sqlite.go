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

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/logger"
	"github.com/wuxujun/ai-agent/internal/memory"
	"github.com/wuxujun/ai-agent/internal/types"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	_ "modernc.org/sqlite"
)

var tracer = otel.Tracer("ai-agent/store")

var log = logger.Component("store")

type SQLiteStore struct {
	db              *sql.DB
	memoryIndexGate memoryIndexGate
}

func NewSQLiteStore(dsn string) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}

	// Optimize SQLite settings to prevent "database is locked (SQLITE_BUSY)" errors
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	// Set pragmas for extra robustness. A failed pragma means the connection is
	// not in the operating mode expected by the store, so fail startup instead
	// of silently continuing with weaker locking behavior.
	if _, err := db.Exec("PRAGMA journal_mode=WAL;"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite configure journal mode: %w", err)
	}
	if _, err := db.Exec("PRAGMA busy_timeout=5000;"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite configure busy timeout: %w", err)
	}

	s := &SQLiteStore{db: db}
	if err := s.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *SQLiteStore) init() error {
	schema := `
CREATE TABLE IF NOT EXISTS sessions (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	title TEXT NOT NULL,
	status TEXT NOT NULL,
	next_sequence INTEGER NOT NULL DEFAULT 0,
	created_at DATETIME NOT NULL,
	updated_at DATETIME NOT NULL
);

CREATE TABLE IF NOT EXISTS tasks (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL DEFAULT '',
	session_id TEXT NOT NULL DEFAULT '',
	sequence_no INTEGER NOT NULL DEFAULT 0,
	created_at DATETIME,
	updated_at DATETIME,
	goal TEXT NOT NULL,
	status TEXT NOT NULL,
	execution_mode TEXT NOT NULL DEFAULT '',
	max_steps INTEGER NOT NULL,
	step_count INTEGER NOT NULL,
	workspace TEXT NOT NULL,
	hypothesis TEXT NOT NULL,
	unresolved_json TEXT NOT NULL,
	tool_budget INTEGER NOT NULL,
	token_budget INTEGER NOT NULL DEFAULT 0,
	llm_call_budget INTEGER NOT NULL DEFAULT 0,
	llm_cost_budget_usd REAL NOT NULL DEFAULT 0,
	llm_calls INTEGER NOT NULL DEFAULT 0,
	llm_estimated_cost_usd REAL NOT NULL DEFAULT 0,
	memories_json TEXT NOT NULL DEFAULT '[]',
	answer_audit_json TEXT NOT NULL DEFAULT '{}',
	final_answer TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS traces (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	task_id TEXT NOT NULL,
	step INTEGER NOT NULL,
	goal TEXT NOT NULL,
	action TEXT NOT NULL,
	query TEXT NOT NULL,
		observation TEXT NOT NULL,
		evidence_json TEXT NOT NULL,
		error_text TEXT NOT NULL DEFAULT '',
		prompt_tokens INTEGER NOT NULL DEFAULT 0,
		completion_tokens INTEGER NOT NULL DEFAULT 0,
		total_tokens INTEGER NOT NULL DEFAULT 0,
		FOREIGN KEY(task_id) REFERENCES tasks(id)
);

CREATE TABLE IF NOT EXISTS memories (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL DEFAULT '',
	session_id TEXT NOT NULL DEFAULT '',
	task_id TEXT NOT NULL,
	goal TEXT NOT NULL,
	final_answer TEXT NOT NULL,
	key_findings TEXT NOT NULL,
	timestamp DATETIME NOT NULL,
	embedding_json TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS task_leases (
	task_id TEXT PRIMARY KEY,
	owner TEXT NOT NULL,
	expires_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS tenant_llm_usage (
	tenant_id TEXT NOT NULL,
	period_start TEXT NOT NULL,
	llm_calls INTEGER NOT NULL DEFAULT 0,
	estimated_cost_usd REAL NOT NULL DEFAULT 0,
	PRIMARY KEY (tenant_id, period_start)
);
`
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("sqlite begin schema migration: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.Exec(schema); err != nil {
		return fmt.Errorf("sqlite create schema: %w", err)
	}

	columns := []sqliteColumnMigration{
		{table: "traces", column: "agent_role", definition: "TEXT NOT NULL DEFAULT ''"},
		{table: "traces", column: "error_text", definition: "TEXT NOT NULL DEFAULT ''"},
		{table: "traces", column: "prompt_tokens", definition: "INTEGER NOT NULL DEFAULT 0"},
		{table: "traces", column: "completion_tokens", definition: "INTEGER NOT NULL DEFAULT 0"},
		{table: "traces", column: "total_tokens", definition: "INTEGER NOT NULL DEFAULT 0"},
		{table: "tasks", column: "token_budget", definition: "INTEGER NOT NULL DEFAULT 0"},
		{table: "tasks", column: "tenant_id", definition: "TEXT NOT NULL DEFAULT ''"},
		{table: "tasks", column: "session_id", definition: "TEXT NOT NULL DEFAULT ''"},
		{table: "tasks", column: "sequence_no", definition: "INTEGER NOT NULL DEFAULT 0"},
		{table: "tasks", column: "created_at", definition: "DATETIME"},
		{table: "tasks", column: "updated_at", definition: "DATETIME"},
		{table: "tasks", column: "execution_mode", definition: "TEXT NOT NULL DEFAULT ''"},
		{table: "tasks", column: "llm_call_budget", definition: "INTEGER NOT NULL DEFAULT 0"},
		{table: "tasks", column: "llm_cost_budget_usd", definition: "REAL NOT NULL DEFAULT 0"},
		{table: "tasks", column: "llm_calls", definition: "INTEGER NOT NULL DEFAULT 0"},
		{table: "tasks", column: "llm_estimated_cost_usd", definition: "REAL NOT NULL DEFAULT 0"},
		{table: "tasks", column: "memories_json", definition: "TEXT NOT NULL DEFAULT '[]'"},
		{table: "tasks", column: "answer_audit_json", definition: "TEXT NOT NULL DEFAULT '{}'"},
		{table: "memories", column: "tenant_id", definition: "TEXT NOT NULL DEFAULT ''"},
		{table: "memories", column: "session_id", definition: "TEXT NOT NULL DEFAULT ''"},
	}
	for _, migration := range columns {
		if err := ensureSQLiteColumn(tx, migration); err != nil {
			return err
		}
	}

	statements := []sqliteMigrationStatement{
		{name: "backfill task created_at", query: `UPDATE tasks SET created_at = CURRENT_TIMESTAMP WHERE created_at IS NULL`},
		{name: "backfill task updated_at", query: `UPDATE tasks SET updated_at = created_at WHERE updated_at IS NULL`},
		{name: "create trace step index", query: `CREATE UNIQUE INDEX IF NOT EXISTS idx_traces_task_id_step ON traces(task_id, step)`},
		{name: "create session tenant index", query: `CREATE INDEX IF NOT EXISTS idx_sessions_tenant_updated ON sessions(tenant_id, updated_at DESC)`},
		{name: "create task session index", query: `CREATE INDEX IF NOT EXISTS idx_tasks_session_sequence ON tasks(tenant_id, session_id, sequence_no)`},
		{name: "create memory session index", query: `CREATE INDEX IF NOT EXISTS idx_memories_session_timestamp ON memories(tenant_id, session_id, timestamp DESC)`},
		{name: "create memory timestamp index", query: `CREATE INDEX IF NOT EXISTS idx_memories_timestamp ON memories(timestamp DESC)`},
	}
	for _, statement := range statements {
		if _, err := tx.Exec(statement.query); err != nil {
			return fmt.Errorf("sqlite migration %s: %w", statement.name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite commit schema migration: %w", err)
	}
	return nil
}

type sqliteColumnMigration struct {
	table      string
	column     string
	definition string
}

type sqliteMigrationStatement struct {
	name  string
	query string
}

func ensureSQLiteColumn(tx *sql.Tx, migration sqliteColumnMigration) error {
	rows, err := tx.Query(`PRAGMA table_info(` + quoteSQLiteIdentifier(migration.table) + `)`)
	if err != nil {
		return fmt.Errorf("sqlite inspect %s.%s: %w", migration.table, migration.column, err)
	}
	found := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, dataType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
			_ = rows.Close()
			return fmt.Errorf("sqlite inspect %s.%s: %w", migration.table, migration.column, err)
		}
		if name == migration.column {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("sqlite inspect %s.%s: %w", migration.table, migration.column, err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("sqlite inspect %s.%s: %w", migration.table, migration.column, err)
	}
	if found {
		return nil
	}
	query := `ALTER TABLE ` + quoteSQLiteIdentifier(migration.table) + ` ADD COLUMN ` + quoteSQLiteIdentifier(migration.column) + ` ` + migration.definition
	if _, err := tx.Exec(query); err != nil {
		return fmt.Errorf("sqlite add column %s.%s: %w", migration.table, migration.column, err)
	}
	return nil
}

func quoteSQLiteIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func (s *SQLiteStore) ReserveTenantLLMCall(ctx context.Context, tenantID string, periodStart time.Time, budget types.TenantLLMBudget) (types.TenantLLMUsage, bool, error) {
	period := periodStart.UTC().Format("2006-01-02")
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return types.TenantLLMUsage{}, false, err
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO tenant_llm_usage (tenant_id, period_start) VALUES (?, ?)`, tenantID, period); err != nil {
		return types.TenantLLMUsage{}, false, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE tenant_llm_usage SET llm_calls = llm_calls + 1 WHERE tenant_id = ? AND period_start = ? AND (? <= 0 OR llm_calls < ?) AND (? <= 0 OR estimated_cost_usd < ?)`, tenantID, period, budget.MaxCalls, budget.MaxCalls, budget.MaxEstimatedCostUSD, budget.MaxEstimatedCostUSD)
	if err != nil {
		return types.TenantLLMUsage{}, false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return types.TenantLLMUsage{}, false, err
	}
	var usage types.TenantLLMUsage
	if err := tx.QueryRowContext(ctx, `SELECT llm_calls, estimated_cost_usd FROM tenant_llm_usage WHERE tenant_id = ? AND period_start = ?`, tenantID, period).Scan(&usage.Calls, &usage.EstimatedCostUSD); err != nil {
		return types.TenantLLMUsage{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return types.TenantLLMUsage{}, false, err
	}
	return usage, rows == 1, nil
}

func (s *SQLiteStore) AddTenantLLMCost(ctx context.Context, tenantID string, periodStart time.Time, estimatedCostUSD float64) error {
	if estimatedCostUSD <= 0 {
		return nil
	}
	period := periodStart.UTC().Format("2006-01-02")
	_, err := s.db.ExecContext(ctx, `INSERT INTO tenant_llm_usage (tenant_id, period_start, estimated_cost_usd) VALUES (?, ?, ?) ON CONFLICT(tenant_id, period_start) DO UPDATE SET estimated_cost_usd = estimated_cost_usd + excluded.estimated_cost_usd`, tenantID, period, estimatedCostUSD)
	return err
}

func (s *SQLiteStore) GetTenantLLMUsage(ctx context.Context, tenantID string, periodStart time.Time) (types.TenantLLMUsage, error) {
	period := periodStart.UTC().Format("2006-01-02")
	var usage types.TenantLLMUsage
	err := s.db.QueryRowContext(ctx, `SELECT llm_calls, estimated_cost_usd FROM tenant_llm_usage WHERE tenant_id = ? AND period_start = ?`, tenantID, period).Scan(&usage.Calls, &usage.EstimatedCostUSD)
	if errors.Is(err, sql.ErrNoRows) {
		return types.TenantLLMUsage{}, nil
	}
	return usage, err
}

func (s *SQLiteStore) Close() error {
	return s.db.Close()
}

func (s *SQLiteStore) SaveTask(ctx context.Context, task *types.Task) error {
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

	_, err = s.db.ExecContext(ctx, `
INSERT INTO tasks (id, tenant_id, session_id, sequence_no, created_at, updated_at, goal, status, execution_mode, max_steps, step_count, workspace, hypothesis, unresolved_json, tool_budget, token_budget, llm_call_budget, llm_cost_budget_usd, llm_calls, llm_estimated_cost_usd, memories_json, answer_audit_json, final_answer)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
goal=excluded.goal,
tenant_id=excluded.tenant_id,
session_id=excluded.session_id,
sequence_no=excluded.sequence_no,
created_at=excluded.created_at,
updated_at=excluded.updated_at,
status=excluded.status,
execution_mode=excluded.execution_mode,
max_steps=excluded.max_steps,
step_count=excluded.step_count,
workspace=excluded.workspace,
hypothesis=excluded.hypothesis,
unresolved_json=excluded.unresolved_json,
tool_budget=excluded.tool_budget,
token_budget=excluded.token_budget,
llm_call_budget=excluded.llm_call_budget,
llm_cost_budget_usd=excluded.llm_cost_budget_usd,
llm_calls=excluded.llm_calls,
llm_estimated_cost_usd=excluded.llm_estimated_cost_usd,
memories_json=excluded.memories_json,
answer_audit_json=excluded.answer_audit_json,
final_answer=excluded.final_answer
`,
		task.ID, task.TenantID, task.SessionID, task.SequenceNo, task.CreatedAt, task.UpdatedAt, task.Goal, task.Status, task.Mode, task.MaxSteps, task.StepCount,
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
//  4. INSERT OR IGNORE provides idempotency: a retry of the same trace set is safe.
//
// Overall complexity across a full task lifetime drops from O(N²) to O(N).
func (s *SQLiteStore) ReplaceTraces(ctx context.Context, taskID string, traces []types.StepTrace) error {
	if len(traces) == 0 {
		// Nothing to write; clean up any orphaned rows from a prior reset.
		_, err := s.db.ExecContext(ctx, `DELETE FROM traces WHERE task_id = ?`, taskID)
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	// Step 1: Find the highest step already persisted for this task.
	var maxPersistedStep int
	row := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(step), -1) FROM traces WHERE task_id = ?`, taskID)
	if err := row.Scan(&maxPersistedStep); err != nil {
		return err
	}

	// Step 2: Handle truncation — delete rows beyond the new trace length.
	// This covers task-reset or step-rollback scenarios.
	if maxPersistedStep > len(traces) {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM traces WHERE task_id = ? AND step > ?`, taskID, len(traces),
		); err != nil {
			return err
		}
		// Re-read maxPersistedStep after truncation so we don't skip re-inserting
		// rows that were just deleted (edge case: truncate then immediately append).
		maxPersistedStep = len(traces)
	}

	// Step 3: INSERT only the truly new traces (step > maxPersistedStep).
	// INSERT OR IGNORE makes concurrent or retry calls safe: a row that already
	// exists at (task_id, step) is silently skipped without error.
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
			`INSERT OR IGNORE INTO traces
					(task_id, step, goal, action, query, observation, evidence_json, agent_role,
					 error_text, prompt_tokens, completion_tokens, total_tokens)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			taskID, tr.Step, tr.Goal, tr.Action, tr.Query, tr.Observation, string(ev), string(tr.AgentRole),
			tr.Error, tr.TokenUsage.PromptTokens, tr.TokenUsage.CompletionTokens, tr.TokenUsage.TotalTokens,
		); err != nil {
			return err
		}
	}

	return tx.Commit()
}

func (s *SQLiteStore) SaveFullTask(ctx context.Context, task *types.Task) error {
	normalizeTaskTimestamps(task)
	ctx, span := tracer.Start(ctx, "store.save_full_task")
	defer span.End()

	span.SetAttributes(
		attribute.String("agent.task.id", task.ID),
		attribute.Int("agent.task.trace_count", len(task.Trace)),
	)

	tx, err := s.db.BeginTx(ctx, nil)
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
INSERT INTO tasks (id, tenant_id, session_id, sequence_no, created_at, updated_at, goal, status, execution_mode, max_steps, step_count, workspace, hypothesis, unresolved_json, tool_budget, token_budget, llm_call_budget, llm_cost_budget_usd, llm_calls, llm_estimated_cost_usd, memories_json, answer_audit_json, final_answer)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
goal=excluded.goal,
tenant_id=excluded.tenant_id,
session_id=excluded.session_id,
sequence_no=excluded.sequence_no,
created_at=excluded.created_at,
updated_at=excluded.updated_at,
status=excluded.status,
execution_mode=excluded.execution_mode,
max_steps=excluded.max_steps,
step_count=excluded.step_count,
workspace=excluded.workspace,
hypothesis=excluded.hypothesis,
unresolved_json=excluded.unresolved_json,
tool_budget=excluded.tool_budget,
token_budget=excluded.token_budget,
llm_call_budget=excluded.llm_call_budget,
llm_cost_budget_usd=excluded.llm_cost_budget_usd,
llm_calls=excluded.llm_calls,
llm_estimated_cost_usd=excluded.llm_estimated_cost_usd,
memories_json=excluded.memories_json,
answer_audit_json=excluded.answer_audit_json,
final_answer=excluded.final_answer
`,
		task.ID, task.TenantID, task.SessionID, task.SequenceNo, task.CreatedAt, task.UpdatedAt, task.Goal, task.Status, task.Mode, task.MaxSteps, task.StepCount,
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
		row := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(step), -1) FROM traces WHERE task_id = ?`, task.ID)
		if err := row.Scan(&maxPersistedStep); err != nil {
			span.RecordError(err)
			return err
		}

		if maxPersistedStep > len(task.Trace) {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM traces WHERE task_id = ? AND step > ?`, task.ID, len(task.Trace),
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
				`INSERT OR IGNORE INTO traces
						(task_id, step, goal, action, query, observation, evidence_json, agent_role,
						 error_text, prompt_tokens, completion_tokens, total_tokens)
					VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				task.ID, tr.Step, tr.Goal, tr.Action, tr.Query, tr.Observation, string(ev), string(tr.AgentRole),
				tr.Error, tr.TokenUsage.PromptTokens, tr.TokenUsage.CompletionTokens, tr.TokenUsage.TotalTokens,
			); err != nil {
				span.RecordError(err)
				return err
			}
		}
	} else {
		// Clean up traces if empty
		if _, err := tx.ExecContext(ctx, `DELETE FROM traces WHERE task_id = ?`, task.ID); err != nil {
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
	if memory.ShouldIndexTask(task) {
		memoryID := memory.TaskMemoryID(task)
		if !s.memoryIndexGate.tryStart(memoryID) {
			return nil
		}
		var exists int
		err := s.db.QueryRowContext(ctx, `SELECT 1 FROM memories WHERE id = ?`, memoryID).Scan(&exists)
		if err == sql.ErrNoRows {
			taskSnap := types.CloneTask(task)

			go func() {
				defer s.memoryIndexGate.done(memoryID)
				asyncCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
				defer cancel()
				mem, err := memory.CreateMemoryFromTask(asyncCtx, taskSnap)
				if err != nil {
					log.Warn("failed to create memory for task", "task_id", taskSnap.ID, "error", err)
					return
				}
				if err := s.SaveMemory(asyncCtx, mem); err != nil {
					log.Warn("failed to save memory for task", "task_id", taskSnap.ID, "error", err)
				}
			}()
		} else {
			s.memoryIndexGate.done(memoryID)
		}
	}

	return nil
}

func (s *SQLiteStore) GetTask(ctx context.Context, id string) (*types.Task, error) {
	ctx, span := tracer.Start(ctx, "store.get_task")
	defer span.End()

	span.SetAttributes(attribute.String("agent.task.id", id))

	row := s.db.QueryRowContext(ctx, `
SELECT id, tenant_id, session_id, sequence_no, created_at, updated_at, goal, status, execution_mode, max_steps, step_count, workspace, hypothesis, unresolved_json, tool_budget, token_budget, llm_call_budget, llm_cost_budget_usd, llm_calls, llm_estimated_cost_usd, memories_json, answer_audit_json, final_answer
FROM tasks WHERE id = ?
`, id)

	var task types.Task
	var unresolvedJSON string
	var memoriesJSON string
	var auditJSON string

	err := row.Scan(
		&task.ID, &task.TenantID, &task.SessionID, &task.SequenceNo, &task.CreatedAt, &task.UpdatedAt, &task.Goal, &task.Status, &task.Mode, &task.MaxSteps, &task.StepCount,
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

	rows, err := s.db.QueryContext(ctx, `
	SELECT step, goal, action, query, observation, evidence_json, agent_role,
	       error_text, prompt_tokens, completion_tokens, total_tokens
FROM traces
WHERE task_id = ?
ORDER BY step ASC, id ASC
`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var tr types.StepTrace
		var evidenceJSON string
		var agentRole string
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

// ListTasks returns tasks matching f (without trace rows) ordered by id ASC.
// It supports status filtering and cursor-style pagination via f.Limit and f.Offset.
func (s *SQLiteStore) ListTasks(ctx context.Context, f ListFilter) ([]*types.Task, error) {
	ctx, span := tracer.Start(ctx, "store.list_tasks")
	defer span.End()

	limit := resolveLimit(f.Limit, 50, 500)
	span.SetAttributes(
		attribute.String("agent.store.filter_status", string(f.Status)),
		attribute.Int("agent.store.limit", limit),
		attribute.Int("agent.store.offset", f.Offset),
	)

	// Build query dynamically so we only add a WHERE clause when needed.
	// Using a fixed column list avoids SELECT * surprises on schema changes.
	const base = `
	SELECT id, tenant_id, session_id, sequence_no, created_at, updated_at, goal, status, execution_mode, max_steps, step_count, workspace, hypothesis, unresolved_json, tool_budget, token_budget, llm_call_budget, llm_cost_budget_usd, llm_calls, llm_estimated_cost_usd, memories_json, answer_audit_json, final_answer
FROM tasks`

	var (
		query string
		args  []any
	)
	var conditions []string
	if f.TenantID != "" {
		conditions = append(conditions, "tenant_id = ?")
		args = append(args, f.TenantID)
	}
	if f.Status != "" {
		conditions = append(conditions, "status = ?")
		args = append(args, string(f.Status))
	}
	if f.SessionID != "" {
		conditions = append(conditions, "session_id = ?")
		args = append(args, f.SessionID)
	}
	query = base
	if len(conditions) > 0 {
		query += "\nWHERE " + strings.Join(conditions, " AND ")
	}
	if f.SessionID != "" {
		query += "\nORDER BY sequence_no ASC, id ASC\nLIMIT ? OFFSET ?"
	} else {
		query += "\nORDER BY id ASC\nLIMIT ? OFFSET ?"
	}
	args = append(args, limit, f.Offset)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		span.RecordError(err)
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
			&t.ID, &t.TenantID, &t.SessionID, &t.SequenceNo, &t.CreatedAt, &t.UpdatedAt, &t.Goal, &t.Status, &t.Mode, &t.MaxSteps, &t.StepCount,
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
	span.SetAttributes(attribute.Int("agent.store.task_count", len(tasks)))
	return tasks, rows.Err()
}

// ExistsTask returns true if a task with the given id already exists in the store.
func (s *SQLiteStore) ExistsTask(ctx context.Context, id string) (bool, error) {
	ctx, span := tracer.Start(ctx, "store.exists_task")
	defer span.End()
	span.SetAttributes(attribute.String("agent.task.id", id))

	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM tasks WHERE id = ?`, id).Scan(&count)
	if err != nil {
		span.RecordError(err)
		return false, err
	}
	return count > 0, nil
}

func (s *SQLiteStore) CreateSession(ctx context.Context, session *types.Session) error {
	now := time.Now().UTC()
	if session.Status == "" {
		session.Status = types.SessionStatusActive
	}
	if session.CreatedAt.IsZero() {
		session.CreatedAt = now
	}
	session.UpdatedAt = now
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions (id, tenant_id, title, status, next_sequence, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`, session.ID, session.TenantID, session.Title, session.Status, session.NextSequence, session.CreatedAt, session.UpdatedAt)
	return err
}

func (s *SQLiteStore) GetSession(ctx context.Context, id, tenantID string) (*types.Session, error) {
	var session types.Session
	err := s.db.QueryRowContext(ctx, `SELECT id, tenant_id, title, status, next_sequence, created_at, updated_at FROM sessions WHERE id = ? AND tenant_id = ?`, id, tenantID).Scan(&session.ID, &session.TenantID, &session.Title, &session.Status, &session.NextSequence, &session.CreatedAt, &session.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSessionNotFound
	}
	return &session, err
}

func (s *SQLiteStore) ListSessions(ctx context.Context, filter ListSessionFilter) ([]*types.Session, error) {
	query := `SELECT id, tenant_id, title, status, next_sequence, created_at, updated_at FROM sessions`
	args := []any{}
	conditions := []string{}
	if filter.TenantID != "" {
		conditions = append(conditions, `tenant_id = ?`)
		args = append(args, filter.TenantID)
	}
	if filter.Status != "" {
		conditions = append(conditions, `status = ?`)
		args = append(args, filter.Status)
	}
	if len(conditions) > 0 {
		query += ` WHERE ` + strings.Join(conditions, ` AND `)
	}
	query += ` ORDER BY updated_at DESC, id ASC LIMIT ? OFFSET ?`
	args = append(args, resolveLimit(filter.Limit, 50, 500), filter.Offset)
	rows, err := s.db.QueryContext(ctx, query, args...)
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

func (s *SQLiteStore) UpdateSession(ctx context.Context, session *types.Session) error {
	session.UpdatedAt = time.Now().UTC()
	result, err := s.db.ExecContext(ctx, `UPDATE sessions SET title = ?, status = ?, updated_at = ? WHERE id = ? AND tenant_id = ?`, session.Title, session.Status, session.UpdatedAt, session.ID, session.TenantID)
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

func (s *SQLiteStore) NextSessionTaskSequence(ctx context.Context, id, tenantID string) (int64, error) {
	var sequence int64
	err := s.db.QueryRowContext(ctx, `UPDATE sessions SET next_sequence = next_sequence + 1, updated_at = ? WHERE id = ? AND tenant_id = ? AND status = ? RETURNING next_sequence`, time.Now().UTC(), id, tenantID, types.SessionStatusActive).Scan(&sequence)
	if err == nil {
		return sequence, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	var status types.SessionStatus
	err = s.db.QueryRowContext(ctx, `SELECT status FROM sessions WHERE id = ? AND tenant_id = ?`, id, tenantID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrSessionNotFound
	}
	if err != nil {
		return 0, err
	}
	return 0, ErrSessionArchived
}

func (s *SQLiteStore) DeleteTask(ctx context.Context, id string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	for _, query := range []string{
		`DELETE FROM traces WHERE task_id = ?`,
		`DELETE FROM memories WHERE task_id = ?`,
		`DELETE FROM task_leases WHERE task_id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, query, id); err != nil {
			return false, err
		}
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM tasks WHERE id = ?`, id)
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

func (s *SQLiteStore) DeleteAllTasks(ctx context.Context) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
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

// SaveMemory persists a memory entry to SQLite.
func (s *SQLiteStore) SaveMemory(ctx context.Context, mem *types.Memory) error {
	embJSON, err := json.Marshal(mem.Embedding)
	if err != nil {
		return err
	}

	_, err = s.db.ExecContext(ctx, `
INSERT INTO memories (id, tenant_id, session_id, task_id, goal, final_answer, key_findings, timestamp, embedding_json)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
task_id=excluded.task_id,
tenant_id=excluded.tenant_id,
session_id=excluded.session_id,
goal=excluded.goal,
final_answer=excluded.final_answer,
key_findings=excluded.key_findings,
timestamp=excluded.timestamp,
embedding_json=excluded.embedding_json
`,
		mem.ID, mem.TenantID, mem.SessionID, mem.TaskID, mem.Goal, mem.FinalAnswer, mem.KeyFindings, mem.Timestamp.UTC(), string(embJSON),
	)
	return err
}

func (s *SQLiteStore) ListMemories(ctx context.Context, filter ListMemoryFilter) ([]*types.Memory, error) {
	query := `SELECT id, tenant_id, session_id, task_id, goal, final_answer, key_findings, timestamp, embedding_json FROM memories`
	args := []any{}
	conditions := []string{}
	if filter.TenantID != "" {
		conditions = append(conditions, `(tenant_id = ? OR (? = 'default' AND tenant_id = ''))`)
		args = append(args, filter.TenantID, filter.TenantID)
	}
	if filter.SessionID != "" {
		conditions = append(conditions, `session_id = ?`)
		args = append(args, filter.SessionID)
	}
	if len(conditions) > 0 {
		query += ` WHERE ` + strings.Join(conditions, ` AND `)
	}
	query += ` ORDER BY timestamp DESC, id ASC LIMIT ? OFFSET ?`
	args = append(args, resolveLimit(filter.Limit, 50, 500), filter.Offset)
	rows, err := s.db.QueryContext(ctx, query, args...)
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

func (s *SQLiteStore) DeleteMemory(ctx context.Context, id, tenantID string) (bool, error) {
	query := `DELETE FROM memories WHERE id = ?`
	args := []any{id}
	if tenantID != "" {
		query += ` AND (tenant_id = ? OR (? = 'default' AND tenant_id = ''))`
		args = append(args, tenantID, tenantID)
	}
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count > 0, err
}

func (s *SQLiteStore) DeleteAllMemories(ctx context.Context, tenantID string) (int64, error) {
	query := `DELETE FROM memories`
	args := []any{}
	if tenantID != "" {
		query += ` WHERE tenant_id = ? OR (? = 'default' AND tenant_id = '')`
		args = append(args, tenantID, tenantID)
	}
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// QueryMemories retrieves and ranks memories based on vector similarity or keyword match.
// To avoid full-table scans as the memories table grows, only the most recent
// store.memory_candidate_limit rows (default 200) are loaded into memory for
// in-process cosine ranking. When the candidate set fills exactly to that cap,
// older memories are silently excluded from ranking — we log a warning so
// operators can raise the limit if recall on older memories matters.
func (s *SQLiteStore) QueryMemories(ctx context.Context, query string, embedding []float32, limit int) ([]*types.Memory, error) {
	ctx, span := tracer.Start(ctx, "store.sqlite.query_memories")
	defer span.End()

	candidateLimit := resolveMemoryCandidateLimit()
	span.SetAttributes(
		attribute.Bool("agent.query.has_embedding", len(embedding) > 0),
		attribute.Int("agent.query.embedding_dim", len(embedding)),
		attribute.Int("agent.query.limit", limit),
		attribute.Int("agent.store.memory_candidate_limit", candidateLimit),
	)

	querySQL := `SELECT id, tenant_id, session_id, task_id, goal, final_answer, key_findings, timestamp, embedding_json FROM memories`
	args := []any{}
	conditions := []string{}
	if scopedTenant, scoped := tenantScope(ctx); scoped {
		conditions = append(conditions, `(tenant_id = ? OR (? = 'default' AND tenant_id = ''))`)
		args = append(args, scopedTenant, scopedTenant)
	}
	if scopedSession, scoped := sessionScope(ctx); scoped {
		conditions = append(conditions, `session_id = ?`)
		args = append(args, scopedSession)
	}
	if len(conditions) > 0 {
		querySQL += ` WHERE ` + strings.Join(conditions, ` AND `)
	}
	querySQL += ` ORDER BY timestamp DESC LIMIT ?`
	args = append(args, candidateLimit)
	rows, err := s.db.QueryContext(ctx, querySQL, args...)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "query memories failed")
		return nil, err
	}
	defer rows.Close()

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
		warnMemoryCandidateLimitReached("sqlite", candidateLimit)
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
func (s *SQLiteStore) TryTransitionTaskStatus(ctx context.Context, id string, from []types.TaskStatus, to types.TaskStatus) (bool, error) {
	if len(from) == 0 {
		return false, nil
	}
	placeholders := make([]string, len(from))
	args := make([]any, 0, len(from)+2)
	args = append(args, to, id)
	for i, f := range from {
		placeholders[i] = "?"
		args = append(args, f)
	}
	query := fmt.Sprintf("UPDATE tasks SET status = ? WHERE id = ? AND status IN (%s)", strings.Join(placeholders, ","))

	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return false, err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if rows == 0 {
		var exists bool
		err = s.db.QueryRowContext(ctx, `SELECT 1 FROM tasks WHERE id = ?`, id).Scan(&exists)
		if err == sql.ErrNoRows {
			return false, sql.ErrNoRows
		}
		return false, nil
	}
	return true, nil
}

func (s *SQLiteStore) AcquireTaskLease(ctx context.Context, id, owner string, ttl time.Duration) (bool, error) {
	if owner == "" || ttl <= 0 {
		return false, nil
	}
	now := time.Now().UnixNano()
	expiresAt := time.Now().Add(ttl).UnixNano()
	res, err := s.db.ExecContext(ctx, `
INSERT INTO task_leases (task_id, owner, expires_at)
VALUES (?, ?, ?)
ON CONFLICT(task_id) DO UPDATE SET
	owner=excluded.owner,
	expires_at=excluded.expires_at
WHERE task_leases.expires_at <= ? OR task_leases.owner = excluded.owner
`, id, owner, expiresAt, now)
	if err != nil {
		return false, err
	}
	rows, err := res.RowsAffected()
	return rows > 0, err
}

func (s *SQLiteStore) ReleaseTaskLease(ctx context.Context, id, owner string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM task_leases WHERE task_id = ? AND owner = ?`, id, owner)
	return err
}
