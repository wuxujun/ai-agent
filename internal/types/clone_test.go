package types

import "testing"

func TestCloneTaskDetachesNestedState(t *testing.T) {
	original := &Task{
		Trace:       []StepTrace{{Evidence: []Evidence{{Lines: []string{"line"}}}}},
		Memories:    []Memory{{Embedding: []float32{1}}},
		AnswerAudit: &AnswerAuditReport{Stages: []AnswerAuditStage{{Findings: []AnswerAuditFinding{{Detail: "detail"}}}}},
	}
	cloned := CloneTask(original)
	cloned.Trace[0].Evidence[0].Lines[0] = "changed"
	cloned.Memories[0].Embedding[0] = 2
	cloned.AnswerAudit.Stages[0].Findings[0].Detail = "changed"
	if original.Trace[0].Evidence[0].Lines[0] != "line" || original.Memories[0].Embedding[0] != 1 || original.AnswerAudit.Stages[0].Findings[0].Detail != "detail" {
		t.Fatalf("clone shares nested state with original: %+v", original)
	}
}
