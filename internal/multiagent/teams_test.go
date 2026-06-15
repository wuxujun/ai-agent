package multiagent_test

import (
	"os"
	"testing"

	"github.com/wuxujun/ai-agent/internal/multiagent"
	"github.com/wuxujun/ai-agent/internal/planner"
)

func TestTeamsConfig_DefaultFallback(t *testing.T) {
	// Clean up any environment override
	os.Unsetenv("AI_AGENT_MULTIAGENT_TEAM")

	// Ensure no local teams.yaml is read by moving away or testing in clean env.
	// We'll rename local teams.yaml if it exists, but since we are running tests,
	// we can check what GetTeamsConfig returns when we force a non-existent path
	// or clean env.
	cfg := multiagent.GetTeamsConfig()
	if cfg == nil {
		t.Fatal("expected non-nil TeamsConfig")
	}
}

func TestTeamsConfig_EnvironmentOverride(t *testing.T) {
	os.Setenv("AI_AGENT_MULTIAGENT_TEAM", "test-team-env")
	defer os.Unsetenv("AI_AGENT_MULTIAGENT_TEAM")

	cfg := multiagent.GetTeamsConfig()
	if cfg.ActiveTeam != "test-team-env" {
		t.Errorf("expected active team to be 'test-team-env', got %q", cfg.ActiveTeam)
	}
}

func TestTeamsConfig_YAMLParse(t *testing.T) {
	yamlContent := `
active_team: "data"
teams:
  software:
    planner:
      name: "Dev Planner"
      system_prompt: "Write software"
      model: "gpt-4"
    writer:
      name: "Doc Writer"
      system_prompt: "Document code"
  data:
    planner:
      name: "Data Planner"
      system_prompt: "Analyze CSVs"
      provider: "gemini"
      model: "gemini-2.5"
    writer:
      name: "Data Writer"
      system_prompt: "Summarize findings"
`
	err := os.WriteFile("teams.yaml", []byte(yamlContent), 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove("teams.yaml")

	cfg := multiagent.GetTeamsConfig()
	if cfg.ActiveTeam != "data" {
		t.Errorf("expected active team to be 'data', got %q", cfg.ActiveTeam)
	}

	activeTeam := cfg.GetActiveTeam()
	if activeTeam.Planner.Name != "Data Planner" {
		t.Errorf("expected planner name 'Data Planner', got %q", activeTeam.Planner.Name)
	}
	if activeTeam.Planner.SystemPrompt != "Analyze CSVs" {
		t.Errorf("expected planner prompt 'Analyze CSVs', got %q", activeTeam.Planner.SystemPrompt)
	}
	if activeTeam.Planner.Model != "gemini-2.5" {
		t.Errorf("expected planner model 'gemini-2.5', got %q", activeTeam.Planner.Model)
	}

	llmCfg := multiagent.GetLLMConfig(activeTeam.Planner)
	if llmCfg.Model != "gemini-2.5" {
		t.Errorf("expected LLM config model 'gemini-2.5', got %q", llmCfg.Model)
	}
	if llmCfg.Provider != planner.ProviderGemini {
		t.Errorf("expected LLM config provider 'gemini', got %q", llmCfg.Provider)
	}
}
