package tools

import (
	"testing"
)

func TestValidateSQLReadOnly(t *testing.T) {
	tests := []struct {
		query   string
		isValid bool
	}{
		// Valid queries
		{"SELECT * FROM users", true},
		{"WITH c AS (SELECT * FROM users) SELECT * FROM c", true},
		{"EXPLAIN SELECT * FROM users", true},
		{"SELECT 'I want to DELETE this' FROM users", true},
		{"SELECT body FROM comments WHERE body LIKE '%update%'", true},
		{"SELECT * FROM users; -- select comment\nSELECT * FROM profiles", true},
		{"SELECT * FROM users /* select comment */ WHERE id = 1", true},
		{"select name, age from members where age > 18", true},

		// Prohibited queries due to mutating keywords
		{"INSERT INTO users VALUES (1, 'alice')", false},
		{"UPDATE users SET name = 'bob' WHERE id = 1", false},
		{"DELETE FROM users WHERE id = 1", false},
		{"DROP TABLE users", false},
		{"ALTER TABLE users ADD COLUMN age INT", false},
		{"CREATE TABLE users (id INT)", false},
		{"REPLACE INTO users VALUES (1, 'alice')", false},
		{"TRUNCATE TABLE logs", false},

		// Prohibited queries due to invalid first verb
		{"PRAGMA integrity_check", false},
		{"SHOW TABLES", false},

		// Prohibited query via SELECT INTO
		{"SELECT * INTO new_table FROM old_table", false},

		// Prohibited query via mutating CTE
		{"WITH c AS (DELETE FROM users) SELECT * FROM c", false},

		// Prohibited query in multi-statement
		{"SELECT * FROM users; DROP TABLE users", false},
	}

	for _, tc := range tests {
		err := ValidateSQLReadOnly(tc.query)
		if tc.isValid && err != nil {
			t.Errorf("Expected query %q to be valid, but got error: %v", tc.query, err)
		}
		if !tc.isValid && err == nil {
			t.Errorf("Expected query %q to be invalid, but got no error", tc.query)
		}
	}
}
