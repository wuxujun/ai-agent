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
	start := 0
	for i := len(task.Trace) - 1; i >= 0; i-- {
		if task.Trace[i].Action == "context_summary" {
			fmt.Fprintf(&trace, "Previous summary: %s\n", task.Trace[i].Observation)
			start = i + 1
			break
		}
	}
	for _, item := range task.Trace[start:] {
		fmt.Fprintf(&trace, "Step %d action=%s query=%s observation=%s error=%s\n", item.Step, item.Action, item.Query, item.Observation, item.Error)
	}
	input := *task
	input.Trace = nil
	input.Goal = "Compress the following execution trace into a concise factual context. Preserve findings, failures, file paths, commands, and unresolved questions. Do not add facts.\n\n" + trace.String()
	answer, usage, err := c.Finalizer.Finalize(ctx, &input)
	answer = truncateRunes(answer, 12000)
	return answer, usage, err
}

func traceSinceSummary(task *types.Task) (summary string, start int) {
	for i := len(task.Trace) - 1; i >= 0; i-- {
		if task.Trace[i].Action == "context_summary" {
			return task.Trace[i].Observation, i + 1
		}
	}
	return "", 0
}

func combinedTraceContext(summary string, traces []types.StepTrace) string {
	var value strings.Builder
	if summary != "" {
		fmt.Fprintf(&value, "Previous summary: %s\n", summary)
	}
	for _, item := range traces {
		fmt.Fprintf(&value, "Step %d action=%s query=%s observation=%s error=%s\n", item.Step, item.Action, item.Query, item.Observation, item.Error)
	}
	return truncateRunes(value.String(), 24000)
}
