package store

import (
	"crypto/sha256"
	"database/sql/driver"
	"encoding/hex"
	"strings"

	"modernc.org/sqlite"
)

const sqliteRestoreContentFunction = "ai_agent_restore_row_sha256_v2"

var sqliteRestoreContentRegistrationError error

// Register before connections can be opened: modernc functions apply to new
// connections. Ordinary stores never invoke this narrowly named pure function.
func init() {
	sqliteRestoreContentRegistrationError = sqlite.RegisterDeterministicScalarFunction(sqliteRestoreContentFunction, 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		if len(args) != 1 {
			return nil, ErrSQLiteRestoreVersionsInvalid
		}
		body, ok := args[0].(string)
		if !ok || len(body) > 3*sqliteRestoreVersionMaxBytes {
			return nil, ErrSQLiteRestoreVersionsInvalid
		}
		sum := sha256.Sum256([]byte("ai-agent/sqlite-core-row/v2\x00" + body))
		return hex.EncodeToString(sum[:]), nil
	})
}

// Hash storage types and exact text/blob bytes. SQLite's 26-digit alternate
// floating point formatting distinguishes doubles that CAST-to-text rounds.
// Dates remain their stored bytes; driver time parsing cannot erase a change.
func sqliteRestoreContentExpression(table sqliteRestoreVersionTable, columns []string, alias string) string {
	parts := []string{"'" + table.name + "'"}
	for _, col := range columns {
		field := `"` + col + `"`
		if alias != "" {
			field = alias + "." + field
		}
		value := `CASE WHEN typeof(` + field + `)='real' THEN printf('%!.26g',` + field + `) ELSE hex(CAST(` + field + ` AS BLOB)) END`
		parts = append(parts, `json_array('`+col+`',typeof(`+field+`),`+value+`)`)
	}
	return sqliteRestoreContentFunction + "(json_array(" + strings.Join(parts, ",") + "))"
}
func sqliteRestoreContentSize(columns []string, alias string) string {
	parts := make([]string, len(columns))
	for i, col := range columns {
		field := `"` + col + `"`
		if alias != "" {
			field = alias + "." + field
		}
		parts[i] = `coalesce(length(CAST(` + field + ` AS BLOB)),0)`
	}
	return strings.Join(parts, "+")
}
