package multiagent

import (
	"context"
	"testing"
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
