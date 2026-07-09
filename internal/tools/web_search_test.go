package tools

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
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

func TestWebSearchExecutePolicyViolation(t *testing.T) {
	t.Setenv("AI_AGENT_SEARCH_URL", "http://127.0.0.1:9090")
	_, _, err := config.Reload()
	if err != nil {
		t.Fatalf("failed to reload config: %v", err)
	}
	defer func() {
		os.Unsetenv("AI_AGENT_SEARCH_URL")
		_, _, _ = config.Reload()
	}()

	tool := &WebSearchTool{}
	_, err = tool.Execute(context.Background(), "/tmp", map[string]any{"query": "test"})
	if err == nil {
		t.Error("expected policy violation error for loopback search URL, got nil")
	} else if !strings.Contains(err.Error(), "policy violation") {
		t.Errorf("expected policy violation error, got: %v", err)
	}
}
