package multiagent

import (
	"context"
	"strings"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/plancritic"
	"github.com/wuxujun/ai-agent/internal/promptmanager"
	"github.com/wuxujun/ai-agent/internal/types"
)

func TestResolveAgentPromptFetchesLangfuseWithInlineFallback(t *testing.T) {
	var gotName, gotFallback string
	fetch := func(_ context.Context, name, fallback string) string {
		gotName, gotFallback = name, fallback
		return "prompt from Langfuse"
	}

	agentCfg := AgentConfig{PromptName: "teams/software/planner", SystemPrompt: "inline fallback"}
	got := resolveAgentPromptWithFetcher(context.Background(), agentCfg, "default-name", "code fallback", fetch)
	if got != "prompt from Langfuse" {
		t.Fatalf("prompt = %q", got)
	}
	if gotName != "teams/software/planner" || gotFallback != "inline fallback" {
		t.Fatalf("fetch arguments = (%q, %q)", gotName, gotFallback)
	}
}

func TestResolveAgentPromptPassesConfiguredSelector(t *testing.T) {
	var gotSelector promptmanager.Selector
	fetch := func(_ context.Context, _ string, selector promptmanager.Selector, _ string) string {
		gotSelector = selector
		return "candidate"
	}
	agentCfg := AgentConfig{PromptName: "teams/software/critic", PromptVersion: 17}
	got := resolveAgentPromptWithSelectorFetcher(context.Background(), agentCfg, "default", "fallback", fetch)
	if got != "candidate" || gotSelector.Version != 17 || gotSelector.Label != "" {
		t.Fatalf("prompt=%q selector=%+v", got, gotSelector)
	}

	agentCfg.PromptVersion = 0
	agentCfg.PromptLabel = "staging"
	_ = resolveAgentPromptWithSelectorFetcher(context.Background(), agentCfg, "default", "fallback", fetch)
	if gotSelector.Label != "staging" || gotSelector.Version != 0 {
		t.Fatalf("selector=%+v", gotSelector)
	}
}

func TestWithCriticPromptSelectorRejectsAmbiguousSelector(t *testing.T) {
	if _, err := WithCriticPromptSelector(context.Background(), promptmanager.Selector{Label: "latest", Version: 2}); err == nil {
		t.Fatal("expected ambiguous selector rejection")
	}
	ctx, err := WithCriticPromptSelector(context.Background(), promptmanager.Selector{Version: 2})
	if err != nil {
		t.Fatal(err)
	}
	override, ok := ctx.Value(criticPromptSelectorContextKey{}).(criticPromptSelectorOverride)
	if !ok || override.Selector.Version != 2 {
		t.Fatalf("override=%+v ok=%t", override, ok)
	}
}

func TestDraftAgentConfigCopiesPromptSelector(t *testing.T) {
	draft := draftAgentConfig(AgentConfig{DraftPromptName: "draft", DraftPromptLabel: "latest", DraftPromptVersion: 0})
	if draft.PromptName != "draft" || draft.PromptLabel != "latest" || draft.PromptVersion != 0 {
		t.Fatalf("draft=%+v", draft)
	}
}

func TestCriticPromptSelectorUsesStrictLangfuseResolution(t *testing.T) {
	t.Cleanup(func() { config.Reload() })
	t.Setenv("LANGFUSE_ENABLED", "false")
	config.Reload()
	ctx := withTeamConfigSnapshot(context.Background(), newTeamConfigSnapshot("eval", TeamConfig{Critic: AgentConfig{PromptName: "teams/eval/critic"}}))
	ctx, err := WithCriticPromptSelector(ctx, promptmanager.Selector{Label: "latest"})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = (&CriticAgent{}).Critique(ctx, &types.Task{Goal: "review"}, plancritic.Plan{Steps: []plancritic.Step{{Action: "read_file"}}})
	if err == nil || !strings.Contains(err.Error(), "Langfuse prompt management is disabled") {
		t.Fatalf("error=%v", err)
	}
}

func TestTaskPromptResolutionFailsWhenPinnedVersionIsUnavailable(t *testing.T) {
	t.Cleanup(func() { config.Reload() })
	t.Setenv("LANGFUSE_ENABLED", "false")
	config.Reload()
	registry, err := promptmanager.NewVersionPinRegistry([]promptmanager.VersionPin{{
		Name: "teams/test/planner", Version: 4, Selector: promptmanager.Selector{Label: "production"},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := promptmanager.WithVersionPinRegistry(context.Background(), registry)
	_, err = resolveAgentPromptForTask(ctx, AgentConfig{PromptName: "teams/test/planner"}, "default", "fallback must not run")
	if err == nil || !strings.Contains(err.Error(), "Langfuse prompt management is disabled") {
		t.Fatalf("error=%v", err)
	}
}

func TestResolveAgentPromptFallsBackWhenLangfuseDisabled(t *testing.T) {
	fetch := func(_ context.Context, _ string, fallback string) string { return fallback }

	agentCfg := AgentConfig{PromptName: "teams/software/writer", SystemPrompt: "inline fallback"}
	if got := resolveAgentPromptWithFetcher(context.Background(), agentCfg, "default-name", "code fallback", fetch); got != "inline fallback" {
		t.Fatalf("prompt = %q, want inline fallback", got)
	}

	agentCfg.SystemPrompt = ""
	if got := resolveAgentPromptWithFetcher(context.Background(), agentCfg, "default-name", "code fallback", fetch); got != "code fallback" {
		t.Fatalf("prompt = %q, want code fallback", got)
	}
}
