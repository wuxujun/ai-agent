package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/review"
	"github.com/wuxujun/ai-agent/internal/testgen"
	"github.com/wuxujun/ai-agent/internal/types"
)

type stubTestGenerator struct {
	result *testgen.Result
	usage  types.TokenUsage
}

type recordingTestGenerator struct{ sawReview *bool }

func (g recordingTestGenerator) Generate(_ context.Context, task *types.Task, _ review.ChangeSet) (*testgen.Result, types.TokenUsage, error) {
	*g.sawReview = taskHasAction(task, "code_review")
	return &testgen.Result{Summary: "covered"}, types.TokenUsage{}, nil
}

func (s stubTestGenerator) Generate(context.Context, *types.Task, review.ChangeSet) (*testgen.Result, types.TokenUsage, error) {
	return s.result, s.usage, nil
}

func TestGenerateTestSuggestionsAppendsAnswerAndEvidence(t *testing.T) {
	engine := &Engine{
		TestGenerator:   stubTestGenerator{result: &testgen.Result{Summary: "one test", Suggestions: []testgen.Suggestion{{Priority: "p1", Path: "main_test.go", Framework: "go test", Name: "TestFailure", Covers: "error path", Rationale: "prevents regression", SuggestedCode: "func TestFailure(t *testing.T) {}"}}}, usage: types.TokenUsage{TotalTokens: 11}},
		LLMSceneEnabled: func(scene string) bool { return scene == config.LLMSceneTestGenerator },
		CollectCodeChanges: func(context.Context, string) (review.ChangeSet, error) {
			return review.ChangeSet{Paths: []string{"main.go"}, Diff: "+ risky()"}, nil
		},
	}
	task := &types.Task{FinalAnswer: "implemented", Trace: []types.StepTrace{{Action: "apply_patch"}}}
	engine.generateTestSuggestions(context.Background(), task)
	if !strings.Contains(task.FinalAnswer, "## Suggested tests") || !strings.Contains(task.FinalAnswer, "main_test.go") || !strings.Contains(task.FinalAnswer, "func TestFailure") {
		t.Fatalf("final answer=%q", task.FinalAnswer)
	}
	last := task.Trace[len(task.Trace)-1]
	if last.Action != "test_generate" || last.TokenUsage.TotalTokens != 11 || len(last.Evidence) != 1 {
		t.Fatalf("trace=%+v", last)
	}
	engine.generateTestSuggestions(context.Background(), task)
	if len(task.Trace) != 2 {
		t.Fatalf("test generation should run once, traces=%d", len(task.Trace))
	}
}

func TestRunCodeQualityGatesSharesChangesAndOrdersReviewFirst(t *testing.T) {
	collections := 0
	sawReview := false
	engine := &Engine{
		CodeReviewer:  stubCodeReviewer{result: &review.Result{Summary: "clean"}},
		TestGenerator: recordingTestGenerator{sawReview: &sawReview},
		LLMSceneEnabled: func(scene string) bool {
			return scene == config.LLMSceneCodeReviewer || scene == config.LLMSceneTestGenerator
		},
		CollectCodeChanges: func(context.Context, string) (review.ChangeSet, error) {
			collections++
			return review.ChangeSet{Paths: []string{"main.go"}, Diff: "+change"}, nil
		},
	}
	task := &types.Task{Trace: []types.StepTrace{{Action: "write_file"}}}
	engine.runCodeQualityGates(context.Background(), task)
	if collections != 1 || !sawReview || !taskHasAction(task, "test_generate") {
		t.Fatalf("collections=%d saw_review=%t trace=%+v", collections, sawReview, task.Trace)
	}
}
