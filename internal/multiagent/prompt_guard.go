package multiagent

import (
	"context"
	"fmt"

	"github.com/wuxujun/ai-agent/internal/promptguard"
	"github.com/wuxujun/ai-agent/internal/types"
)

func (c *Coordinator) inspectStepEvidence(ctx context.Context, task *types.Task, evidence *StepEvidence, failed bool) *types.StepTrace {
	if c.PromptInjectionDetector == nil || evidence == nil || failed || !promptguard.IsExternalAction(evidence.Action) {
		return nil
	}
	id := fmt.Sprintf("research:%s:%s", evidence.StepID, evidence.Action)
	sources := []promptguard.Source{{ID: id, Kind: evidence.Action, Text: promptguard.EvidenceText(evidence.Observation, evidence.Evidence)}}
	result, usage, err := c.PromptInjectionDetector.Detect(ctx, task, sources)
	if finding, quarantined := promptguard.Quarantined(result, id); quarantined {
		evidence.Observation = fmt.Sprintf("external content quarantined (%s)", finding.Category)
		evidence.Evidence = promptguard.QuarantineEvidence(evidence.Evidence)
	}
	if err != nil {
		log.Warn("Prompt injection model check failed; deterministic result retained", "task_id", task.ID, "error", err)
	}
	if c.Metrics != nil {
		c.Metrics.ObserveTokens(usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens, "prompt_injection_detector")
	}
	audit := promptguard.NewAuditTrace(task.StepCount, "research_output", 1, result, usage, err)
	audit.AgentRole = RoleResearcher
	return &audit
}
