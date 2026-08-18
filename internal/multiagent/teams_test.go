package multiagent_test

import (
	"os"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/multiagent"
	"github.com/wuxujun/ai-agent/internal/planner"
)

func TestTeamsConfig_DefaultFallback(t *testing.T) {
	t.Setenv("AI_AGENT_MULTIAGENT_WORKFLOW", "")
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.MultiAgent.Team = ""
		cfg.MultiAgent.Runtime = "legacy"
		cfg.MultiAgent.DAGCanaryPercent = 0
	}))

	// Ensure no local teams.yaml is read by moving away or testing in clean env.
	// We'll rename local teams.yaml if it exists, but since we are running tests,
	// we can check what GetTeamsConfig returns when we force a non-existent path
	// or clean env.
	cfg := multiagent.GetTeamsConfig()
	if cfg == nil {
		t.Fatal("expected non-nil TeamsConfig")
	}
	if cfg.ActiveWorkflow() != multiagent.WorkflowResearch {
		t.Fatalf("default workflow = %q, want %q", cfg.ActiveWorkflow(), multiagent.WorkflowResearch)
	}
}

func TestTeamsConfig_ConfigOverride(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) { cfg.MultiAgent.Team = "test-team-config" }))

	cfg := multiagent.GetTeamsConfig()
	if cfg.ActiveTeam != "test-team-config" {
		t.Errorf("expected active team to be 'test-team-config', got %q", cfg.ActiveTeam)
	}
}

func TestTeamsConfig_WikiGraphToolBoundary(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) { cfg.MultiAgent.Team = "wiki_graph" }))

	cfg := multiagent.GetTeamsConfig()
	team := cfg.GetActiveTeam()
	if cfg.ActiveTeam != "wiki_graph" {
		t.Fatalf("active team = %q", cfg.ActiveTeam)
	}
	if len(team.Planner.Tools) != 2 || team.Planner.Tools[0] != "wiki_search" || team.Planner.Tools[1] != "wiki_graph" {
		t.Fatalf("wiki_graph planner tools = %v", team.Planner.Tools)
	}
	for _, tool := range team.Planner.Tools {
		if tool == "wiki_fetch" || tool == "wiki_graph_fetch" {
			t.Fatalf("internal fetch tool exposed to planner: %v", team.Planner.Tools)
		}
	}
}

func TestTeamsConfig_WikiSuggestToolBoundary(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) { cfg.MultiAgent.Team = "wiki_suggest" }))

	cfg := multiagent.GetTeamsConfig()
	team := cfg.GetActiveTeam()
	if cfg.ActiveTeam != "wiki_suggest" || len(team.Planner.Tools) != 2 || team.Planner.Tools[0] != "wiki_search" || team.Planner.Tools[1] != "wiki_suggest" {
		t.Fatalf("wiki_suggest team=%q tools=%v", cfg.ActiveTeam, team.Planner.Tools)
	}
}

func TestListTeamSummariesIsSortedAndRedacted(t *testing.T) {
	summaries := multiagent.ListTeamSummaries()
	if len(summaries) == 0 {
		t.Fatal("expected configured team summaries")
	}
	defaultCount := 0
	for i, summary := range summaries {
		if i > 0 && summaries[i-1].Name >= summary.Name {
			t.Fatalf("team summaries are not strictly sorted: %q then %q", summaries[i-1].Name, summary.Name)
		}
		if summary.Name == "" || summary.ConfigDigest == "" || summary.Workflow == "" || summary.Runtime == "" || summary.Lifecycle != multiagent.TeamLifecycleActive || !summary.Selectable {
			t.Fatalf("incomplete team summary: %+v", summary)
		}
		if summary.Default {
			defaultCount++
		}
	}
	if defaultCount != 1 {
		t.Fatalf("default team count = %d, want 1", defaultCount)
	}
}

func TestTeamsConfig_YAMLParse(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.MultiAgent.Team = ""
		cfg.MultiAgent.Runtime = ""
		cfg.MultiAgent.DAGCanaryPercent = 0
	}))
	yamlContent := `
active_team: "data"
resume_config_policy: "require_match"
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
    runtime: "dag"
    workflow: "adaptive"
    routing:
      reviewed_intents: ["coding", "automation"]
      reviewed_complexities: ["medium", "high"]
      reviewed_min_plan_steps: 4
      reviewed_min_remaining_llm_calls: 3
      reviewed_min_remaining_tokens: 1200
      allow_research_high_risk_tools: false
    critic_policy:
      max_replans: 2
    planner:
      name: "Data Planner"
      system_prompt: "Analyze CSVs"
      prompt_name: "teams/data/planner"
      prompt_label: "staging"
      provider: "gemini"
      model: "gemini-2.5"
    critic:
      name: "Plan Reviewer"
      langfuse_prompt: "teams/data/critic"
      prompt_version: 9
    executor:
      name: "Data Executor"
    verifier:
      name: "Result Verifier"
      draft_prompt_name: "teams/data/verifier-draft"
      draft_prompt_label: "latest"
      draft_system_prompt: "Draft from evidence"
      draft_provider: "openai-responses"
      draft_model: "gpt-draft"
      draft_llm_scene: "multiagent_writer"
      prompt_name: "teams/data/verifier-check"
      prompt_version: 5
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
	if cfg.ResumeConfigPolicy != multiagent.ResumeConfigRequireMatch {
		t.Fatalf("resume config policy = %q", cfg.ResumeConfigPolicy)
	}

	activeTeam := cfg.GetActiveTeam()
	if activeTeam.Planner.Name != "Data Planner" {
		t.Errorf("expected planner name 'Data Planner', got %q", activeTeam.Planner.Name)
	}
	if activeTeam.Planner.SystemPrompt != "Analyze CSVs" {
		t.Errorf("expected planner prompt 'Analyze CSVs', got %q", activeTeam.Planner.SystemPrompt)
	}
	if activeTeam.Planner.PromptName != "teams/data/planner" || activeTeam.Critic.LangfusePrompt != "teams/data/critic" {
		t.Fatalf("Langfuse prompt references were not parsed: %+v", activeTeam)
	}
	if activeTeam.Planner.PromptLabel != "staging" || activeTeam.Critic.PromptVersion != 9 {
		t.Fatalf("Langfuse prompt selectors were not parsed: %+v", activeTeam)
	}
	if activeTeam.Planner.Model != "gemini-2.5" {
		t.Errorf("expected planner model 'gemini-2.5', got %q", activeTeam.Planner.Model)
	}
	if cfg.ActiveWorkflow() != multiagent.WorkflowAdaptive {
		t.Fatalf("expected adaptive workflow, got %q", cfg.ActiveWorkflow())
	}
	if cfg.ActiveRuntime() != multiagent.RuntimeDAG {
		t.Fatalf("expected DAG runtime, got %q", cfg.ActiveRuntime())
	}
	if len(activeTeam.Routing.ReviewedIntents) != 2 || len(activeTeam.Routing.ReviewedComplexities) != 2 || activeTeam.Routing.ReviewedMinPlanSteps != 4 || activeTeam.Routing.ReviewedMinRemainingLLMCalls != 3 || activeTeam.Routing.ReviewedMinRemainingTokens != 1200 || activeTeam.Routing.AllowResearchHighRiskTools {
		t.Fatalf("adaptive routing configuration was not parsed: %+v", activeTeam.Routing)
	}
	if activeTeam.CriticPolicy.MaxReplans == nil || *activeTeam.CriticPolicy.MaxReplans != 2 {
		t.Fatalf("critic policy was not parsed: %+v", activeTeam.CriticPolicy)
	}
	if activeTeam.Critic.Name != "Plan Reviewer" || activeTeam.Executor.Name != "Data Executor" || activeTeam.Verifier.Name != "Result Verifier" {
		t.Fatalf("four-role configuration was not parsed: %+v", activeTeam)
	}
	if activeTeam.Verifier.DraftPromptName != "teams/data/verifier-draft" || activeTeam.Verifier.DraftSystemPrompt != "Draft from evidence" || activeTeam.Verifier.PromptName != "teams/data/verifier-check" {
		t.Fatalf("split verifier prompts were not parsed: %+v", activeTeam.Verifier)
	}
	if activeTeam.Verifier.DraftPromptLabel != "latest" || activeTeam.Verifier.PromptVersion != 5 {
		t.Fatalf("split verifier selectors were not parsed: %+v", activeTeam.Verifier)
	}
	if activeTeam.Verifier.DraftProvider != "openai-responses" || activeTeam.Verifier.DraftModel != "gpt-draft" || activeTeam.Verifier.DraftLLMScene != "multiagent_writer" {
		t.Fatalf("split verifier LLM configuration was not parsed: %+v", activeTeam.Verifier)
	}

	llmCfg := multiagent.GetLLMConfig(activeTeam.Planner)
	if llmCfg.Model != "gemini-2.5" {
		t.Errorf("expected LLM config model 'gemini-2.5', got %q", llmCfg.Model)
	}
	if llmCfg.Provider != planner.ProviderGemini {
		t.Errorf("expected LLM config provider 'gemini', got %q", llmCfg.Provider)
	}
}

func TestTeamsConfig_WorkflowEnvironmentOverride(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) { cfg.MultiAgent.Team = "software" }))
	t.Setenv("AI_AGENT_MULTIAGENT_WORKFLOW", "planner-critic-executor-verifier")

	cfg := multiagent.GetTeamsConfig()
	if cfg.ActiveWorkflow() != multiagent.WorkflowReviewed {
		t.Fatalf("expected workflow override, got %q", cfg.ActiveWorkflow())
	}
}

func TestTeamsConfig_AdaptiveWorkflowEnvironmentOverride(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) { cfg.MultiAgent.Team = "software" }))
	t.Setenv("AI_AGENT_MULTIAGENT_WORKFLOW", "auto")

	cfg := multiagent.GetTeamsConfig()
	if cfg.ActiveWorkflow() != multiagent.WorkflowAdaptive {
		t.Fatalf("expected adaptive workflow override, got %q", cfg.ActiveWorkflow())
	}
}

func TestTeamsConfig_RuntimeConfigOverride(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.MultiAgent.Team = "software"
		cfg.MultiAgent.Runtime = "dag"
	}))

	cfg := multiagent.GetTeamsConfig()
	if cfg.ActiveRuntime() != multiagent.RuntimeDAG {
		t.Fatalf("expected DAG runtime override, got %q", cfg.ActiveRuntime())
	}
}

func TestTeamsConfig_DAGCanaryPercentConfigOverride(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.MultiAgent.Team = "software"
		cfg.MultiAgent.DAGCanaryPercent = 5
	}))

	cfg := multiagent.GetTeamsConfig()
	if got := cfg.GetActiveTeam().DAGCanaryPercent; got != 5 {
		t.Fatalf("DAG canary percent = %d, want 5", got)
	}

}

func TestTeamsConfig_ResumePolicyEnvironmentOverride(t *testing.T) {
	t.Setenv("AI_AGENT_MULTIAGENT_RESUME_CONFIG_POLICY", "latest")

	cfg := multiagent.GetTeamsConfig()
	if cfg.ResumeConfigPolicy != multiagent.ResumeConfigUseLatest {
		t.Fatalf("expected use_latest resume policy, got %q", cfg.ResumeConfigPolicy)
	}
}
