package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/wuxujun/ai-agent/internal/memory"
	"github.com/wuxujun/ai-agent/internal/types"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	_ "modernc.org/sqlite"
)

var tracer = otel.Tracer("ai-agent/store")

type SQLiteStore struct {
	db *sql.DB
}

func NewSQLiteStore(dsn string) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}

	s := &SQLiteStore{db: db}
	if err := s.init(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *SQLiteStore) init() error {
	schema := `
CREATE TABLE IF NOT EXISTS tasks (
	id TEXT PRIMARY KEY,
	goal TEXT NOT NULL,
	status TEXT NOT NULL,
	max_steps INTEGER NOT NULL,
	step_count INTEGER NOT NULL,
	workspace TEXT NOT NULL,
	hypothesis TEXT NOT NULL,
	unresolved_json TEXT NOT NULL,
	tool_budget INTEGER NOT NULL,
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
	FOREIGN KEY(task_id) REFERENCES tasks(id)
);

CREATE TABLE IF NOT EXISTS memories (
	id TEXT PRIMARY KEY,
	task_id TEXT NOT NULL,
	goal TEXT NOT NULL,
	final_answer TEXT NOT NULL,
	key_findings TEXT NOT NULL,
	timestamp DATETIME NOT NULL,
	embedding_json TEXT NOT NULL
);
`
	if _, err := s.db.Exec(schema); err != nil {
		return err
	}
	// Migrate: add agent_role column to traces if it doesn't exist yet.
	// SQLite returns an error if the column already exists; we ignore it.
	_, _ = s.db.Exec(`ALTER TABLE traces ADD COLUMN agent_role TEXT NOT NULL DEFAULT ''`)
	_, _ = s.db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_traces_task_id_step ON traces(task_id, step)`)
	// Index for time-bounded memory retrieval (QueryMemories uses ORDER BY timestamp DESC)
	_, _ = s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_memories_timestamp ON memories(timestamp DESC)`)
	return nil
}

func (s *SQLiteStore) Close() error {
	return s.db.Close()
}

func (s *SQLiteStore) SaveTask(ctx context.Context, task *types.Task) error {
	unresolved, err := json.Marshal(task.Unresolved)
	if err != nil {
		return err
	}

	_, err = s.db.ExecContext(ctx, `
INSERT INTO tasks (id, goal, status, max_steps, step_count, workspace, hypothesis, unresolved_json, tool_budget, final_answer)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
goal=excluded.goal,
status=excluded.status,
max_steps=excluded.max_steps,
step_count=excluded.step_count,
workspace=excluded.workspace,
hypothesis=excluded.hypothesis,
unresolved_json=excluded.unresolved_json,
tool_budget=excluded.tool_budget,
final_answer=excluded.final_answer
`,
		task.ID, task.Goal, task.Status, task.MaxSteps, task.StepCount,
		task.Workspace, task.Hypothesis, string(unresolved), task.ToolBudget, task.FinalAnswer,
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
	row := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(step), 0) FROM traces WHERE task_id = ?`, taskID)
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
				(task_id, step, goal, action, query, observation, evidence_json, agent_role)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			taskID, tr.Step, tr.Goal, tr.Action, tr.Query, tr.Observation, string(ev), string(tr.AgentRole),
		); err != nil {
			return err
		}
	}

	return tx.Commit()
}

func (s *SQLiteStore) SaveFullTask(ctx context.Context, task *types.Task) error {
	ctx, span := tracer.Start(ctx, "store.save_full_task")
	defer span.End()

	span.SetAttributes(
		attribute.String("agent.task.id", task.ID),
		attribute.Int("agent.task.trace_count", len(task.Trace)),
	)

	if err := s.SaveTask(ctx, task); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "save task failed")
		return err
	}
	if err := s.ReplaceTraces(ctx, task.ID, task.Trace); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "append traces failed")
		return err
	}

	if task.Status == types.StatusCompleted {
		// Index the completed task as a long-term memory for cross-task RAG.
		// This is done asynchronously with its own context so that:
		// 1. The Embedding API call (network I/O) never blocks the SaveFullTask
		//    response path — task saves return immediately.
		// 2. A cancelled parent context (e.g. request timeout) cannot abort the
		//    indexing of a task that has already been successfully persisted.
		//
		// Deep-copy the slice fields so the goroutine is not racing with the
		// caller, which may append to task.Trace after SaveFullTask returns.
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
				log.Printf("[Store] Warning: failed to create memory for task %s: %v", taskSnap.ID, err)
				return
			}
			if err := s.SaveMemory(asyncCtx, mem); err != nil {
				log.Printf("[Store] Warning: failed to save memory for task %s: %v", taskSnap.ID, err)
			}
		}()
	}

	return nil
}

func (s *SQLiteStore) GetTask(ctx context.Context, id string) (*types.Task, error) {
	ctx, span := tracer.Start(ctx, "store.get_task")
	defer span.End()

	span.SetAttributes(attribute.String("agent.task.id", id))

	row := s.db.QueryRowContext(ctx, `
SELECT id, goal, status, max_steps, step_count, workspace, hypothesis, unresolved_json, tool_budget, final_answer
FROM tasks WHERE id = ?
`, id)

	var task types.Task
	var unresolvedJSON string

	err := row.Scan(
		&task.ID, &task.Goal, &task.Status, &task.MaxSteps, &task.StepCount,
		&task.Workspace, &task.Hypothesis, &unresolvedJSON, &task.ToolBudget, &task.FinalAnswer,
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

	rows, err := s.db.QueryContext(ctx, `
SELECT step, goal, action, query, observation, evidence_json, agent_role
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
SELECT id, goal, status, max_steps, step_count, workspace, hypothesis, unresolved_json, tool_budget, final_answer
FROM tasks`

	var (
		query string
		args  []any
	)
	if f.Status != "" {
		query = base + "\nWHERE status = ?\nORDER BY id ASC\nLIMIT ? OFFSET ?"
		args = []any{string(f.Status), limit, f.Offset}
	} else {
		query = base + "\nORDER BY id ASC\nLIMIT ? OFFSET ?"
		args = []any{limit, f.Offset}
	}

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
		if err := rows.Scan(
			&t.ID, &t.Goal, &t.Status, &t.MaxSteps, &t.StepCount,
			&t.Workspace, &t.Hypothesis, &unresolvedJSON, &t.ToolBudget, &t.FinalAnswer,
		); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(unresolvedJSON), &t.Unresolved); err != nil {
			return nil, err
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

// SaveMemory persists a memory entry to SQLite.
func (s *SQLiteStore) SaveMemory(ctx context.Context, mem *types.Memory) error {
	embJSON, err := json.Marshal(mem.Embedding)
	if err != nil {
		return err
	}

	_, err = s.db.ExecContext(ctx, `
INSERT INTO memories (id, task_id, goal, final_answer, key_findings, timestamp, embedding_json)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
task_id=excluded.task_id,
goal=excluded.goal,
final_answer=excluded.final_answer,
key_findings=excluded.key_findings,
timestamp=excluded.timestamp,
embedding_json=excluded.embedding_json
`,
		mem.ID, mem.TaskID, mem.Goal, mem.FinalAnswer, mem.KeyFindings, mem.Timestamp, string(embJSON),
	)
	return err
}

// QueryMemories retrieves and ranks memories based on vector similarity or keyword match.
// To avoid full-table scans as the memories table grows, only the most recent
// maxCandidates rows are loaded into memory for in-process cosine ranking.
const queryCandidateLimit = 200

func (s *SQLiteStore) QueryMemories(ctx context.Context, query string, embedding []float32, limit int) ([]*types.Memory, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, task_id, goal, final_answer, key_findings, timestamp, embedding_json
FROM memories
ORDER BY timestamp DESC
LIMIT ?
`, queryCandidateLimit)
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

