package multiagent

import (
	"context"
	"testing"

	"github.com/wuxujun/ai-agent/internal/evidencefilter"
	"github.com/wuxujun/ai-agent/internal/types"
)

type multiEvidenceFilter struct{ calls int }

func (f *multiEvidenceFilter) Filter(_ context.Context, _ *types.Task, query string, fragments []evidencefilter.Fragment) (*evidencefilter.Result, types.TokenUsage, error) {
	f.calls++
	decisions := make([]evidencefilter.Decision, len(fragments))
	for i, fragment := range fragments {
		decisions[i] = evidencefilter.Decision{FragmentID: fragment.ID, Keep: i == 0, Relevance: "low", Reason: query}
		if i == 0 {
			decisions[i].Relevance = "high"
		}
	}
	return &evidencefilter.Result{Decisions: decisions}, types.TokenUsage{TotalTokens: 3}, nil
}

func TestCoordinatorFiltersResearchEvidenceBeforeWriter(t *testing.T) {
	filter := &multiEvidenceFilter{}
	coordinator := &Coordinator{EvidenceRelevanceFilter: filter}
	evidence := &StepEvidence{StepID: "s1", StepDesc: "find release", Action: "web_search", Observation: "release fact\nadvertisement"}
	audit := coordinator.filterStepEvidence(context.Background(), &types.Task{}, evidence, false)
	if filter.calls != 1 || evidence.Observation != "release fact" || audit == nil || audit.AgentRole != RoleResearcher || audit.TokenUsage.TotalTokens != 3 {
		t.Fatalf("calls=%d evidence=%+v audit=%+v", filter.calls, evidence, audit)
	}
}
