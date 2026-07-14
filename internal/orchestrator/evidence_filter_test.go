package orchestrator

import (
	"context"
	"testing"

	"github.com/wuxujun/ai-agent/internal/evidencefilter"
	"github.com/wuxujun/ai-agent/internal/types"
)

type fixedEvidenceFilter struct{ calls int }

func (f *fixedEvidenceFilter) Filter(_ context.Context, _ *types.Task, _ string, fragments []evidencefilter.Fragment) (*evidencefilter.Result, types.TokenUsage, error) {
	f.calls++
	decisions := make([]evidencefilter.Decision, len(fragments))
	for i, fragment := range fragments {
		decisions[i] = evidencefilter.Decision{FragmentID: fragment.ID, Keep: i > 0, Relevance: "high", Reason: "relevant"}
		if i == 0 {
			decisions[i].Relevance = "low"
		}
	}
	return &evidencefilter.Result{Decisions: decisions}, types.TokenUsage{TotalTokens: 5}, nil
}

func TestFilterExternalTracesRunsBeforePlannerContext(t *testing.T) {
	filter := &fixedEvidenceFilter{}
	engine := &Engine{EvidenceRelevanceFilter: filter}
	traces := []types.StepTrace{{Action: "web_search", Query: "go", Observation: "advertisement\nrelevant result"}, {Action: "read_file", Observation: "local"}}
	got, audits := engine.filterExternalTraces(context.Background(), &types.Task{}, traces)
	if filter.calls != 1 || got[0].Observation != "relevant result" || got[1].Observation != "local" || len(audits) != 1 || audits[0].Action != evidencefilter.TraceAction || audits[0].TokenUsage.TotalTokens != 5 {
		t.Fatalf("calls=%d traces=%+v audits=%+v", filter.calls, got, audits)
	}
}

func TestFilterExternalTracesSkipsPromptGuardQuarantine(t *testing.T) {
	filter := &fixedEvidenceFilter{}
	engine := &Engine{EvidenceRelevanceFilter: filter}
	traces := []types.StepTrace{{Action: "http_fetch", Observation: "external content quarantined (policy_bypass)"}}
	_, audits := engine.filterExternalTraces(context.Background(), &types.Task{}, traces)
	if filter.calls != 0 || len(audits) != 0 {
		t.Fatalf("calls=%d audits=%+v", filter.calls, audits)
	}
}
