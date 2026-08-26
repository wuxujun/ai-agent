package orchestrator

import (
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/types"
)

func TestMultiAgentTeamNamePrefersTaskTeam(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.MultiAgent.Team = "wiki_suggest"
	}))

	if got := multiAgentTeamName(&types.Task{Team: " kb_qa "}); got != "kb_qa" {
		t.Fatalf("multi-agent log team = %q, want task Team kb_qa", got)
	}
}

func TestMultiAgentTeamNameFallsBackToProcessDefault(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.MultiAgent.Team = "wiki_suggest"
	}))

	if got := multiAgentTeamName(&types.Task{}); got != "wiki_suggest" {
		t.Fatalf("multi-agent log team = %q, want process default wiki_suggest", got)
	}
}
