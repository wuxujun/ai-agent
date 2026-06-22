package tools

import (
	"testing"
)

func TestWebSearchValidation(t *testing.T) {
	tool := &WebSearchTool{}

	// Query must not be empty
	err := tool.Validate(map[string]any{"query": ""})
	if err == nil {
		t.Error("expected error for empty query")
	}

	err = tool.Validate(map[string]any{"query": "   "})
	if err == nil {
		t.Error("expected error for whitespace query")
	}

	err = tool.Validate(map[string]any{"query": "golang"})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}
