package tools

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/wuxujun/ai-agent/internal/policy"
	"github.com/wuxujun/ai-agent/internal/types"
	_ "modernc.org/sqlite"
)

type SQLQueryTool struct{}

func (t *SQLQueryTool) Name() string {
	return "sql_query"
}

func (t *SQLQueryTool) RiskLevel() types.RiskLevel {
	return types.RiskLevelLow
}

func (t *SQLQueryTool) RetryPolicy() RetryPolicy {
	return RetryPolicy{MaxRetries: 2, Backoff: time.Second}
}

func (t *SQLQueryTool) Description() string {
	return "Execute a read-only SQL query against a SQLite database in the workspace. Protected by read-only AST checks."
}

func (t *SQLQueryTool) Parameters() map[string]any {
	return map[string]any{
		"path":  map[string]any{"type": "string", "description": "Workspace-relative path to SQLite database file (defaults to 'data/agent.db')"},
		"query": map[string]any{"type": "string", "description": "SQL SELECT query to execute"},
	}
}

func (t *SQLQueryTool) Validate(params map[string]any) error {
	query, _ := params["query"].(string)
	query = strings.TrimSpace(query)
	if query == "" {
		return fmt.Errorf("sql_query requires non-empty query")
	}

	path, _ := params["path"].(string)
	path = strings.TrimSpace(path)
	if path != "" && (strings.HasPrefix(path, "/") || strings.Contains(path, "..")) {
		return fmt.Errorf("invalid database path")
	}

	// Run AST protection check
	if err := ValidateSQLReadOnly(query); err != nil {
		return fmt.Errorf("SQL safety check failed: %w", err)
	}

	return nil
}

func (t *SQLQueryTool) Execute(ctx context.Context, workspace string, params map[string]interface{}) (*ToolResult, error) {
	path, _ := params["path"].(string)
	path = strings.TrimSpace(path)
	if path == "" {
		path = "data/agent.db"
	}

	query, _ := params["query"].(string)
	query = strings.TrimSpace(query)

	fullPath := filepath.Join(workspace, path)
	if err := policy.ValidateReadPath(workspace, fullPath); err != nil {
		return nil, fmt.Errorf("sql_query policy violation: %w", err)
	}

	db, err := sql.Open("sqlite", fullPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}
	defer db.Close()

	// Execute with timeout context
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("SQL execution error: %w", err)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("failed to read columns: %w", err)
	}

	// Scan results
	var output []string
	output = append(output, strings.Join(cols, " | "))

	// Prepare pointers for scanning
	vals := make([]interface{}, len(cols))
	valPtrs := make([]interface{}, len(cols))
	for i := range vals {
		valPtrs[i] = &vals[i]
	}

	rowCount := 0
	for rows.Next() {
		if err := rows.Scan(valPtrs...); err != nil {
			return nil, fmt.Errorf("scan error: %w", err)
		}

		var rowStrs []string
		for _, val := range vals {
			if val == nil {
				rowStrs = append(rowStrs, "NULL")
			} else {
				// Convert to string representation
				switch v := val.(type) {
				case []byte:
					rowStrs = append(rowStrs, string(v))
				default:
					rowStrs = append(rowStrs, fmt.Sprintf("%v", v))
				}
			}
		}
		output = append(output, strings.Join(rowStrs, " | "))
		rowCount++
		// Cap observation rows to avoid context blowup
		if rowCount >= 100 {
			output = append(output, "...[truncated, showing top 100 rows]")
			break
		}
	}

	observation := strings.Join(output, "\n")

	return &ToolResult{
		Query:       query,
		Observation: fmt.Sprintf("Query executed successfully. Returned %d row(s):\n%s", rowCount, observation),
		Evidence: []types.Evidence{{
			Path:  path,
			Lines: output,
			Query: query,
		}},
	}, nil
}

// ValidateSQLReadOnly checks that the SQL query is strictly read-only
// and doesn't contain any mutating keywords or operations.
func ValidateSQLReadOnly(query string) error {
	tokens, err := tokenizeSQL(query)
	if err != nil {
		return err
	}

	if len(tokens) == 0 {
		return fmt.Errorf("empty query")
	}

	// Block list of mutating verbs
	forbidden := map[string]bool{
		"INSERT":   true,
		"UPDATE":   true,
		"DELETE":   true,
		"DROP":     true,
		"ALTER":    true,
		"CREATE":   true,
		"REPLACE":  true,
		"TRUNCATE": true,
		"MERGE":    true,
		"GRANT":    true,
		"REVOKE":   true,
		"INTO":     true, // Blocks SELECT INTO
	}

	for _, token := range tokens {
		upper := strings.ToUpper(token)
		if forbidden[upper] {
			return fmt.Errorf("query contains mutating token %q which is prohibited in read-only queries", token)
		}
	}

	// Ensure each statement (separated by ;) starts with SELECT, WITH, or EXPLAIN
	newStmt := true
	for _, token := range tokens {
		if newStmt {
			upper := strings.ToUpper(token)
			if upper != "SELECT" && upper != "WITH" && upper != "EXPLAIN" {
				return fmt.Errorf("statement starts with forbidden verb %q (only SELECT, WITH, and EXPLAIN are allowed)", token)
			}
			newStmt = false
		}
		if token == ";" {
			newStmt = true
		}
	}

	return nil
}

func tokenizeSQL(query string) ([]string, error) {
	var tokens []string
	var buf strings.Builder
	runes := []rune(query)
	n := len(runes)

	for i := 0; i < n; {
		r := runes[i]

		// Skip inline comments
		if r == '-' && i+1 < n && runes[i+1] == '-' {
			i += 2
			for i < n && runes[i] != '\n' {
				i++
			}
			continue
		}

		// Skip block comments
		if r == '/' && i+1 < n && runes[i+1] == '*' {
			i += 2
			foundEnd := false
			for i < n {
				if runes[i] == '*' && i+1 < n && runes[i+1] == '/' {
					i += 2
					foundEnd = true
					break
				}
				i++
			}
			if !foundEnd {
				return nil, fmt.Errorf("unclosed comment block")
			}
			continue
		}

		// Handle string literals (treat as token placeholder to avoid false-positives inside strings)
		if r == '\'' || r == '"' || r == '`' {
			quote := r
			i++
			foundEnd := false
			for i < n {
				if runes[i] == '\\' && i+1 < n {
					i += 2
					continue
				}
				if runes[i] == quote {
					i++
					foundEnd = true
					break
				}
				i++
			}
			if !foundEnd {
				return nil, fmt.Errorf("unclosed string literal")
			}
			tokens = append(tokens, "'string'")
			continue
		}

		// Whitespace
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			if buf.Len() > 0 {
				tokens = append(tokens, buf.String())
				buf.Reset()
			}
			i++
			continue
		}

		// Separators
		if r == ';' || r == '(' || r == ')' || r == ',' {
			if buf.Len() > 0 {
				tokens = append(tokens, buf.String())
				buf.Reset()
			}
			tokens = append(tokens, string(r))
			i++
			continue
		}

		buf.WriteRune(r)
		i++
	}

	if buf.Len() > 0 {
		tokens = append(tokens, buf.String())
	}

	return tokens, nil
}

func init() {
	Register(&SQLQueryTool{})
}
