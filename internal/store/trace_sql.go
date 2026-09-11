package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/wuxujun/ai-agent/internal/types"
)

// SQL step is the 1-based event position in the complete trace snapshot.
// execution_step preserves the public Step value, which multiple audit events
// may share. Existing rows with NULL execution_step retain their legacy Step on
// read and are converted on the next complete save. No execution budget changes.
func saveTraceSnapshotSQL(ctx context.Context, tx *sql.Tx, id string, traces []types.StepTrace, postgres bool) error {
	mark := func(n int) string {
		if postgres {
			return fmt.Sprintf("$%d", n)
		}
		return "?"
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM traces WHERE task_id = "+mark(1)+" AND (step < 1 OR step > "+mark(2)+")", id, len(traces)); err != nil {
		return err
	}
	columns := []string{"execution_step", "goal", "action", "query", "observation", "evidence_json", "agent_role", "error_text", "prompt_tokens", "completion_tokens", "total_tokens"}
	placeholders := make([]string, len(columns)+2)
	for i := range placeholders {
		placeholders[i] = mark(i + 1)
	}
	updates := make([]string, len(columns))
	changed := make([]string, len(columns))
	for i, column := range columns {
		updates[i] = column + " = excluded." + column
		changed[i] = "traces." + column + " <> excluded." + column
	}
	query := "INSERT INTO traces (task_id, step, " + strings.Join(columns, ", ") + ") VALUES (" + strings.Join(placeholders, ", ") + ") ON CONFLICT(task_id, step) DO UPDATE SET " + strings.Join(updates, ", ") + " WHERE traces.execution_step IS NULL OR " + strings.Join(changed, " OR ")
	for i, tr := range traces {
		evidence, err := json.Marshal(tr.Evidence)
		if err != nil {
			return err
		}
		args := []any{id, i + 1, tr.Step, tr.Goal, tr.Action, tr.Query, tr.Observation, string(evidence), string(tr.AgentRole), tr.Error, tr.TokenUsage.PromptTokens, tr.TokenUsage.CompletionTokens, tr.TokenUsage.TotalTokens}
		if postgres {
			for i, value := range args {
				if text, ok := value.(string); ok {
					args[i] = postgresText(text)
				}
			}
		}
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return err
		}
	}
	return nil
}
