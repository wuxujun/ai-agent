package orchestrator

import (
	"context"
	"fmt"
	"strings"

	"github.com/wuxujun/ai-agent/internal/promptguard"
	"github.com/wuxujun/ai-agent/internal/types"
)

func (e *Engine) inspectExternalTraces(ctx context.Context, task *types.Task, traces []types.StepTrace) ([]types.StepTrace, *types.StepTrace) {
	if e.PromptInjectionDetector == nil {
		return traces, nil
	}
	var sources []promptguard.Source
	indices := make(map[string]int)
	for i := range traces {
		if !promptguard.IsExternalAction(traces[i].Action) || traces[i].Error != "" {
			continue
		}
		id := fmt.Sprintf("tool:%d:%s", i, traces[i].Action)
		sources = append(sources, promptguard.Source{ID: id, Kind: traces[i].Action, Text: promptguard.EvidenceText(traces[i].Observation, traces[i].Evidence)})
		indices[id] = i
	}
	if len(sources) == 0 {
		return traces, nil
	}
	result, usage, err := e.PromptInjectionDetector.Detect(ctx, task, sources)
	for sourceID, index := range indices {
		if finding, quarantined := promptguard.Quarantined(result, sourceID); quarantined {
			traces[index].Observation = fmt.Sprintf("external content quarantined (%s)", finding.Category)
			traces[index].Evidence = promptguard.QuarantineEvidence(traces[index].Evidence)
		}
	}
	if err != nil {
		engineLog.Warn("prompt injection model check failed; deterministic result retained", "task_id", task.ID, "error", err)
	}
	audit := promptguard.NewAuditTrace(task.StepCount+len(traces), "tool_output", len(sources), result, usage, err)
	if e.Metrics != nil {
		e.Metrics.ObserveTokens(usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens, "prompt_injection_detector")
	}
	return traces, &audit
}

func (e *Engine) inspectExternalMemories(ctx context.Context, task *types.Task, memories []types.Memory) []types.Memory {
	if e.PromptInjectionDetector == nil || len(memories) == 0 {
		return memories
	}
	sources := make([]promptguard.Source, 0, len(memories))
	for i, item := range memories {
		sources = append(sources, promptguard.Source{
			ID:   fmt.Sprintf("rag:%d", i),
			Kind: "third_party_rag",
			Text: strings.Join([]string{item.Goal, item.KeyFindings, item.FinalAnswer}, "\n"),
		})
	}
	result, usage, err := e.PromptInjectionDetector.Detect(ctx, task, sources)
	kept := make([]types.Memory, 0, len(memories))
	for i, item := range memories {
		if _, quarantined := promptguard.Quarantined(result, fmt.Sprintf("rag:%d", i)); !quarantined {
			kept = append(kept, item)
		}
	}
	if err != nil {
		engineLog.Warn("RAG prompt injection model check failed; deterministic result retained", "task_id", task.ID, "error", err)
	}
	task.Trace = append(task.Trace, promptguard.NewAuditTrace(task.StepCount, "third_party_rag", len(sources), result, usage, err))
	if e.Metrics != nil {
		e.Metrics.ObserveTokens(usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens, "prompt_injection_detector")
	}
	return kept
}
