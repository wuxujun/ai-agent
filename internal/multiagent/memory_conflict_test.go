package multiagent

import (
	"context"
	"testing"

	"github.com/wuxujun/ai-agent/internal/types"
)

type memoryPlanStub struct{}

func (memoryPlanStub) Plan(context.Context, string, string, []types.Memory) (*ResearchPlan, error) {
	return &ResearchPlan{ThoughtSummary: "read", Steps: []ResearchStep{{ID: "step-1", Action: "read_file", FilePath: "fact.txt"}}}, nil
}

func (memoryPlanStub) Replan(context.Context, string, string, []types.StepTrace, []types.Memory) (*ResearchPlan, error) {
	return &ResearchPlan{}, nil
}

type memoryResearchStub struct{}

func (memoryResearchStub) Research(context.Context, string, ResearchStep) (*StepEvidence, error) {
	return &StepEvidence{StepID: "step-1", Action: "read_file", Observation: "current", Evidence: []types.Evidence{{Path: "fact.txt", Lines: []string{"current fact"}}}}, nil
}

type memoryWriterStub struct{ memories []types.Memory }

func (w *memoryWriterStub) Write(_ context.Context, _ string, _ []StepEvidence, memories []types.Memory) (*WriterOutput, error) {
	w.memories = append([]types.Memory(nil), memories...)
	return &WriterOutput{FinalAnswer: "done", Confidence: "high"}, nil
}

func TestCoordinatorResolvesMemoryConflictsAfterResearchBeforeWriter(t *testing.T) {
	writer := &memoryWriterStub{}
	resolverCalls := 0
	coordinator := &Coordinator{
		Planner:    memoryPlanStub{},
		Researcher: memoryResearchStub{},
		Writer:     writer,
		ResolveMemoryConflicts: func(_ context.Context, task *types.Task) {
			resolverCalls++
			if len(task.Trace) < 2 || len(task.Trace[len(task.Trace)-1].Evidence) == 0 {
				t.Fatal("resolver ran before research evidence was recorded")
			}
			task.Memories = task.Memories[1:]
		},
	}
	task := &types.Task{ID: "multi-memory", Goal: "check", Status: types.StatusCreated, MaxSteps: 5, ToolBudget: 2, Memories: []types.Memory{{ID: "old"}, {ID: "current"}}}
	if err := coordinator.Run(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if resolverCalls != 1 || len(writer.memories) != 1 || writer.memories[0].ID != "current" {
		t.Fatalf("resolver_calls=%d writer_memories=%+v", resolverCalls, writer.memories)
	}
}
