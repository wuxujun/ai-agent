package multiagent

import (
	"context"
	"encoding/json"
	"os"
	"strings"

	"github.com/wuxujun/ai-agent/internal/config"
	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/promptmanager"
	"github.com/wuxujun/ai-agent/internal/tools"
	"github.com/wuxujun/ai-agent/internal/types"
	"gopkg.in/yaml.v3"
)

type Workflow string

const (
	WorkflowResearch Workflow = "planner_researcher_writer"
	WorkflowReviewed Workflow = "planner_critic_executor_verifier"
	WorkflowAdaptive Workflow = "adaptive"
)

const WorkflowRouteTraceAction = "multiagent_workflow_route"

type AgentConfig struct {
	Name              string `yaml:"name" json:"name"`
	SystemPrompt      string `yaml:"system_prompt" json:"system_prompt"`
	PromptName        string `yaml:"prompt_name" json:"prompt_name"`
	LangfusePrompt    string `yaml:"langfuse_prompt" json:"langfuse_prompt"`
	DraftPromptName   string `yaml:"draft_prompt_name" json:"draft_prompt_name"`
	DraftSystemPrompt string `yaml:"draft_system_prompt" json:"draft_system_prompt"`
	Provider          string `yaml:"provider" json:"provider"`
	Model             string `yaml:"model" json:"model"`
	LLMScene          string `yaml:"llm_scene" json:"llm_scene"`
}

func draftPromptConfig(agentCfg AgentConfig) AgentConfig {
	return AgentConfig{
		SystemPrompt: agentCfg.DraftSystemPrompt,
		PromptName:   agentCfg.DraftPromptName,
	}
}

// resolveAgentPrompt preserves inline prompt compatibility while allowing a
// team role to name a Langfuse-managed prompt. prompt_name is the preferred
// field; langfuse_prompt remains an explicit alias.
func resolveAgentPrompt(ctx context.Context, agentCfg AgentConfig, defaultName, defaultPrompt string) string {
	return resolveAgentPromptWithFetcher(ctx, agentCfg, defaultName, defaultPrompt, promptmanager.GetManager().Get)
}

func resolveAgentPromptWithFetcher(ctx context.Context, agentCfg AgentConfig, defaultName, defaultPrompt string, fetch func(context.Context, string, string) string) string {
	fallback := defaultPrompt
	if strings.TrimSpace(agentCfg.SystemPrompt) != "" {
		fallback = agentCfg.SystemPrompt
	}
	promptName := strings.TrimSpace(agentCfg.PromptName)
	if promptName == "" {
		promptName = strings.TrimSpace(agentCfg.LangfusePrompt)
	}
	if promptName != "" {
		return fetch(ctx, promptName, fallback)
	}
	if strings.TrimSpace(agentCfg.SystemPrompt) != "" {
		return agentCfg.SystemPrompt
	}
	return fetch(ctx, defaultName, defaultPrompt)
}

func hasConfiguredPrompt(agentCfg AgentConfig) bool {
	return strings.TrimSpace(agentCfg.SystemPrompt) != "" || strings.TrimSpace(agentCfg.PromptName) != "" || strings.TrimSpace(agentCfg.LangfusePrompt) != ""
}

type TeamConfig struct {
	Workflow   Workflow              `yaml:"workflow" json:"workflow"`
	Routing    WorkflowRoutingConfig `yaml:"routing" json:"routing"`
	Planner    AgentConfig           `yaml:"planner" json:"planner"`
	Critic     AgentConfig           `yaml:"critic" json:"critic"`
	Executor   AgentConfig           `yaml:"executor" json:"executor"`
	Verifier   AgentConfig           `yaml:"verifier" json:"verifier"`
	Researcher AgentConfig           `yaml:"researcher" json:"researcher"`
	Writer     AgentConfig           `yaml:"writer" json:"writer"`
}

type WorkflowRoutingConfig struct {
	ReviewedIntents            []string `yaml:"reviewed_intents" json:"reviewed_intents"`
	ReviewedComplexities       []string `yaml:"reviewed_complexities" json:"reviewed_complexities"`
	ReviewedMinPlanSteps       int      `yaml:"reviewed_min_plan_steps" json:"reviewed_min_plan_steps"`
	AllowResearchHighRiskTools bool     `yaml:"allow_research_high_risk_tools" json:"allow_research_high_risk_tools"`
}

type TeamsConfig struct {
	ActiveTeam string                `yaml:"active_team" json:"active_team"`
	Teams      map[string]TeamConfig `yaml:"teams" json:"teams"`
}

// GetTeamsConfig loads and parses teams.yaml if it exists.
// Hot-reloads on demand by not caching the result forever.
func GetTeamsConfig() *TeamsConfig {
	cfg := &TeamsConfig{
		ActiveTeam: "default",
		Teams:      make(map[string]TeamConfig),
	}

	// Try loading from teams.yaml in the current directory and parent directories
	paths := []string{"teams.yaml", "../teams.yaml", "../../teams.yaml"}
	var data []byte
	var err error
	for _, p := range paths {
		data, err = os.ReadFile(p)
		if err == nil {
			break
		}
	}

	if err == nil {
		if parseErr := yaml.Unmarshal(data, cfg); parseErr != nil {
			log.Error("Failed to parse teams.yaml", "error", parseErr)
		}
	}

	// If AI_AGENT_MULTIAGENT_TEAM environment variable is set, override the active team
	if envTeam := os.Getenv("AI_AGENT_MULTIAGENT_TEAM"); envTeam != "" {
		cfg.ActiveTeam = envTeam
	}
	if envWorkflow := os.Getenv("AI_AGENT_MULTIAGENT_WORKFLOW"); envWorkflow != "" {
		team := cfg.Teams[cfg.ActiveTeam]
		team.Workflow = parseWorkflow(envWorkflow)
		cfg.Teams[cfg.ActiveTeam] = team
	}

	return cfg
}

// ActiveWorkflow returns the selected orchestration mode. Empty and
// unrecognised values deliberately preserve the original three-role workflow.
func (c *TeamsConfig) ActiveWorkflow() Workflow {
	return parseWorkflow(string(c.GetActiveTeam().Workflow))
}

func parseWorkflow(value string) Workflow {
	normalized := strings.NewReplacer("-", "_", " ", "_").Replace(strings.ToLower(strings.TrimSpace(value)))
	switch normalized {
	case "planner_critic_executor_verifier", "review", "reviewed", "execution":
		return WorkflowReviewed
	case "adaptive", "auto":
		return WorkflowAdaptive
	default:
		return WorkflowResearch
	}
}

type workflowRouteDecision struct {
	Configured Workflow `json:"configured"`
	Effective  Workflow `json:"effective"`
	Reason     string   `json:"reason"`
}

func resolveWorkflow(configured Workflow, routing WorkflowRoutingConfig, task *types.Task, plan *ResearchPlan) workflowRouteDecision {
	if configured != WorkflowAdaptive {
		return workflowRouteDecision{Configured: configured, Effective: configured, Reason: "configured"}
	}
	if persisted, ok := persistedWorkflowRoute(task); ok {
		return persisted
	}
	if !routing.AllowResearchHighRiskTools && plan != nil {
		for _, step := range plan.Steps {
			if tool, ok := tools.Get(step.Action); ok && tool.RiskLevel() == types.RiskLevelHigh {
				return workflowRouteDecision{Configured: configured, Effective: WorkflowReviewed, Reason: "high_risk_action:" + step.Action}
			}
		}
	}
	intent, complexity := taskRoutingClassification(task)
	complexities := routing.ReviewedComplexities
	if len(complexities) == 0 {
		complexities = []string{"high"}
	}
	if containsRoutingValue(complexities, complexity) {
		return workflowRouteDecision{Configured: configured, Effective: WorkflowReviewed, Reason: "complexity:" + complexity}
	}
	if containsRoutingValue(routing.ReviewedIntents, intent) {
		return workflowRouteDecision{Configured: configured, Effective: WorkflowReviewed, Reason: "intent:" + intent}
	}
	if routing.ReviewedMinPlanSteps > 0 && plan != nil && len(plan.Steps) >= routing.ReviewedMinPlanSteps {
		return workflowRouteDecision{Configured: configured, Effective: WorkflowReviewed, Reason: "plan_steps"}
	}
	return workflowRouteDecision{Configured: configured, Effective: WorkflowResearch, Reason: "default_research"}
}

func persistedWorkflowRoute(task *types.Task) (workflowRouteDecision, bool) {
	if task == nil {
		return workflowRouteDecision{}, false
	}
	for i := len(task.Trace) - 1; i >= 0; i-- {
		trace := task.Trace[i]
		if trace.Action != WorkflowRouteTraceAction {
			continue
		}
		var decision workflowRouteDecision
		if json.Unmarshal([]byte(trace.Observation), &decision) != nil {
			return workflowRouteDecision{}, false
		}
		if decision.Effective != WorkflowResearch && decision.Effective != WorkflowReviewed {
			return workflowRouteDecision{}, false
		}
		decision.Configured = WorkflowAdaptive
		decision.Reason = "persisted:" + decision.Reason
		return decision, true
	}
	return workflowRouteDecision{}, false
}

func taskRoutingClassification(task *types.Task) (string, string) {
	if task == nil {
		return "", ""
	}
	for i := len(task.Trace) - 1; i >= 0; i-- {
		trace := task.Trace[i]
		if trace.Action != llmcore.IntentRouteTraceAction {
			continue
		}
		var details struct {
			Complexity string `json:"complexity"`
		}
		if json.Unmarshal([]byte(trace.Observation), &details) == nil {
			return strings.ToLower(strings.TrimSpace(trace.Query)), strings.ToLower(strings.TrimSpace(details.Complexity))
		}
		break
	}
	return "", ""
}

func containsRoutingValue(values []string, target string) bool {
	target = strings.ToLower(strings.TrimSpace(target))
	if target == "" {
		return false
	}
	for _, value := range values {
		if strings.ToLower(strings.TrimSpace(value)) == target {
			return true
		}
	}
	return false
}

// GetActiveTeam returns the active TeamConfig. If the active team is not found,
// it returns an empty TeamConfig (which falls back to original hardcoded values).
func (c *TeamsConfig) GetActiveTeam() TeamConfig {
	if c.Teams == nil {
		return TeamConfig{}
	}
	if team, ok := c.Teams[c.ActiveTeam]; ok {
		return team
	}
	return TeamConfig{}
}

// GetLLMConfig returns an LLMConfig derived from AgentConfig, falling back
// to the default LLMConfig if fields are omitted.
func GetLLMConfig(agentCfg AgentConfig, defaultScene ...string) LLMConfig {
	scene := ""
	if len(defaultScene) > 0 {
		scene = defaultScene[0]
	}
	if agentCfg.LLMScene != "" {
		scene = agentCfg.LLMScene
	}
	cfg := LLMConfigForScene(scene)
	if agentCfg.Provider != "" {
		globalCfg := config.Get()
		resolved := globalCfg.ResolveLLMProviderConfig(agentCfg.Provider)
		cfg.Provider = planner.ProviderType(agentCfg.Provider)
		cfg.APIKey = resolved.APIKey
		cfg.BaseURL = resolved.BaseURL
		if agentCfg.Model == "" {
			cfg.Model = resolved.Model
		}
	}
	if agentCfg.Model != "" {
		cfg.Model = agentCfg.Model
	}
	return cfg
}
