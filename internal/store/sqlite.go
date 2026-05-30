package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"

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

func (s *SQLiteStore) ReplaceTraces(ctx context.Context, taskID string, traces []types.StepTrace) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM traces WHERE task_id = ?`, taskID); err != nil {
		return err
	}

	for _, tr := range traces {
		ev, err := json.Marshal(tr.Evidence)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO traces (task_id, step, goal, action, query, observation, evidence_json, agent_role)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
`, taskID, tr.Step, tr.Goal, tr.Action, tr.Query, tr.Observation, string(ev), string(tr.AgentRole)); err != nil {
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
		span.SetStatus(codes.Error, "replace traces failed")
		return err
	}

	if task.Status == types.StatusCompleted {
		// Automatically index completed task as a long-term memory for cross-task RAG
		if mem, err := memory.CreateMemoryFromTask(ctx, task); err == nil {
			_ = s.SaveMemory(ctx, mem)
		}
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

// ListTasks returns all tasks (without trace rows) ordered by id, capped at 500.
func (s *SQLiteStore) ListTasks(ctx context.Context) ([]*types.Task, error) {
	ctx, span := tracer.Start(ctx, "store.list_tasks")
	defer span.End()

	rows, err := s.db.QueryContext(ctx, `
SELECT id, goal, status, max_steps, step_count, workspace, hypothesis, unresolved_json, tool_budget, final_answer
FROM tasks
ORDER BY id ASC
LIMIT 500
`)
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
func (s *SQLiteStore) QueryMemories(ctx context.Context, query string, embedding []float32, limit int) ([]*types.Memory, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, task_id, goal, final_answer, key_findings, timestamp, embedding_json
FROM memories
`)
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

