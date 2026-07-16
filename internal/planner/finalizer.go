package planner

import (
	"context"
	"fmt"
	"strings"

	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/types"
)

type TaskFinalizer interface {
	Finalize(ctx context.Context, task *types.Task) (string, types.TokenUsage, error)
}

type LLMTaskFinalizer struct {
	Scene string
}

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

func NewLLMTaskFinalizer(scene string) *LLMTaskFinalizer {
	return &LLMTaskFinalizer{Scene: scene}
}

func buildFinalizerEvidence(task *types.Task) string {
	var evidence strings.Builder
	for _, item := range task.Trace {
		fmt.Fprintf(&evidence, "Step %d action=%s query=%s observation=%s error=%s\n", item.Step, item.Action, item.Query, item.Observation, item.Error)
		for _, ev := range item.Evidence {
			fmt.Fprintf(&evidence, "Evidence path=%s query=%s\n", ev.Path, ev.Query)
			for _, line := range ev.Lines {
				fmt.Fprintf(&evidence, "%s\n", line)
			}
		}
	}
	for _, mem := range task.Memories {
		fmt.Fprintf(&evidence, "Memory goal=%s findings=%s answer=%s\n", mem.Goal, mem.KeyFindings, mem.FinalAnswer)
	}
	return evidence.String()
}

func (f *LLMTaskFinalizer) Finalize(ctx context.Context, task *types.Task) (string, types.TokenUsage, error) {
	var output struct {
		FinalAnswer     string `json:"final_answer"`
		EvidenceSummary string `json:"evidence_summary"`
		Confidence      string `json:"confidence"`
	}
	schema := map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"final_answer":     map[string]any{"type": "string"},
			"evidence_summary": map[string]any{"type": "string", "maxLength": 2000},
			"confidence":       map[string]any{"type": "string", "enum": []string{"high", "medium", "low"}},
		},
		"required": []string{"final_answer", "evidence_summary", "confidence"},
	}
	prompt := fmt.Sprintf("Original goal: %s\n\nEvidence:\n%s", task.Goal, truncateRunes(buildFinalizerEvidence(task), 64000))
	usage, err := llmcore.CallJSON(ctx, llmcore.ConfigForScene(f.Scene), "Synthesize a self-contained final answer using only the supplied evidence. State uncertainty when evidence is incomplete. Return JSON only.", prompt, schema, &output)
	if err != nil {
		return "", types.TokenUsage{}, err
	}
	if strings.TrimSpace(output.FinalAnswer) == "" {
		return "", usage, fmt.Errorf("task finalizer returned an empty final answer")
	}
	return output.FinalAnswer, usage, nil
}
