package orchestrator

import (
	"context"

	"github.com/wuxujun/ai-agent/internal/evidencefilter"
	"github.com/wuxujun/ai-agent/internal/types"
)

func (e *Engine) filterExternalTraces(ctx context.Context, task *types.Task, traces []types.StepTrace) ([]types.StepTrace, []types.StepTrace) {
	if e.EvidenceRelevanceFilter == nil {
		return traces, nil
	}
	var audits []types.StepTrace
	for i := range traces {
		trace := &traces[i]
		if trace.Error != "" || !evidencefilter.Eligible(trace.Action, trace.Observation) {
			continue
		}
		fragments := evidencefilter.Extract(trace.Observation, trace.Evidence)
		if len(fragments) == 0 {
			continue
		}
		result, usage, err := e.EvidenceRelevanceFilter.Filter(ctx, task, trace.Query, fragments)
		trace.Observation, trace.Evidence = evidencefilter.Apply(trace.Observation, trace.Evidence, result)
		if err != nil {
			engineLog.Warn("evidence relevance filter failed; non-duplicate fragments preserved", "task_id", task.ID, "action", trace.Action, "error", err)
		}
		audits = append(audits, evidencefilter.NewAuditTrace(task.StepCount+len(traces), trace.Action, result, usage, err))
		if e.Metrics != nil {
			e.Metrics.ObserveTokens(usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens, "evidence_relevance_filter")
		}
	}
	return traces, audits
}
