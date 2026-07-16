package planner

import (
	"strings"
	"testing"

	"github.com/wuxujun/ai-agent/internal/types"
)

func TestBuildFinalizerEvidenceIncludesFetchedEvidenceContent(t *testing.T) {
	task := &types.Task{Trace: []types.StepTrace{{
		Step: 2, Action: "rag_fetch", Observation: "fetched 1 rag item(s)",
		Evidence: []types.Evidence{{Path: "rag-1", Query: "学术顾问", Lines: []string{"顾问姓名：张三"}}},
	}}}
	got := buildFinalizerEvidence(task)
	for _, want := range []string{"fetched 1 rag item(s)", "rag-1", "学术顾问", "顾问姓名：张三"} {
		if !strings.Contains(got, want) {
			t.Fatalf("finalizer evidence missing %q: %s", want, got)
		}
	}
}
