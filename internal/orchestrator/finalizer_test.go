package orchestrator

import (
	"context"
	"errors"
	"testing"

	"github.com/wuxujun/ai-agent/internal/types"
)

type stubFinalizer struct {
	answer string
	err    error
}

func (s stubFinalizer) Finalize(context.Context, *types.Task) (string, types.TokenUsage, error) {
	return s.answer, types.TokenUsage{}, s.err
}

func TestFinalizeAnswer(t *testing.T) {
	task := &types.Task{ID: "task-finalize"}
	if got, _ := (&Engine{}).finalizeAnswer(context.Background(), task, "fallback"); got != "fallback" {
		t.Fatalf("nil finalizer result = %q", got)
	}
	if got, _ := (&Engine{Finalizer: stubFinalizer{answer: "synthesized"}}).finalizeAnswer(context.Background(), task, "fallback"); got != "fallback" {
		t.Fatalf("disabled finalizer result = %q", got)
	}
	engine := &Engine{Finalizer: usageFinalizer{answer: "synthesized", usage: types.TokenUsage{TotalTokens: 12}}, LLMSceneEnabled: func(string) bool { return true }}
	if got, usage := engine.finalizeAnswer(context.Background(), task, "fallback"); got != "synthesized" || usage.TotalTokens != 12 {
		t.Fatalf("enabled finalizer = %q, usage=%+v", got, usage)
	}
	engine.Finalizer = usageFinalizer{err: errors.New("failed")}
	if got, _ := engine.finalizeAnswer(context.Background(), task, "fallback"); got != "fallback" {
		t.Fatalf("failed finalizer result = %q", got)
	}
}

type usageFinalizer struct {
	answer string
	usage  types.TokenUsage
	err    error
}

func (s usageFinalizer) Finalize(context.Context, *types.Task) (string, types.TokenUsage, error) {
	return s.answer, s.usage, s.err
}
