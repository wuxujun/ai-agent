package orchestrator

import (
	"context"
	"errors"
	"testing"

	"github.com/wuxujun/ai-agent/internal/memory"
	"github.com/wuxujun/ai-agent/internal/types"
)

type conflictResult struct {
	resolution *memory.ConflictResolution
	err        error
}

type stubConflictResolver struct {
	results []conflictResult
	calls   int
}

func (s *stubConflictResolver) Resolve(context.Context, *types.Task) (*memory.ConflictResolution, types.TokenUsage, error) {
	result := s.results[s.calls]
	s.calls++
	return result.resolution, types.TokenUsage{TotalTokens: 7}, result.err
}

func TestMemoryConflictResolutionRunsOncePerEvidenceVersion(t *testing.T) {
	resolver := &stubConflictResolver{results: []conflictResult{
		{resolution: &memory.ConflictResolution{Memories: []types.Memory{{ID: "new"}}, Dropped: 1, ConflictCount: 1}},
		{resolution: &memory.ConflictResolution{Memories: []types.Memory{{ID: "new"}}}},
	}}
	engine := &Engine{MemoryConflictResolver: resolver, LLMSceneEnabled: func(string) bool { return true }}
	task := &types.Task{ID: "conflicts", Memories: []types.Memory{{ID: "old"}, {ID: "new"}}}
	engine.ResolveMemoryConflicts(context.Background(), task)
	engine.ResolveMemoryConflicts(context.Background(), task)
	if resolver.calls != 1 || len(task.Memories) != 1 || task.Memories[0].ID != "new" {
		t.Fatalf("calls=%d memories=%+v", resolver.calls, task.Memories)
	}
	task.Trace = append(task.Trace, types.StepTrace{Evidence: []types.Evidence{{Lines: []string{"current fact"}}}})
	engine.ResolveMemoryConflicts(context.Background(), task)
	if resolver.calls != 2 {
		t.Fatalf("new evidence did not trigger resolution: calls=%d trace=%+v", resolver.calls, task.Trace)
	}
}

func TestMemoryConflictResolutionFailsOpen(t *testing.T) {
	resolver := &stubConflictResolver{results: []conflictResult{{err: errors.New("unavailable")}}}
	engine := &Engine{MemoryConflictResolver: resolver, LLMSceneEnabled: func(string) bool { return true }}
	task := &types.Task{ID: "conflict-failure", Memories: []types.Memory{{ID: "one"}, {ID: "two"}}}
	engine.ResolveMemoryConflicts(context.Background(), task)
	engine.ResolveMemoryConflicts(context.Background(), task)
	if resolver.calls != 1 || len(task.Memories) != 2 || !taskHasAction(task, memory.ConflictResolutionTraceAction) {
		t.Fatalf("calls=%d task=%+v", resolver.calls, task)
	}
}
