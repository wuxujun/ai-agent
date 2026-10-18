package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/wuxujun/ai-agent/internal/types"
)

// AuditFilter selects reports without loading task goals, answers or traces.
type AuditFilter struct {
	TenantID                                   string
	Status                                     types.TaskStatus
	Stage, StageState, Confidence, Enforcement string
	Publishable                                *bool
	EligibleOnly                               bool
	Limit, Offset                              int
}

type AuditStore interface {
	ListAuditTasks(context.Context, AuditFilter) ([]*types.Task, error)
	CountAuditEligibleTasks(context.Context, string, types.TaskStatus) (int, error)
}

func (s *SQLiteStore) ListAuditTasks(ctx context.Context, f AuditFilter) ([]*types.Task, error) {
	return listSQLAudits(ctx, s.db, f, false)
}
func (p *PostgresStore) ListAuditTasks(ctx context.Context, f AuditFilter) ([]*types.Task, error) {
	return listSQLAudits(ctx, p.db, f, true)
}
func (s *SQLiteStore) CountAuditEligibleTasks(ctx context.Context, tenant string, status types.TaskStatus) (int, error) {
	return countSQLAuditEligible(ctx, s.db, tenant, status, false)
}
func (p *PostgresStore) CountAuditEligibleTasks(ctx context.Context, tenant string, status types.TaskStatus) (int, error) {
	return countSQLAuditEligible(ctx, p.db, tenant, status, true)
}

func auditSQLConditions(f AuditFilter, postgres, reports bool) (string, []any) {
	conditions := []string{}
	args := []any{}
	add := func(expr string, value any) {
		args = append(args, value)
		mark := "?"
		if postgres {
			mark = fmt.Sprintf("$%d", len(args))
		}
		conditions = append(conditions, expr+mark)
	}
	if f.TenantID != "" {
		add("tenant_id = ", f.TenantID)
	}
	if f.Status != "" {
		add("status = ", f.Status)
	}
	if f.EligibleOnly {
		conditions = append(conditions, "status IN ('completed','partial','failed')")
		// Match strings.TrimSpace, including newline and Unicode whitespace.
		args = append(args, "\t\n\v\f\r \u0085\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000")
		trim := "TRIM(final_answer, ?)"
		if postgres {
			trim = fmt.Sprintf("BTRIM(final_answer, $%d)", len(args))
		}
		conditions = append(conditions, trim+" <> ''")
	}
	if reports {
		field := func(name string) string {
			if postgres {
				return "(answer_audit_json::jsonb ->> '" + name + "')"
			}
			return "json_extract(answer_audit_json, '$." + name + "')"
		}
		// JSON null represents a task without an audit; {} is also the legacy default.
		if postgres {
			conditions = append(conditions, "answer_audit_json::jsonb NOT IN ('null'::jsonb, '{}'::jsonb)")
		} else {
			conditions = append(conditions, "answer_audit_json NOT IN ('null', '{}', '')")
		}
		if f.Confidence != "" {
			add(field("final_confidence")+" = ", f.Confidence)
		}
		if f.Enforcement != "" {
			add(field("enforcement")+" = ", f.Enforcement)
		}
		if f.Publishable != nil {
			if postgres {
				add(field("publishable")+" = ", fmt.Sprint(*f.Publishable))
			} else {
				add(field("publishable")+" = ", *f.Publishable)
			}
		}
		if f.Stage != "" || f.StageState != "" {
			stageConditions := []string{}
			for _, item := range []struct{ field, value string }{{"name", f.Stage}, {"status", f.StageState}} {
				if item.value == "" {
					continue
				}
				args = append(args, item.value)
				mark := "?"
				expr := "json_extract(stage.value, '$." + item.field + "')"
				if postgres {
					mark = fmt.Sprintf("$%d", len(args))
					expr = "stage.value ->> '" + item.field + "'"
				}
				stageConditions = append(stageConditions, expr+" = "+mark)
			}
			source := "json_each(answer_audit_json, '$.stages') AS stage"
			if postgres {
				source = "jsonb_array_elements(COALESCE(NULLIF(answer_audit_json::jsonb -> 'stages', 'null'::jsonb), '[]'::jsonb)) AS stage(value)"
			}
			conditions = append(conditions, "EXISTS (SELECT 1 FROM "+source+" WHERE "+strings.Join(stageConditions, " AND ")+")")
		}
	}
	if len(conditions) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conditions, " AND "), args
}

func listSQLAudits(ctx context.Context, db *sql.DB, f AuditFilter, postgres bool) ([]*types.Task, error) {
	where, args := auditSQLConditions(f, postgres, true)
	query := "SELECT id, tenant_id, status, answer_audit_json FROM tasks" + where + " ORDER BY id ASC"
	limit := resolveLimit(f.Limit, 50, 500)
	if postgres {
		query += fmt.Sprintf(" LIMIT $%d OFFSET $%d", len(args)+1, len(args)+2)
	} else {
		query += " LIMIT ? OFFSET ?"
	}
	args = append(args, limit, max(0, f.Offset))
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tasks := make([]*types.Task, 0, limit)
	for rows.Next() {
		task := &types.Task{}
		var raw string
		if err := rows.Scan(&task.ID, &task.TenantID, &task.Status, &raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &task.AnswerAudit); err != nil {
			return nil, fmt.Errorf("decode audit report: %w", err)
		}
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}
func countSQLAuditEligible(ctx context.Context, db *sql.DB, tenant string, status types.TaskStatus, postgres bool) (int, error) {
	where, args := auditSQLConditions(AuditFilter{TenantID: tenant, Status: status, EligibleOnly: true}, postgres, false)
	var count int
	err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM tasks"+where, args...).Scan(&count)
	return count, err
}
