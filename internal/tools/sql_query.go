package tools

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wuxujun/ai-agent/internal/policy"
	"github.com/wuxujun/ai-agent/internal/types"
	_ "modernc.org/sqlite"
)

type SQLQueryTool struct{}

const (
	maxSQLResultBytes = 1 << 20
	maxSQLResultRows  = 100
)

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
	return "Execute SELECT, WITH, or EXPLAIN SELECT queries against an existing workspace SQLite database. Uses SQL token checks and a read-only connection. Results are limited to 100 rows and 1 MiB per observation/evidence text."
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
	if path != "" && (filepath.IsAbs(path) || strings.Contains(path, "..") || strings.ContainsAny(path, "?#\x00")) {
		return fmt.Errorf("invalid database path")
	}

	// This is a conservative SQLite token check, not a SQL AST parser.
	if err := ValidateSQLReadOnly(query); err != nil {
		return fmt.Errorf("SQL safety check failed: %w", err)
	}

	return nil
}

func (t *SQLQueryTool) Execute(ctx context.Context, workspace string, params map[string]interface{}) (*ToolResult, error) {
	// Not every caller goes through planner validation.
	if err := t.Validate(params); err != nil {
		return nil, err
	}
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

	if info, err := os.Stat(fullPath); err != nil || !info.Mode().IsRegular() {
		if err == nil {
			err = fmt.Errorf("database path is not a regular file")
		}
		return nil, fmt.Errorf("failed to open database: %w", err)
	}
	db, err := openReadOnlySQLite(fullPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}
	defer db.Close()

	// The caller's timeout/cancellation applies to opening and iterating too.
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("SQL execution error: %w", err)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("failed to read columns: %w", err)
	}

	// Reserve room for the observation prefix and truncation marker. Account for
	// column names, separators, and newlines as well as cell data in both outputs.
	const dataBudget = maxSQLResultBytes - 128
	var output []string
	outputBytes := 0
	appendLine := func(parts []string) bool {
		size := 0
		if len(parts) > 0 {
			size = (len(parts) - 1) * len(" | ")
		}
		if len(output) > 0 {
			size++
		}
		for _, part := range parts {
			size += len(part)
		}
		if size > dataBudget-outputBytes {
			return false
		}
		output = append(output, strings.Join(parts, " | "))
		outputBytes += size
		return true
	}
	truncated := !appendLine(cols)

	// Prepare pointers for scanning
	vals := make([]interface{}, len(cols))
	valPtrs := make([]interface{}, len(cols))
	for i := range vals {
		valPtrs[i] = &vals[i]
	}

	rowCount := 0
	for !truncated && rows.Next() {
		if rowCount == maxSQLResultRows {
			truncated = true
			break
		}
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
					// The driver has already materialized the cell; avoid another
					// unbounded allocation when rendering it.
					if len(v) > dataBudget-outputBytes {
						truncated = true
						break
					}
					rowStrs = append(rowStrs, string(v))
				case string:
					rowStrs = append(rowStrs, v)
				default:
					rowStrs = append(rowStrs, fmt.Sprintf("%v", v))
				}
			}
		}
		if truncated || !appendLine(rowStrs) {
			truncated = true
			break
		}
		rowCount++
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("row iteration error: %w", err)
	}
	if truncated {
		output = append(output, "...[truncated, row or byte limit reached]")
	}

	observation := strings.Join(output, "\n")

	return &ToolResult{
		Query:       query,
		Observation: fmt.Sprintf("Query result. Showing %d row(s):\n%s", rowCount, observation),
		Evidence: []types.Evidence{{
			Path:  path,
			Lines: output,
			Query: query,
		}},
	}, nil
}

// URI-encode the actual filename: percent escapes in a filesystem path must
// never be interpreted as path traversal or SQLite connection parameters.
// modernc applies _pragma to EVERY newly opened connection, including a pool
// replacement; mode=ro independently prevents writes to the main database.
func openReadOnlySQLite(path string) (*sql.DB, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	uriPath := filepath.ToSlash(absPath)
	if !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	u := url.URL{Scheme: "file", Path: uriPath, RawQuery: url.Values{
		"mode": {"ro"}, "_pragma": {"query_only(1)"},
	}.Encode()}
	return sql.Open("sqlite", u.String())
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
		"PRAGMA":   true, // Some pragmas run during preparation, even under EXPLAIN.
		"ATTACH":   true,
		"DETACH":   true,
		"VACUUM":   true,
		"REINDEX":  true,
		"ANALYZE":  true,
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
	if strings.IndexByte(query, 0) >= 0 {
		return nil, fmt.Errorf("NUL byte in query")
	}
	var tokens []string
	var buf strings.Builder
	runes := []rune(query)
	n := len(runes)

	for i := 0; i < n; {
		r := runes[i]
		// SQLite can treat a token-leading UTF-8 BOM as whitespace. Reject it
		// outside literals/comments so it cannot hide PRAGMA from this check.
		if r == '\ufeff' {
			return nil, fmt.Errorf("BOM outside quoted SQL literal or comment")
		}

		// Skip inline comments
		if r == '-' && i+1 < n && runes[i+1] == '-' {
			if buf.Len() > 0 {
				tokens = append(tokens, buf.String())
				buf.Reset()
			}
			i += 2
			for i < n && runes[i] != '\n' {
				i++
			}
			continue
		}

		// Skip block comments
		if r == '/' && i+1 < n && runes[i+1] == '*' {
			if buf.Len() > 0 {
				tokens = append(tokens, buf.String())
				buf.Reset()
			}
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
		if r == '[' {
			if buf.Len() > 0 {
				tokens = append(tokens, buf.String())
				buf.Reset()
			}
			i++
			foundEnd := false
			for i < n {
				if runes[i] == ']' {
					i++
					foundEnd = true
					break
				}
				i++
			}
			if !foundEnd {
				return nil, fmt.Errorf("unclosed bracketed identifier")
			}
			tokens = append(tokens, "[identifier]")
			continue
		}
		if r == '\'' || r == '"' || r == '`' {
			if buf.Len() > 0 {
				tokens = append(tokens, buf.String())
				buf.Reset()
			}
			quote := r
			i++
			foundEnd := false
			for i < n {
				if runes[i] == quote {
					if i+1 < n && runes[i+1] == quote {
						i += 2
						continue
					}
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
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\f' {
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
