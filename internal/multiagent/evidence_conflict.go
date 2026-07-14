package multiagent

import (
	"context"
	"fmt"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/evidenceconflict"
	"github.com/wuxujun/ai-agent/internal/evidencefilter"
	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/sourcecredibility"
	"github.com/wuxujun/ai-agent/internal/types"
)

func (c *Coordinator) resolveEvidenceConflicts(ctx context.Context, task *types.Task, evidence []StepEvidence) *StepEvidence {
	if c.EvidenceConflictResolver == nil {
		return nil
	}
	if _, enabled := config.Get().LLM.Scenes[config.LLMSceneEvidenceConflictResolver]; !enabled || !llmcore.AllowedForTask(config.LLMSceneEvidenceConflictResolver, task) {
		return nil
	}
	sources := evidenceSourcesFromResearch(evidence)
	if len(sources) < 2 {
		return nil
	}
	fingerprint := evidenceconflict.Fingerprint(sources)
	for i := len(task.Trace) - 1; i >= 0; i-- {
		trace := task.Trace[i]
		if trace.Action == evidenceconflict.TraceAction && trace.Query == fingerprint {
			return conflictStepEvidence(trace, findCredibilityTrace(task, fingerprint))
		}
	}
	result, usage, err := c.EvidenceConflictResolver.Resolve(ctx, task, sources)
	trace := evidenceconflict.NewTrace(task.StepCount, sources, result, usage, err)
	trace.AgentRole = RoleResearcher
	task.Trace = append(task.Trace, trace)
	if err != nil {
		log.Warn("Evidence conflict resolver failed; preserving all sources", "task_id", task.ID, "error", err)
	}
	if c.Metrics != nil {
		c.Metrics.ObserveTokens(usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens, "evidence_conflict_resolver")
	}
	var scoreTrace *types.StepTrace
	if err == nil && result != nil && len(result.Conflicts) > 0 && c.SourceCredibilityScorer != nil {
		if _, enabled := config.Get().LLM.Scenes[config.LLMSceneSourceCredibilityScorer]; enabled && llmcore.AllowedForTask(config.LLMSceneSourceCredibilityScorer, task) {
			scores, scoreUsage, scoreErr := c.SourceCredibilityScorer.Score(ctx, task, sources, result.Conflicts)
			created := sourcecredibility.NewTrace(task.StepCount, fingerprint, scores, scoreUsage, scoreErr)
			created.AgentRole = RoleResearcher
			task.Trace = append(task.Trace, created)
			scoreTrace = &created
			if scoreErr != nil {
				log.Warn("Source credibility scorer failed; preserving neutral conflict annotation", "task_id", task.ID, "error", scoreErr)
			}
			if c.Metrics != nil {
				c.Metrics.ObserveTokens(scoreUsage.PromptTokens, scoreUsage.CompletionTokens, scoreUsage.TotalTokens, "source_credibility_scorer")
			}
		}
	}
	return conflictStepEvidence(trace, scoreTrace)
}

func evidenceSourcesFromResearch(evidence []StepEvidence) []evidenceconflict.Source {
	var sources []evidenceconflict.Source
	for evidenceIndex, item := range evidence {
		if item.Failed || !evidencefilter.Eligible(item.Action, item.Observation) {
			continue
		}
		for _, fragment := range evidencefilter.Extract(item.Observation, item.Evidence) {
			sources = append(sources, evidenceconflict.Source{
				ID:      fmt.Sprintf("research:%d:%s:%s", evidenceIndex, item.StepID, fragment.ID),
				Origin:  firstResearchOrigin(fragment.Path, item.StepID, item.Action),
				Content: fragment.Text,
			})
		}
	}
	return evidenceconflict.LimitSources(sources, 24)
}

func conflictStepEvidence(trace types.StepTrace, credibility *types.StepTrace) *StepEvidence {
	if len(trace.Evidence) == 0 {
		return nil
	}
	combined := append([]types.Evidence(nil), trace.Evidence...)
	observation := trace.Observation
	usage := trace.TokenUsage
	if credibility != nil && len(credibility.Evidence) > 0 {
		combined = append(combined, credibility.Evidence...)
		observation += " | " + credibility.Observation
		usage.PromptTokens += credibility.TokenUsage.PromptTokens
		usage.CompletionTokens += credibility.TokenUsage.CompletionTokens
		usage.TotalTokens += credibility.TokenUsage.TotalTokens
	}
	return &StepEvidence{
		StepID:      "evidence-conflicts",
		StepDesc:    "External evidence conflict annotations",
		Action:      evidenceconflict.TraceAction,
		Observation: observation,
		Evidence:    combined,
		TokenUsage:  usage,
	}
}

func findCredibilityTrace(task *types.Task, fingerprint string) *types.StepTrace {
	for i := len(task.Trace) - 1; i >= 0; i-- {
		if task.Trace[i].Action == sourcecredibility.TraceAction && task.Trace[i].Query == fingerprint {
			trace := task.Trace[i]
			return &trace
		}
	}
	return nil
}

func firstResearchOrigin(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return "external research evidence"
}
