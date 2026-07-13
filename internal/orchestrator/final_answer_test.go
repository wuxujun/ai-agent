package orchestrator

import (
	"strings"
	"testing"

	"github.com/wuxujun/ai-agent/internal/types"
)

func TestFinalAnswerForLimitUsesTraceProgress(t *testing.T) {
	task := &types.Task{
		ID:   "task-progress",
		Goal: "find config",
		Trace: []types.StepTrace{
			{
				Action:      "search_text",
				Query:       "database",
				Observation: "found database settings in config.yaml",
			},
		},
	}

	got := finalAnswerForLimit(task, limitReasonStepOrToolBudget)
	if strings.Contains(got, "stopped by budget or max steps") {
		t.Fatalf("expected richer final answer, got %q", got)
	}
	for _, want := range []string{
		"Stopped before a final answer could be produced",
		"Goal: find config",
		"Progress so far:",
		"found database settings in config.yaml",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("final answer missing %q: %s", want, got)
		}
	}
}

func TestFinalAnswerForLimitWithoutTraceExplainsNoObservations(t *testing.T) {
	task := &types.Task{
		ID:   "task-empty",
		Goal: "find config",
	}

	got := finalAnswerForLimit(task, limitReasonTokenBudget)
	if !strings.Contains(got, "token budget") {
		t.Fatalf("expected token budget reason, got %q", got)
	}
	if !strings.Contains(got, "No tool observations were recorded") {
		t.Fatalf("expected no-observation explanation, got %q", got)
	}
}

func TestFinalAnswerForLLMBudget(t *testing.T) {
	got := finalAnswerForLimit(&types.Task{Goal: "finish task"}, limitReasonLLMBudget)
	if !strings.Contains(got, "LLM call or estimated cost budget") {
		t.Fatalf("expected LLM budget reason, got %q", got)
	}
}
