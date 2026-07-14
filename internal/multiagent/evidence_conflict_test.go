package multiagent

import (
	"context"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/evidenceconflict"
	"github.com/wuxujun/ai-agent/internal/sourcecredibility"
	"github.com/wuxujun/ai-agent/internal/types"
)

type multiEvidenceConflictResolver struct{ calls int }

type multiCredibilityScorer struct{ calls int }

func (s *multiCredibilityScorer) Score(_ context.Context, _ *types.Task, _ []evidenceconflict.Source, conflicts []evidenceconflict.Conflict) (*sourcecredibility.Result, types.TokenUsage, error) {
	s.calls++
	return &sourcecredibility.Result{Scores: []sourcecredibility.Score{
		{SourceID: conflicts[0].SourceIDs[0], Authority: "high", Freshness: "current", Traceability: "primary", Overall: "high", Rationale: "official"},
		{SourceID: conflicts[0].SourceIDs[1], Authority: "low", Freshness: "unknown", Traceability: "secondary", Overall: "low", Rationale: "blog"},
	}}, types.TokenUsage{TotalTokens: 5}, nil
}

func (r *multiEvidenceConflictResolver) Resolve(_ context.Context, _ *types.Task, sources []evidenceconflict.Source) (*evidenceconflict.Result, types.TokenUsage, error) {
	r.calls++
	return &evidenceconflict.Result{Conflicts: []evidenceconflict.Conflict{{SourceIDs: []string{sources[0].ID, sources[1].ID}, Severity: "material", Topic: "status", Explanation: "different status"}}}, types.TokenUsage{TotalTokens: 7}, nil
}

func TestCoordinatorAnnotatesConflictsBeforeWriterWithoutDroppingSources(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.LLM.Scenes = map[string]config.LLMEndpointConfig{config.LLMSceneEvidenceConflictResolver: {}, config.LLMSceneSourceCredibilityScorer: {}}
	}))
	resolver := &multiEvidenceConflictResolver{}
	scorer := &multiCredibilityScorer{}
	coordinator := &Coordinator{EvidenceConflictResolver: resolver, SourceCredibilityScorer: scorer}
	evidence := []StepEvidence{{StepID: "s1", Action: "web_search", Observation: "feature enabled"}, {StepID: "s2", Action: "http_fetch", Observation: "feature disabled"}}
	task := &types.Task{}
	annotation := coordinator.resolveEvidenceConflicts(context.Background(), task, evidence)
	if resolver.calls != 1 || scorer.calls != 1 || annotation == nil || annotation.Action != evidenceconflict.TraceAction || len(annotation.Evidence) != 3 || len(task.Trace) != 2 || task.Trace[0].TokenUsage.TotalTokens != 7 || task.Trace[1].Action != sourcecredibility.TraceAction {
		t.Fatalf("calls=%d annotation=%+v task=%+v", resolver.calls, annotation, task)
	}
	if evidence[0].Observation != "feature enabled" || evidence[1].Observation != "feature disabled" {
		t.Fatalf("original evidence changed: %+v", evidence)
	}
	second := coordinator.resolveEvidenceConflicts(context.Background(), task, evidence)
	if resolver.calls != 1 || scorer.calls != 1 || second == nil || len(second.Evidence) != 3 {
		t.Fatalf("calls=%d second=%+v", resolver.calls, second)
	}
}
