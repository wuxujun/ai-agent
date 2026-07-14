package review

import (
	"context"
	"strings"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/types"
)

type reviewerCaller struct {
	config llm.Config
	prompt string
	result Result
}

func (c *reviewerCaller) CallJSON(_ context.Context, cfg llm.Config, _, prompt string, _ map[string]any, dest any) (types.TokenUsage, error) {
	c.config, c.prompt = cfg, prompt
	*dest.(*Result) = c.result
	return types.TokenUsage{PromptTokens: 8, CompletionTokens: 4, TotalTokens: 12}, nil
}

func TestLLMCodeReviewerValidatesChangedPaths(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.LLM.Provider = "openai-responses"
		cfg.LLM.Model = "default"
		cfg.LLM.Scenes = map[string]config.LLMEndpointConfig{config.LLMSceneCodeReviewer: {Model: "reviewer"}}
	}))
	caller := &reviewerCaller{result: Result{Summary: "one issue", Findings: []Finding{{Severity: "high", Path: "main.go", Line: 10, Title: "nil dereference", Detail: "guard the pointer before use"}}}}
	ctx := llm.WithRuntime(context.Background(), llm.NewRuntime(caller, nil))
	result, usage, err := NewLLMCodeReviewer(config.LLMSceneCodeReviewer).Review(ctx, &types.Task{Goal: "fix code"}, ChangeSet{Paths: []string{"main.go"}, Diff: "+value := ptr.Name"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Findings) != 1 || usage.TotalTokens != 12 || caller.config.Scene != config.LLMSceneCodeReviewer || !strings.Contains(caller.prompt, "main.go") {
		t.Fatalf("result=%+v usage=%+v config=%+v", result, usage, caller.config)
	}
	caller.result.Findings[0].Path = "unchanged.go"
	if _, _, err := NewLLMCodeReviewer(config.LLMSceneCodeReviewer).Review(ctx, &types.Task{Goal: "fix code"}, ChangeSet{Paths: []string{"main.go"}, Diff: "+change"}); err == nil {
		t.Fatal("expected unchanged finding path to be rejected")
	}
}

func TestParseStatusPathsAndChangeTrigger(t *testing.T) {
	status := " M main.go\x00?? new.go\x00R  renamed.go\x00old.go\x00"
	paths := parseStatusPaths(status)
	want := []string{"main.go", "new.go", "old.go", "renamed.go"}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Fatalf("paths=%v want=%v", paths, want)
	}
	if !isUntrackedStatus(status, "new.go") {
		t.Fatal("new.go should be detected as untracked")
	}
	if !TaskMayHaveCodeChanges(&types.Task{Trace: []types.StepTrace{{Action: "apply_patch"}}}) {
		t.Fatal("successful apply_patch should trigger code review")
	}
	if TaskMayHaveCodeChanges(&types.Task{Trace: []types.StepTrace{{Action: "write_file", Error: "denied"}}}) {
		t.Fatal("failed write_file should not trigger code review")
	}
}

func TestChangeInputFiltersFilesAndRedactsSecrets(t *testing.T) {
	if reviewableUntracked(".env") || reviewableUntracked("private.key") || reviewableUntracked("screen.png") {
		t.Fatal("sensitive or binary untracked files must not be uploaded for review")
	}
	if !reviewableUntracked("internal/server.go") || !reviewableUntracked("Dockerfile") {
		t.Fatal("code files should be reviewable")
	}
	input := "+api_key = sk-abcdefghijklmnopqrstuvwxyz\n+OPENAI_API_KEY=another-secret-value\n+Authorization: Bearer abc.def.secret\n-----BEGIN PRIVATE KEY-----\nsecret\n-----END PRIVATE KEY-----"
	redacted := redactSecrets(input)
	if strings.Contains(redacted, "abcdefghijklmnopqrstuvwxyz") || strings.Contains(redacted, "another-secret-value") || strings.Contains(redacted, "abc.def.secret") || strings.Contains(redacted, "\nsecret\n") {
		t.Fatalf("secret was not redacted: %s", redacted)
	}
	truncatedKey := redactSecrets("-----BEGIN PRIVATE KEY-----\nsecret without an end marker")
	if strings.Contains(truncatedKey, "secret without") {
		t.Fatalf("truncated private key was not redacted: %s", truncatedKey)
	}
}
