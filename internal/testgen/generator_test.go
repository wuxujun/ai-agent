package testgen

import (
	"context"
	"strings"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/review"
	"github.com/wuxujun/ai-agent/internal/types"
)

type generatorCaller struct {
	config llm.Config
	prompt string
	result Result
}

func (c *generatorCaller) CallJSON(_ context.Context, cfg llm.Config, _, prompt string, _ map[string]any, dest any) (types.TokenUsage, error) {
	c.config, c.prompt = cfg, prompt
	*dest.(*Result) = c.result
	return types.TokenUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}, nil
}

func TestLLMGeneratorUsesReviewFindingsAndValidatesOutput(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.LLM.Provider = "openai-responses"
		cfg.LLM.Model = "default"
		cfg.LLM.Scenes = map[string]config.LLMEndpointConfig{config.LLMSceneTestGenerator: {Model: "test-model"}}
	}))
	caller := &generatorCaller{result: Result{Summary: "add regression coverage", Suggestions: []Suggestion{{Priority: "p1", Path: "main_test.go", Framework: "go test", Name: "TestNilInput", Covers: "nil input", Rationale: "prevents the reported panic", SuggestedCode: "func TestNilInput(t *testing.T) {}"}}}}
	ctx := llm.WithRuntime(context.Background(), llm.NewRuntime(caller, nil))
	task := &types.Task{Goal: "fix nil handling", Trace: []types.StepTrace{{Action: "code_review", Evidence: []types.Evidence{{Path: "main.go", Lines: []string{"[high] nil dereference"}}}}}}
	result, usage, err := NewLLMGenerator(config.LLMSceneTestGenerator).Generate(ctx, task, review.ChangeSet{Paths: []string{"main.go"}, Diff: "+if value == nil"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Suggestions) != 1 || usage.TotalTokens != 15 || caller.config.Scene != config.LLMSceneTestGenerator || !strings.Contains(caller.prompt, "nil dereference") {
		t.Fatalf("result=%+v usage=%+v config=%+v prompt=%q", result, usage, caller.config, caller.prompt)
	}
	caller.result.Suggestions[0].Path = "../escape_test.go"
	if _, _, err := NewLLMGenerator(config.LLMSceneTestGenerator).Generate(ctx, task, review.ChangeSet{Paths: []string{"main.go"}, Diff: "+change"}); err == nil {
		t.Fatal("expected unsafe test path to be rejected")
	}
}

func TestValidTestPath(t *testing.T) {
	for _, path := range []string{"main_test.go", "tests/test_api.py", "src/api.test.ts"} {
		if !validTestPath(path) {
			t.Errorf("valid test path rejected: %s", path)
		}
	}
	for _, path := range []string{"", "../escape_test.go", "/tmp/test.py", "test.txt", "bad\nname.go"} {
		if validTestPath(path) {
			t.Errorf("invalid test path accepted: %q", path)
		}
	}
}
