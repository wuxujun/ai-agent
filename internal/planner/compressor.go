package planner

import (
	"context"
	"fmt"
	"strings"

	"github.com/wuxujun/ai-agent/internal/types"
)

type ContextCompressor interface {
	Compress(ctx context.Context, task *types.Task) (string, types.TokenUsage, error)
}

type LLMContextCompressor struct {
	Finalizer *LLMTaskFinalizer
}

func NewLLMContextCompressor(scene string) *LLMContextCompressor {
	return &LLMContextCompressor{Finalizer: NewLLMTaskFinalizer(scene)}
}

func (c *LLMContextCompressor) Compress(ctx context.Context, task *types.Task) (string, types.TokenUsage, error) {
	var trace strings.Builder
	for _, item := range task.Trace {
		fmt.Fprintf(&trace, "Step %d action=%s query=%s observation=%s error=%s\n", item.Step, item.Action, item.Query, item.Observation, item.Error)
	}
	input := *task
	input.Trace = nil
	input.Goal = "Compress the following execution trace into a concise factual context. Preserve findings, failures, file paths, commands, and unresolved questions. Do not add facts.\n\n" + trace.String()
	answer, usage, err := c.Finalizer.Finalize(ctx, &input)
	if len(answer) > 12000 {
		answer = answer[:12000]
	}
	return answer, usage, err
}
