package types

import (
	"testing"
	"time"
)

func TestCloneTaskDetachesNestedState(t *testing.T) {
	started := time.Date(2026, time.October, 7, 12, 0, 0, 0, time.UTC)
	duration := int64(25)
	original := &Task{
		Trace:       []StepTrace{{Evidence: []Evidence{{Lines: []string{"line"}}}, OccurredAt: &started, DurationMS: &duration}},
		Memories:    []Memory{{Embedding: []float32{1}}},
		AnswerAudit: &AnswerAuditReport{Stages: []AnswerAuditStage{{Findings: []AnswerAuditFinding{{Detail: "detail"}}}}},
	}
	cloned := CloneTask(original)
	cloned.Trace[0].Evidence[0].Lines[0] = "changed"
	*cloned.Trace[0].OccurredAt = started.Add(time.Hour)
	*cloned.Trace[0].DurationMS = 99
	cloned.Memories[0].Embedding[0] = 2
	cloned.AnswerAudit.Stages[0].Findings[0].Detail = "changed"
	if original.Trace[0].Evidence[0].Lines[0] != "line" || !original.Trace[0].OccurredAt.Equal(started) || *original.Trace[0].DurationMS != 25 ||
		original.Memories[0].Embedding[0] != 1 || original.AnswerAudit.Stages[0].Findings[0].Detail != "detail" {
		t.Fatalf("clone shares nested state with original: %+v", original)
	}
}

func TestStepTraceSetExecutionTiming(t *testing.T) {
	start := time.Date(2026, time.October, 7, 12, 0, 0, 0, time.FixedZone("UTC+2", 2*60*60))
	var trace StepTrace
	trace.SetExecutionTiming(start, 1500*time.Microsecond)
	if trace.OccurredAt == nil || trace.OccurredAt.Location() != time.UTC || !trace.OccurredAt.Equal(start) ||
		trace.DurationMS == nil || *trace.DurationMS != 1 {
		t.Fatalf("timing = %+v", trace)
	}
	var unknown StepTrace
	unknown.SetExecutionTiming(time.Time{}, time.Second)
	if unknown.OccurredAt != nil || unknown.DurationMS != nil {
		t.Fatalf("zero start produced timing: %+v", unknown)
	}
}
