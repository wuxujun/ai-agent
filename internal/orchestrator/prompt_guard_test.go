package orchestrator

import (
	"context"
	"testing"

	"github.com/wuxujun/ai-agent/internal/promptguard"
	"github.com/wuxujun/ai-agent/internal/types"
)

type fixedInjectionDetector struct {
	findingID string
	calls     int
}

func (d *fixedInjectionDetector) Detect(_ context.Context, _ *types.Task, sources []promptguard.Source) (*promptguard.Result, types.TokenUsage, error) {
	d.calls++
	id := d.findingID
	if id == "" {
		id = sources[0].ID
	}
	return &promptguard.Result{Findings: []promptguard.Finding{{SourceID: id, Risk: "malicious", Category: "instruction_override", Reason: "attempts to replace instructions"}}}, types.TokenUsage{TotalTokens: 5}, nil
}

func TestInspectExternalTracesQuarantinesBeforePlannerContext(t *testing.T) {
	detector := &fixedInjectionDetector{}
	engine := &Engine{PromptInjectionDetector: detector}
	traces := []types.StepTrace{{Action: "read_file", Evidence: []types.Evidence{{Lines: []string{"local"}}}}, {Action: "web_search", Observation: "results", Evidence: []types.Evidence{{Path: "https://example.test", Lines: []string{"unsafe"}}}}}
	got, audit := engine.inspectExternalTraces(context.Background(), &types.Task{StepCount: 2}, traces)
	if detector.calls != 1 || audit == nil || audit.Action != promptguard.TraceAction || audit.TokenUsage.TotalTokens != 5 {
		t.Fatalf("calls=%d audit=%+v", detector.calls, audit)
	}
	if got[0].Evidence[0].Lines[0] != "local" || got[1].Evidence[0].Lines[0] != promptguard.QuarantineMessage {
		t.Fatalf("traces=%+v", got)
	}
}

func TestInspectExternalMemoriesDropsQuarantinedRAGItem(t *testing.T) {
	detector := &fixedInjectionDetector{findingID: "rag:0"}
	task := &types.Task{}
	engine := &Engine{PromptInjectionDetector: detector}
	got := engine.inspectExternalMemories(context.Background(), task, []types.Memory{{ID: "bad", KeyFindings: "unsafe"}, {ID: "good", KeyFindings: "safe"}})
	if len(got) != 1 || got[0].ID != "good" || len(task.Trace) != 1 || task.Trace[0].Action != promptguard.TraceAction {
		t.Fatalf("memories=%+v task=%+v", got, task)
	}
}
