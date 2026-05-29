package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	_ "github.com/lib/pq"
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
	FOREIGN KEY(task_id) REFERENCES tasks(id) ON DELETE CASCADE
);
`
	_, err := p.db.Exec(schema)
	return err
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

	_, err = p.db.ExecContext(ctx, `
INSERT INTO tasks (id, goal, status, max_steps, step_count, workspace, hypothesis, unresolved_json, tool_budget, final_answer)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT(id) DO UPDATE SET
goal=EXCLUDED.goal,
status=EXCLUDED.status,
max_steps=EXCLUDED.max_steps,
step_count=EXCLUDED.step_count,
workspace=EXCLUDED.workspace,
hypothesis=EXCLUDED.hypothesis,
unresolved_json=EXCLUDED.unresolved_json,
tool_budget=EXCLUDED.tool_budget,
final_answer=EXCLUDED.final_answer
`,
		task.ID, task.Goal, string(task.Status), task.MaxSteps, task.StepCount,
		task.Workspace, task.Hypothesis, string(unresolved), task.ToolBudget, task.FinalAnswer,
	)
	return err
}

// ReplaceTraces deletes old traces for a task and inserts new ones within a transaction.
func (p *PostgresStore) ReplaceTraces(ctx context.Context, taskID string, traces []types.StepTrace) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM traces WHERE task_id = $1`, taskID); err != nil {
		return err
	}

	for _, tr := range traces {
		ev, err := json.Marshal(tr.Evidence)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO traces (task_id, step, goal, action, query, observation, evidence_json)
VALUES ($1, $2, $3, $4, $5, $6, $7)
`, taskID, tr.Step, tr.Goal, tr.Action, tr.Query, tr.Observation, string(ev)); err != nil {
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
		span.SetStatus(codes.Error, "replace traces failed")
		return err
	}
	return nil
}

// GetTask retrieves a task and its traces. Returns sql.ErrNoRows if not found.
func (p *PostgresStore) GetTask(ctx context.Context, id string) (*types.Task, error) {
	ctx, span := tracer.Start(ctx, "store.postgres.get_task")
	defer span.End()

	span.SetAttributes(attribute.String("agent.task.id", id))

	row := p.db.QueryRowContext(ctx, `
SELECT id, goal, status, max_steps, step_count, workspace, hypothesis, unresolved_json, tool_budget, final_answer
FROM tasks WHERE id = $1
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

	rows, err := p.db.QueryContext(ctx, `
SELECT step, goal, action, query, observation, evidence_json
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
		var evidenceJSON string
		if err := rows.Scan(&tr.Step, &tr.Goal, &tr.Action, &tr.Query, &tr.Observation, &evidenceJSON); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(evidenceJSON), &tr.Evidence); err != nil {
			return nil, err
		}
		task.Trace = append(task.Trace, tr)
	}

	return &task, rows.Err()
}
