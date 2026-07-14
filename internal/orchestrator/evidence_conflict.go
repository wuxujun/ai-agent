package orchestrator

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

func (e *Engine) resolveEvidenceConflicts(ctx context.Context, task *types.Task, current []types.StepTrace) []types.StepTrace {
	if e.EvidenceConflictResolver == nil || !e.llmSceneEnabled(config.LLMSceneEvidenceConflictResolver) || !llmcore.AllowedForTask(config.LLMSceneEvidenceConflictResolver, task) {
		return nil
	}
	all := make([]types.StepTrace, 0, len(task.Trace)+len(current))
	all = append(all, task.Trace...)
	all = append(all, current...)
	sources := evidenceSourcesFromTraces(all)
	if len(sources) < 2 {
		return nil
	}
	fingerprint := evidenceconflict.Fingerprint(sources)
	if evidenceconflict.AlreadyResolved(task, fingerprint) {
		return nil
	}
	result, usage, err := e.EvidenceConflictResolver.Resolve(ctx, task, sources)
	trace := evidenceconflict.NewTrace(task.StepCount+len(current), sources, result, usage, err)
	if err != nil {
		engineLog.Warn("evidence conflict resolver failed; preserving all sources", "task_id", task.ID, "error", err)
	}
	if e.Metrics != nil {
		e.Metrics.ObserveTokens(usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens, "evidence_conflict_resolver")
	}
	audits := []types.StepTrace{trace}
	if err == nil && result != nil && len(result.Conflicts) > 0 && e.SourceCredibilityScorer != nil && e.llmSceneEnabled(config.LLMSceneSourceCredibilityScorer) && llmcore.AllowedForTask(config.LLMSceneSourceCredibilityScorer, task) && !sourcecredibility.AlreadyScored(task, trace.Query) {
		scores, scoreUsage, scoreErr := e.SourceCredibilityScorer.Score(ctx, task, sources, result.Conflicts)
		scoreTrace := sourcecredibility.NewTrace(task.StepCount+len(current), trace.Query, scores, scoreUsage, scoreErr)
		audits = append(audits, scoreTrace)
		if scoreErr != nil {
			engineLog.Warn("source credibility scorer failed; preserving neutral conflict annotation", "task_id", task.ID, "error", scoreErr)
		}
		if e.Metrics != nil {
			e.Metrics.ObserveTokens(scoreUsage.PromptTokens, scoreUsage.CompletionTokens, scoreUsage.TotalTokens, "source_credibility_scorer")
		}
	}
	return audits
}

func evidenceSourcesFromTraces(traces []types.StepTrace) []evidenceconflict.Source {
	var sources []evidenceconflict.Source
	for traceIndex, trace := range traces {
		if trace.Error != "" || !evidencefilter.Eligible(trace.Action, trace.Observation) {
			continue
		}
		for _, fragment := range evidencefilter.Extract(trace.Observation, trace.Evidence) {
			sources = append(sources, evidenceconflict.Source{
				ID:      fmt.Sprintf("trace:%d:%s:%s", traceIndex, trace.Action, fragment.ID),
				Origin:  firstNonEmpty(fragment.Path, trace.Query, trace.Action),
				Content: fragment.Text,
			})
		}
	}
	return evidenceconflict.LimitSources(sources, 24)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return "external evidence"
}
