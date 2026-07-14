package orchestrator

import (
	"context"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/evidenceconflict"
	"github.com/wuxujun/ai-agent/internal/sourcecredibility"
	"github.com/wuxujun/ai-agent/internal/types"
)

type fixedEvidenceConflictResolver struct {
	calls   int
	sources []evidenceconflict.Source
}

type fixedCredibilityScorer struct{ calls int }

func (s *fixedCredibilityScorer) Score(_ context.Context, _ *types.Task, _ []evidenceconflict.Source, conflicts []evidenceconflict.Conflict) (*sourcecredibility.Result, types.TokenUsage, error) {
	s.calls++
	return &sourcecredibility.Result{Scores: []sourcecredibility.Score{
		{SourceID: conflicts[0].SourceIDs[0], Authority: "high", Freshness: "current", Traceability: "primary", Overall: "high", Rationale: "official"},
		{SourceID: conflicts[0].SourceIDs[1], Authority: "unknown", Freshness: "unknown", Traceability: "secondary", Overall: "low", Rationale: "unattributed"},
	}}, types.TokenUsage{TotalTokens: 5}, nil
}

func (r *fixedEvidenceConflictResolver) Resolve(_ context.Context, _ *types.Task, sources []evidenceconflict.Source) (*evidenceconflict.Result, types.TokenUsage, error) {
	r.calls++
	r.sources = append([]evidenceconflict.Source(nil), sources...)
	return &evidenceconflict.Result{Conflicts: []evidenceconflict.Conflict{{SourceIDs: []string{sources[0].ID, sources[1].ID}, Severity: "material", Topic: "version", Explanation: "different versions"}}}, types.TokenUsage{TotalTokens: 6}, nil
}

func TestResolveEvidenceConflictsAddsAdvisoryCredibilityTrace(t *testing.T) {
	resolver := &fixedEvidenceConflictResolver{}
	scorer := &fixedCredibilityScorer{}
	engine := &Engine{
		EvidenceConflictResolver: resolver,
		SourceCredibilityScorer:  scorer,
		LLMSceneEnabled: func(scene string) bool {
			return scene == config.LLMSceneEvidenceConflictResolver || scene == config.LLMSceneSourceCredibilityScorer
		},
	}
	current := []types.StepTrace{{Action: "web_search", Observation: "v1"}, {Action: "http_fetch", Observation: "v2"}}
	audits := engine.resolveEvidenceConflicts(context.Background(), &types.Task{}, current)
	if scorer.calls != 1 || len(audits) != 2 || audits[1].Action != sourcecredibility.TraceAction || audits[1].TokenUsage.TotalTokens != 5 || len(audits[1].Evidence) != 2 {
		t.Fatalf("calls=%d audits=%+v", scorer.calls, audits)
	}
	if current[0].Observation != "v1" || current[1].Observation != "v2" {
		t.Fatalf("sources changed: %+v", current)
	}
}

func TestResolveEvidenceConflictsUsesFilteredExternalSourcesAndDeduplicates(t *testing.T) {
	resolver := &fixedEvidenceConflictResolver{}
	engine := &Engine{EvidenceConflictResolver: resolver, LLMSceneEnabled: func(scene string) bool { return scene == config.LLMSceneEvidenceConflictResolver }}
	task := &types.Task{Trace: []types.StepTrace{{Action: "web_search", Query: "version", Observation: "version is 1"}}}
	current := []types.StepTrace{{Action: "http_fetch", Query: "docs", Observation: "version is 2"}, {Action: "read_file", Observation: "local content"}}
	audits := engine.resolveEvidenceConflicts(context.Background(), task, current)
	if resolver.calls != 1 || len(resolver.sources) != 2 || len(audits) != 1 || audits[0].Action != evidenceconflict.TraceAction || audits[0].TokenUsage.TotalTokens != 6 || len(audits[0].Evidence) != 1 {
		t.Fatalf("calls=%d sources=%+v audits=%+v", resolver.calls, resolver.sources, audits)
	}
	task.Trace = append(task.Trace, current...)
	task.Trace = append(task.Trace, audits...)
	if second := engine.resolveEvidenceConflicts(context.Background(), task, nil); len(second) != 0 || resolver.calls != 1 {
		t.Fatalf("second=%+v calls=%d", second, resolver.calls)
	}
	if current[0].Observation != "version is 2" {
		t.Fatalf("source was modified: %+v", current)
	}
}

func TestEvidenceSourcesSkipQuarantinedContent(t *testing.T) {
	sources := evidenceSourcesFromTraces([]types.StepTrace{{Action: "web_search", Observation: "external content quarantined (instruction_override)"}, {Action: "http_fetch", Observation: "safe fact"}})
	if len(sources) != 1 || sources[0].Content != "safe fact" {
		t.Fatalf("sources=%+v", sources)
	}
}
