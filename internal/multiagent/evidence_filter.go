package multiagent

import (
	"context"

	"github.com/wuxujun/ai-agent/internal/evidencefilter"
	"github.com/wuxujun/ai-agent/internal/types"
)

func (c *Coordinator) filterStepEvidence(ctx context.Context, task *types.Task, evidence *StepEvidence, failed bool) *types.StepTrace {
	if c.EvidenceRelevanceFilter == nil || evidence == nil || failed || !evidencefilter.Eligible(evidence.Action, evidence.Observation) {
		return nil
	}
	fragments := evidencefilter.Extract(evidence.Observation, evidence.Evidence)
	if len(fragments) == 0 {
		return nil
	}
	result, usage, err := c.EvidenceRelevanceFilter.Filter(ctx, task, evidence.StepDesc, fragments)
	evidence.Observation, evidence.Evidence = evidencefilter.Apply(evidence.Observation, evidence.Evidence, result)
	if err != nil {
		log.Warn("Evidence relevance filter failed; non-duplicate fragments preserved", "task_id", task.ID, "action", evidence.Action, "error", err)
	}
	if c.Metrics != nil {
		c.Metrics.ObserveTokens(usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens, "evidence_relevance_filter")
	}
	audit := evidencefilter.NewAuditTrace(task.StepCount, evidence.Action, result, usage, err)
	audit.AgentRole = RoleResearcher
	return &audit
}
