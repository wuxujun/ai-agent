package orchestrator

import (
	"context"
	"errors"
	"testing"

	"github.com/wuxujun/ai-agent/internal/policy"
	"github.com/wuxujun/ai-agent/internal/types"
)

type safetyResult struct {
	decision *policy.SafetyDecision
	usage    types.TokenUsage
	err      error
}

type stubSafetyGuard struct {
	results []safetyResult
	stages  []policy.SafetyStage
}

func (s *stubSafetyGuard) Evaluate(_ context.Context, stage policy.SafetyStage, _ *types.Task, _ string) (*policy.SafetyDecision, types.TokenUsage, error) {
	s.stages = append(s.stages, stage)
	result := s.results[len(s.stages)-1]
	return result.decision, result.usage, result.err
}

func TestSafetyGuardChecksInputThenFinalOutput(t *testing.T) {
	guard := &stubSafetyGuard{results: []safetyResult{
		{decision: &policy.SafetyDecision{Allowed: true, SafeText: "original"}, usage: types.TokenUsage{TotalTokens: 3}},
		{decision: &policy.SafetyDecision{Allowed: true, SafeText: "redacted final"}, usage: types.TokenUsage{TotalTokens: 4}},
	}}
	engine := &Engine{
		Planner:         stopPlanner{},
		SafetyGuard:     guard,
		Mode:            ModeLegacy,
		LLMSceneEnabled: func(string) bool { return true },
	}
	task := &types.Task{ID: "safe", Goal: "goal", Status: types.StatusRunning, MaxSteps: 5, StepCount: 1, ToolBudget: 1}
	if err := engine.Next(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if task.FinalAnswer != "redacted final" || len(guard.stages) != 2 || guard.stages[0] != policy.SafetyStageInput || guard.stages[1] != policy.SafetyStageOutput {
		t.Fatalf("answer=%q stages=%v", task.FinalAnswer, guard.stages)
	}
	if !taskHasAction(task, "safety_guard_input") || !taskHasAction(task, "safety_guard_output") {
		t.Fatalf("safety traces missing: %+v", task.Trace)
	}
}

func TestSafetyGuardBlockedInputStopsBeforePlanner(t *testing.T) {
	guard := &stubSafetyGuard{results: []safetyResult{{decision: &policy.SafetyDecision{
		Allowed: false, SafeText: "request refused", Categories: []string{"malware"},
	}}}}
	engine := &Engine{Planner: stopPlanner{}, SafetyGuard: guard, Mode: ModeLegacy, LLMSceneEnabled: func(string) bool { return true }}
	task := &types.Task{ID: "blocked", Goal: "unsafe", Status: types.StatusCreated, MaxSteps: 5, ToolBudget: 1}
	if err := engine.Next(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if task.Status != types.StatusCompleted || task.FinalAnswer != "request refused" || len(guard.stages) != 1 {
		t.Fatalf("task=%+v stages=%v", task, guard.stages)
	}
	if taskHasAction(task, "stop") || taskHasAction(task, "safety_guard_output") {
		t.Fatalf("blocked input continued execution: %+v", task.Trace)
	}
}

func TestSafetyGuardInputFailureIsAttemptedOnlyOnce(t *testing.T) {
	guard := &stubSafetyGuard{results: []safetyResult{{err: errors.New("unavailable")}}}
	engine := &Engine{SafetyGuard: guard, LLMSceneEnabled: func(string) bool { return true }}
	task := &types.Task{ID: "failed", Goal: "goal"}
	if !engine.guardInput(context.Background(), task) || !engine.guardInput(context.Background(), task) {
		t.Fatal("failed optional guard should remain fail-open")
	}
	if len(guard.stages) != 1 || !taskHasAction(task, "safety_guard_input") {
		t.Fatalf("stages=%v trace=%+v", guard.stages, task.Trace)
	}
}
