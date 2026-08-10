package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestJSONQueryToolSelectsPointerAndEscapedKeys(t *testing.T) {
	workspace := t.TempDir()
	content := `{"users":[{"name":"Ada","active":true}],"a/b":{"~key":42}}`
	if err := os.WriteFile(filepath.Join(workspace, "data.json"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	tool := &JSONQueryTool{}
	result, err := tool.Execute(context.Background(), workspace, map[string]interface{}{"path": "data.json", "pointer": "/users/0/name"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Observation != `"Ada"` || len(result.Evidence) != 1 {
		t.Fatalf("unexpected result: %+v", result)
	}
	result, err = tool.Execute(context.Background(), workspace, map[string]interface{}{"path": "data.json", "pointer": "/a~1b/~0key"})
	if err != nil || result.Observation != "42" {
		t.Fatalf("escaped pointer result=%+v err=%v", result, err)
	}
}

func TestJSONQueryToolRejectsUnsafeAndInvalidInput(t *testing.T) {
	tool := &JSONQueryTool{}
	for _, params := range []map[string]any{
		{"path": "", "pointer": ""},
		{"path": "../secret.json", "pointer": ""},
		{"path": "/tmp/secret.json", "pointer": ""},
		{"path": "data.json", "pointer": "users/0"},
	} {
		if err := tool.Validate(params); err == nil {
			t.Fatalf("invalid params accepted: %+v", params)
		}
	}

	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "bad.json"), []byte(`{"a":1} {"b":2}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Execute(context.Background(), workspace, map[string]interface{}{"path": "bad.json", "pointer": ""}); err == nil || !strings.Contains(err.Error(), "multiple top-level") {
		t.Fatalf("trailing JSON was not rejected: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "data.json"), []byte(`{"users":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Execute(context.Background(), workspace, map[string]interface{}{"path": "data.json", "pointer": "/users/0"}); err == nil || !strings.Contains(err.Error(), "invalid array index") {
		t.Fatalf("invalid pointer was not rejected: %v", err)
	}
	if _, err := tool.Execute(context.Background(), workspace, map[string]interface{}{"path": "data.json", "pointer": "/users~2bad"}); err == nil || !strings.Contains(err.Error(), "invalid escape") {
		t.Fatalf("invalid pointer escape was not rejected: %v", err)
	}
}
