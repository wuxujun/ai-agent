package multiagent

import (
	"strings"
	"testing"
)

func TestTeamPromptSeedsIncludesDraftAndIsDeterministic(t *testing.T) {
	cfg := &TeamsConfig{Teams: map[string]TeamConfig{
		"zeta": {
			Writer: AgentConfig{
				PromptName: "teams/zeta/writer", PromptLabel: "production", SystemPrompt: "write",
			},
		},
		"alpha": {
			Planner: AgentConfig{
				PromptName: "teams/alpha/planner", PromptLabel: "production", SystemPrompt: "plan",
			},
			Verifier: AgentConfig{
				DraftPromptName: "teams/alpha/verifier-draft", DraftPromptLabel: "latest", DraftSystemPrompt: "draft",
			},
		},
	}}
	seeds, err := TeamPromptSeeds(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(seeds) != 3 {
		t.Fatalf("seeds=%+v", seeds)
	}
	got := []string{seeds[0].Name, seeds[1].Name, seeds[2].Name}
	want := []string{"teams/alpha/planner", "teams/alpha/verifier-draft", "teams/zeta/writer"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("names=%v want=%v", got, want)
		}
	}
	if seeds[1].Content != "draft" || seeds[1].Selector.Label != "latest" {
		t.Fatalf("draft seed=%+v", seeds[1])
	}
}

func TestTeamPromptSeedsRejectsConflicts(t *testing.T) {
	cfg := &TeamsConfig{Teams: map[string]TeamConfig{
		"one": {Planner: AgentConfig{PromptName: "shared", PromptLabel: "production", SystemPrompt: "one"}},
		"two": {Writer: AgentConfig{PromptName: "shared", PromptLabel: "production", SystemPrompt: "two"}},
	}}
	if _, err := TeamPromptSeeds(cfg); err == nil || !strings.Contains(err.Error(), "conflicting local content") {
		t.Fatalf("expected content conflict, got %v", err)
	}
	cfg.Teams["two"] = TeamConfig{Writer: AgentConfig{PromptName: "shared", PromptLabel: "latest", SystemPrompt: "one"}}
	if _, err := TeamPromptSeeds(cfg); err == nil || !strings.Contains(err.Error(), "conflicting selectors") {
		t.Fatalf("expected selector conflict, got %v", err)
	}
}

func TestRepositoryTeamsDeclareLangfusePromptSeeds(t *testing.T) {
	cfg, err := LoadTeamsConfigStrict()
	if err != nil {
		t.Fatal(err)
	}
	seeds, err := TeamPromptSeeds(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(seeds) != 10 {
		t.Fatalf("repository teams define %d prompt seeds, want 10", len(seeds))
	}
}
