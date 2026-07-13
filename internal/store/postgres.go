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
	db             *sql.DB
	pgvectorMu     sync.Mutex
	pgvectorReady  bool
	pgvectorIdxDim int
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
		error_text TEXT NOT NULL DEFAULT '',
		prompt_tokens INT NOT NULL DEFAULT 0,
		completion_tokens INT NOT NULL DEFAULT 0,
		total_tokens INT NOT NULL DEFAULT 0,
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

CREATE TABLE IF NOT EXISTS task_leases (
	task_id VARCHAR(255) PRIMARY KEY,
	owner TEXT NOT NULL,
	expires_at BIGINT NOT NULL
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
	_, _ = p.db.Exec(`ALTER TABLE traces ADD COLUMN IF NOT EXISTS error_text TEXT NOT NULL DEFAULT ''`)
	_, _ = p.db.Exec(`ALTER TABLE traces ADD COLUMN IF NOT EXISTS prompt_tokens INT NOT NULL DEFAULT 0`)
	_, _ = p.db.Exec(`ALTER TABLE traces ADD COLUMN IF NOT EXISTS completion_tokens INT NOT NULL DEFAULT 0`)
	_, _ = p.db.Exec(`ALTER TABLE traces ADD COLUMN IF NOT EXISTS total_tokens INT NOT NULL DEFAULT 0`)
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

	_, err = tx.ExecContext(ctx, `
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
final_answer=EXCLUDED.final_answer`,
		task.ID, task.Goal, task.Status, task.MaxSteps, task.StepCount,
		task.Workspace, task.Hypothesis, string(unresolved), task.ToolBudget, task.TokenBudget, string(memoriesJSON), task.FinalAnswer,
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
		var exists int
		err := p.db.QueryRowContext(ctx, `SELECT 1 FROM memories WHERE id = $1`, "mem-"+task.ID).Scan(&exists)
		if err == sql.ErrNoRows {
			taskSnap := *task
			taskSnap.Trace = make([]types.StepTrace, len(task.Trace))
			copy(taskSnap.Trace, task.Trace)
			taskSnap.Memories = make([]types.Memory, len(task.Memories))
			copy(taskSnap.Memories, task.Memories)
			taskSnap.Unresolved = make([]string, len(task.Unresolved))
			copy(taskSnap.Unresolved, task.Unresolved)

			go func() {
				asyncCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
				defer cancel()
				mem, err := memory.CreateMemoryFromTask(asyncCtx, &taskSnap)
				if err != nil {
					log.Warn("failed to create memory for task in postgres store", "task_id", taskSnap.ID, "error", err)
					return
				}
				if err := p.SaveMemory(asyncCtx, mem); err != nil {
					log.Warn("failed to save memory for task in postgres store", "task_id", taskSnap.ID, "error", err)
				}
			}()
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

	if usePostgresPGVector() {
		if err := p.ensurePGVector(ctx); err != nil {
			return err
		}
		vecValue := any(nil)
		if len(mem.Embedding) > 0 {
			vecValue = pgVectorLiteral(mem.Embedding)
		}
		_, err = p.db.ExecContext(ctx, `
INSERT INTO memories (id, task_id, goal, final_answer, key_findings, timestamp, embedding_json, embedding_vector)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8::vector)
ON CONFLICT(id) DO UPDATE SET
task_id=EXCLUDED.task_id,
goal=EXCLUDED.goal,
final_answer=EXCLUDED.final_answer,
key_findings=EXCLUDED.key_findings,
timestamp=EXCLUDED.timestamp,
embedding_json=EXCLUDED.embedding_json,
embedding_vector=EXCLUDED.embedding_vector
`,
			mem.ID, mem.TaskID, mem.Goal, mem.FinalAnswer, mem.KeyFindings, mem.Timestamp, string(embJSON), vecValue,
		)
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

	var (
		rows *sql.Rows
		err  error
	)
	if indexDim > 0 && dim == indexDim {
		query := fmt.Sprintf(`
SELECT id, task_id, goal, final_answer, key_findings, timestamp, embedding_json
FROM memories
WHERE embedding_vector IS NOT NULL AND vector_dims(embedding_vector) = %d
ORDER BY (embedding_vector::vector(%d)) <=> $1::vector(%d)
LIMIT $2
`, indexDim, indexDim, indexDim)
		rows, err = p.db.QueryContext(ctx, query, vectorValue, limit)
	} else {
		rows, err = p.db.QueryContext(ctx, `
SELECT id, task_id, goal, final_answer, key_findings, timestamp, embedding_json
FROM memories
WHERE embedding_vector IS NOT NULL AND vector_dims(embedding_vector) = $2
ORDER BY embedding_vector <=> $1::vector
LIMIT $3
`, vectorValue, dim, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var res []*types.Memory
	for rows.Next() {
		var mem types.Memory
		var embJSON string
		if err := rows.Scan(&mem.ID, &mem.TaskID, &mem.Goal, &mem.FinalAnswer, &mem.KeyFindings, &mem.Timestamp, &embJSON); err != nil {
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
	rows, err := p.db.QueryContext(ctx, `
SELECT id, task_id, goal, final_answer, key_findings, timestamp, embedding_json
FROM memories
ORDER BY timestamp DESC
LIMIT $1
`, candidateLimit)
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

	for rows.Next() {
		var mem types.Memory
		var embJSON string
		if err := rows.Scan(&mem.ID, &mem.TaskID, &mem.Goal, &mem.FinalAnswer, &mem.KeyFindings, &mem.Timestamp, &embJSON); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "scan memory failed")
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
		score = memory.ApplyTimeDecay(score, mem.Timestamp, now, decayRate)
		ranked = append(ranked, rankResult{mem: &mem, score: score})
	}

	if len(ranked) >= candidateLimit {
		warnMemoryCandidateLimitReached("postgres", candidateLimit)
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
