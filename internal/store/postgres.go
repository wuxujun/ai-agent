package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	_ "github.com/lib/pq"
	"github.com/wuxujun/ai-agent/internal/memory"
	"github.com/wuxujun/ai-agent/internal/types"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// PostgresStore implements Store using a PostgreSQL database.
type PostgresStore struct {
	db *sql.DB
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
	return p, nil
}

func (p *PostgresStore) init() error {
	schema := `
CREATE TABLE IF NOT EXISTS tasks (
	id VARCHAR(255) PRIMARY KEY,
	goal TEXT NOT NULL,
	status VARCHAR(50) NOT NULL,
	max_steps INT NOT NULL,
	step_count INT NOT NULL,
	workspace TEXT NOT NULL,
	hypothesis TEXT NOT NULL,
	unresolved_json TEXT NOT NULL,
	tool_budget INT NOT NULL,
	token_budget INT NOT NULL DEFAULT 0,
	memories_json TEXT NOT NULL DEFAULT '[]',
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
	FOREIGN KEY(task_id) REFERENCES tasks(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS memories (
	id VARCHAR(255) PRIMARY KEY,
	task_id VARCHAR(255) NOT NULL,
	goal TEXT NOT NULL,
	final_answer TEXT NOT NULL,
	key_findings TEXT NOT NULL,
	timestamp TIMESTAMP NOT NULL,
	embedding_json TEXT NOT NULL
);
`
	_, err := p.db.Exec(schema)
	if err != nil {
		return err
	}
	// Idempotent migrations for existing databases that predate these columns.
	// Postgres supports IF NOT EXISTS on ADD COLUMN since 9.6.
	_, _ = p.db.Exec(`ALTER TABLE tasks ADD COLUMN IF NOT EXISTS token_budget INT NOT NULL DEFAULT 0`)
	_, _ = p.db.Exec(`ALTER TABLE tasks ADD COLUMN IF NOT EXISTS memories_json TEXT NOT NULL DEFAULT '[]'`)
	_, _ = p.db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_traces_task_id_step ON traces(task_id, step)`)
	return nil
}

// Close closes the database connection.
func (p *PostgresStore) Close() error {
	return p.db.Close()
}

// SaveTask inserts or updates task metadata.
func (p *PostgresStore) SaveTask(ctx context.Context, task *types.Task) error {
	unresolved, err := json.Marshal(task.Unresolved)
	if err != nil {
		return err
	}

	memoriesJSON, err := json.Marshal(memoriesForPersistence(task.Memories))
	if err != nil {
		return err
	}

	_, err = p.db.ExecContext(ctx, `
INSERT INTO tasks (id, goal, status, max_steps, step_count, workspace, hypothesis, unresolved_json, tool_budget, token_budget, memories_json, final_answer)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
ON CONFLICT(id) DO UPDATE SET
goal=EXCLUDED.goal,
status=EXCLUDED.status,
max_steps=EXCLUDED.max_steps,
step_count=EXCLUDED.step_count,
workspace=EXCLUDED.workspace,
hypothesis=EXCLUDED.hypothesis,
unresolved_json=EXCLUDED.unresolved_json,
tool_budget=EXCLUDED.tool_budget,
token_budget=EXCLUDED.token_budget,
memories_json=EXCLUDED.memories_json,
final_answer=EXCLUDED.final_answer
`,
		task.ID, task.Goal, string(task.Status), task.MaxSteps, task.StepCount,
		task.Workspace, task.Hypothesis, string(unresolved), task.ToolBudget, task.TokenBudget, string(memoriesJSON), task.FinalAnswer,
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
				(task_id, step, goal, action, query, observation, evidence_json, agent_role)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (task_id, step) DO NOTHING`,
			taskID, tr.Step, tr.Goal, tr.Action, tr.Query, tr.Observation, string(ev), string(tr.AgentRole),
		); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// SaveFullTask saves the task metadata and all associated traces inside a telemetry transaction.
func (p *PostgresStore) SaveFullTask(ctx context.Context, task *types.Task) error {
	ctx, span := tracer.Start(ctx, "store.postgres.save_full_task")
	defer span.End()

	span.SetAttributes(
		attribute.String("agent.task.id", task.ID),
		attribute.Int("agent.task.trace_count", len(task.Trace)),
	)

	if err := p.SaveTask(ctx, task); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "save task failed")
		return err
	}
	if err := p.ReplaceTraces(ctx, task.ID, task.Trace); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "append traces failed")
		return err
	}

	if task.Status == types.StatusCompleted {
		// Automatically index completed task as a long-term memory for cross-task RAG
		if mem, err := memory.CreateMemoryFromTask(ctx, task); err == nil {
			_ = p.SaveMemory(ctx, mem)
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
SELECT id, goal, status, max_steps, step_count, workspace, hypothesis, unresolved_json, tool_budget, token_budget, memories_json, final_answer
FROM tasks WHERE id = $1
`, id)

	var task types.Task
	var unresolvedJSON string
	var memoriesJSON string

	err := row.Scan(
		&task.ID, &task.Goal, &task.Status, &task.MaxSteps, &task.StepCount,
		&task.Workspace, &task.Hypothesis, &unresolvedJSON, &task.ToolBudget, &task.TokenBudget, &memoriesJSON, &task.FinalAnswer,
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

	rows, err := p.db.QueryContext(ctx, `
SELECT step, goal, action, query, observation, evidence_json, agent_role
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
		if err := rows.Scan(&tr.Step, &tr.Goal, &tr.Action, &tr.Query, &tr.Observation, &evidenceJSON, &agentRole); err != nil {
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
	where := ""
	if f.Status != "" {
		where = "WHERE status = $1"
		args = append(args, string(f.Status))
	}
	offsetArg := len(args) + 1
	limitArg := len(args) + 2
	args = append(args, f.Offset, limit)

	query := fmt.Sprintf(`
SELECT id, goal, status, max_steps, step_count, workspace, hypothesis, unresolved_json, tool_budget, token_budget, memories_json, final_answer
FROM tasks
%s
ORDER BY id ASC
LIMIT $%d OFFSET $%d
`, where, limitArg, offsetArg)

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
		if err := rows.Scan(
			&t.ID, &t.Goal, &t.Status, &t.MaxSteps, &t.StepCount,
			&t.Workspace, &t.Hypothesis, &unresolvedJSON, &t.ToolBudget, &t.TokenBudget, &memoriesJSON, &t.FinalAnswer,
		); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(unresolvedJSON), &t.Unresolved); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(memoriesJSON), &t.Memories); err != nil {
			return nil, err
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

// SaveMemory persists a memory entry to Postgres.
func (p *PostgresStore) SaveMemory(ctx context.Context, mem *types.Memory) error {
	embJSON, err := json.Marshal(mem.Embedding)
	if err != nil {
		return err
	}

	_, err = p.db.ExecContext(ctx, `
INSERT INTO memories (id, task_id, goal, final_answer, key_findings, timestamp, embedding_json)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT(id) DO UPDATE SET
task_id=EXCLUDED.task_id,
goal=EXCLUDED.goal,
final_answer=EXCLUDED.final_answer,
key_findings=EXCLUDED.key_findings,
timestamp=EXCLUDED.timestamp,
embedding_json=EXCLUDED.embedding_json
`,
		mem.ID, mem.TaskID, mem.Goal, mem.FinalAnswer, mem.KeyFindings, mem.Timestamp, string(embJSON),
	)
	return err
}

// QueryMemories retrieves and ranks memories based on vector similarity or keyword match in Postgres.
// Like the SQLite backend, only the most recent store.memory_candidate_limit
// rows (default 200) are loaded for in-process ranking; logs a warning when the
// cap is hit so operators can raise the limit if older memories matter.
func (p *PostgresStore) QueryMemories(ctx context.Context, query string, embedding []float32, limit int) ([]*types.Memory, error) {
	candidateLimit := resolveMemoryCandidateLimit()
	rows, err := p.db.QueryContext(ctx, `
SELECT id, task_id, goal, final_answer, key_findings, timestamp, embedding_json
FROM memories
ORDER BY timestamp DESC
LIMIT $1
`, candidateLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type rankResult struct {
		mem   *types.Memory
		score float32
	}
	var ranked []rankResult

	for rows.Next() {
		var mem types.Memory
		var embJSON string
		if err := rows.Scan(&mem.ID, &mem.TaskID, &mem.Goal, &mem.FinalAnswer, &mem.KeyFindings, &mem.Timestamp, &embJSON); err != nil {
			return nil, err
		}

		if err := json.Unmarshal([]byte(embJSON), &mem.Embedding); err != nil {
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
			"backend", "postgres",
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
	return res, rows.Err()
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

