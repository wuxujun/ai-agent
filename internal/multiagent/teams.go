package multiagent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
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

type OrchestrationRuntime string

const (
	WorkflowResearch Workflow = "planner_researcher_writer"
	WorkflowReviewed Workflow = "planner_critic_executor_verifier"
	WorkflowAdaptive Workflow = "adaptive"
)

const (
	RuntimeLegacy OrchestrationRuntime = "legacy"
	RuntimeDAG    OrchestrationRuntime = "dag"
)

const (
	WorkflowRouteTraceAction    = "multiagent_workflow_route"
	TeamConfigChangeTraceAction = "multiagent_team_config_change"
)

type ResumeConfigPolicy string

const (
	ResumeConfigUseLatest    ResumeConfigPolicy = "use_latest"
	ResumeConfigRequireMatch ResumeConfigPolicy = "require_match"
)

type AgentConfig struct {
	Name               string `yaml:"name" json:"name"`
	SystemPrompt       string `yaml:"system_prompt" json:"system_prompt"`
	PromptName         string `yaml:"prompt_name" json:"prompt_name"`
	LangfusePrompt     string `yaml:"langfuse_prompt" json:"langfuse_prompt"`
	PromptLabel        string `yaml:"prompt_label" json:"prompt_label"`
	PromptVersion      int    `yaml:"prompt_version" json:"prompt_version"`
	DraftPromptName    string `yaml:"draft_prompt_name" json:"draft_prompt_name"`
	DraftPromptLabel   string `yaml:"draft_prompt_label" json:"draft_prompt_label"`
	DraftPromptVersion int    `yaml:"draft_prompt_version" json:"draft_prompt_version"`
	DraftSystemPrompt  string `yaml:"draft_system_prompt" json:"draft_system_prompt"`
	DraftProvider      string `yaml:"draft_provider" json:"draft_provider"`
	DraftModel         string `yaml:"draft_model" json:"draft_model"`
	DraftLLMScene      string `yaml:"draft_llm_scene" json:"draft_llm_scene"`
	Provider           string `yaml:"provider" json:"provider"`
	Model              string `yaml:"model" json:"model"`
	LLMScene           string `yaml:"llm_scene" json:"llm_scene"`
}

func draftAgentConfig(agentCfg AgentConfig) AgentConfig {
	return AgentConfig{
		SystemPrompt:  agentCfg.DraftSystemPrompt,
		PromptName:    agentCfg.DraftPromptName,
		PromptLabel:   agentCfg.DraftPromptLabel,
		PromptVersion: agentCfg.DraftPromptVersion,
		Provider:      firstConfigured(agentCfg.DraftProvider, agentCfg.Provider),
		Model:         firstConfigured(agentCfg.DraftModel, agentCfg.Model),
		LLMScene:      agentCfg.DraftLLMScene,
	}
}

func firstConfigured(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// resolveAgentPrompt preserves inline prompt compatibility while allowing a
// team role to name a Langfuse-managed prompt. prompt_name is the preferred
// field; langfuse_prompt remains an explicit alias.
func resolveAgentPrompt(ctx context.Context, agentCfg AgentConfig, defaultName, defaultPrompt string) string {
	return resolveAgentPromptWithSelectorFetcher(ctx, agentCfg, defaultName, defaultPrompt, promptmanager.GetManager().GetWithSelector)
}

func resolveAgentPromptForTask(ctx context.Context, agentCfg AgentConfig, defaultName, defaultPrompt string) (string, error) {
	fallback := defaultPrompt
	if strings.TrimSpace(agentCfg.SystemPrompt) != "" {
		fallback = agentCfg.SystemPrompt
	}
	promptName := strings.TrimSpace(agentCfg.PromptName)
	if promptName == "" {
		promptName = strings.TrimSpace(agentCfg.LangfusePrompt)
	}
	if promptName != "" {
		resolved, err := promptmanager.GetManager().ResolvePinned(ctx, promptName, agentPromptSelector(agentCfg), fallback)
		return resolved.Content, err
	}
	if strings.TrimSpace(agentCfg.SystemPrompt) != "" {
		return agentCfg.SystemPrompt, nil
	}
	resolved, err := promptmanager.GetManager().ResolvePinned(ctx, defaultName, agentPromptSelector(agentCfg), defaultPrompt)
	return resolved.Content, err
}

func resolveAgentPromptWithFetcher(ctx context.Context, agentCfg AgentConfig, defaultName, defaultPrompt string, fetch func(context.Context, string, string) string) string {
	return resolveAgentPromptWithSelectorFetcher(ctx, agentCfg, defaultName, defaultPrompt, func(ctx context.Context, name string, _ promptmanager.Selector, fallback string) string {
		return fetch(ctx, name, fallback)
	})
}

func resolveAgentPromptWithSelectorFetcher(ctx context.Context, agentCfg AgentConfig, defaultName, defaultPrompt string, fetch func(context.Context, string, promptmanager.Selector, string) string) string {
	fallback := defaultPrompt
	if strings.TrimSpace(agentCfg.SystemPrompt) != "" {
		fallback = agentCfg.SystemPrompt
	}
	promptName := strings.TrimSpace(agentCfg.PromptName)
	if promptName == "" {
		promptName = strings.TrimSpace(agentCfg.LangfusePrompt)
	}
	if promptName != "" {
		return fetch(ctx, promptName, agentPromptSelector(agentCfg), fallback)
	}
	if strings.TrimSpace(agentCfg.SystemPrompt) != "" {
		return agentCfg.SystemPrompt
	}
	return fetch(ctx, defaultName, agentPromptSelector(agentCfg), defaultPrompt)
}

func hasConfiguredPrompt(agentCfg AgentConfig) bool {
	return strings.TrimSpace(agentCfg.SystemPrompt) != "" || strings.TrimSpace(agentCfg.PromptName) != "" || strings.TrimSpace(agentCfg.LangfusePrompt) != "" || strings.TrimSpace(agentCfg.PromptLabel) != "" || agentCfg.PromptVersion != 0
}

func agentPromptSelector(agentCfg AgentConfig) promptmanager.Selector {
	return promptmanager.Selector{Label: agentCfg.PromptLabel, Version: agentCfg.PromptVersion}
}

type TeamConfig struct {
	Runtime      OrchestrationRuntime  `yaml:"runtime" json:"runtime,omitempty"`
	Workflow     Workflow              `yaml:"workflow" json:"workflow"`
	Routing      WorkflowRoutingConfig `yaml:"routing" json:"routing"`
	CriticPolicy CriticPolicyConfig    `yaml:"critic_policy" json:"critic_policy"`
	Planner      AgentConfig           `yaml:"planner" json:"planner"`
	Critic       AgentConfig           `yaml:"critic" json:"critic"`
	Executor     AgentConfig           `yaml:"executor" json:"executor"`
	Verifier     AgentConfig           `yaml:"verifier" json:"verifier"`
	Researcher   AgentConfig           `yaml:"researcher" json:"researcher"`
	Writer       AgentConfig           `yaml:"writer" json:"writer"`
}

type WorkflowRoutingConfig struct {
	ReviewedIntents              []string `yaml:"reviewed_intents" json:"reviewed_intents"`
	ReviewedComplexities         []string `yaml:"reviewed_complexities" json:"reviewed_complexities"`
	ReviewedMinPlanSteps         int      `yaml:"reviewed_min_plan_steps" json:"reviewed_min_plan_steps"`
	ReviewedMinRemainingLLMCalls int      `yaml:"reviewed_min_remaining_llm_calls" json:"reviewed_min_remaining_llm_calls"`
	ReviewedMinRemainingTokens   int      `yaml:"reviewed_min_remaining_tokens" json:"reviewed_min_remaining_tokens"`
	AllowResearchHighRiskTools   bool     `yaml:"allow_research_high_risk_tools" json:"allow_research_high_risk_tools"`
}

type CriticPolicyConfig struct {
	MaxReplans *int `yaml:"max_replans" json:"max_replans"`
}

type TeamsConfig struct {
	ActiveTeam         string                `yaml:"active_team" json:"active_team"`
	ResumeConfigPolicy ResumeConfigPolicy    `yaml:"resume_config_policy" json:"resume_config_policy"`
	Teams              map[string]TeamConfig `yaml:"teams" json:"teams"`
}

type teamConfigSnapshot struct {
	ActiveTeam   string
	Team         TeamConfig
	Digest       string
	ResumePolicy ResumeConfigPolicy
}

type teamConfigSnapshotContextKey struct{}
type forceLegacyRuntimeContextKey struct{}

func withForceLegacyRuntime(ctx context.Context) context.Context {
	return context.WithValue(ctx, forceLegacyRuntimeContextKey{}, true)
}

func newTeamConfigSnapshot(activeTeam string, team TeamConfig) teamConfigSnapshot {
	canonicalTeam := team
	if parseOrchestrationRuntime(string(canonicalTeam.Runtime)) == RuntimeLegacy {
		canonicalTeam.Runtime = ""
	}
	raw, _ := json.Marshal(canonicalTeam)
	digest := sha256.Sum256(raw)
	return teamConfigSnapshot{
		ActiveTeam:   activeTeam,
		Team:         team,
		Digest:       fmt.Sprintf("%x", digest[:12]),
		ResumePolicy: ResumeConfigUseLatest,
	}
}

func withTeamConfigSnapshot(ctx context.Context, snapshot teamConfigSnapshot) context.Context {
	return context.WithValue(ctx, teamConfigSnapshotContextKey{}, snapshot)
}

func teamConfigFromContext(ctx context.Context) teamConfigSnapshot {
	if ctx != nil {
		if snapshot, ok := ctx.Value(teamConfigSnapshotContextKey{}).(teamConfigSnapshot); ok {
			return snapshot
		}
	}
	teamsCfg := GetTeamsConfig()
	snapshot := newTeamConfigSnapshot(teamsCfg.ActiveTeam, teamsCfg.GetActiveTeam())
	snapshot.ResumePolicy = teamsCfg.ResumeConfigPolicy
	return snapshot
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
	if envRuntime := os.Getenv("AI_AGENT_MULTIAGENT_RUNTIME"); envRuntime != "" {
		team := cfg.Teams[cfg.ActiveTeam]
		team.Runtime = parseOrchestrationRuntime(envRuntime)
		cfg.Teams[cfg.ActiveTeam] = team
	}
	if envPolicy := os.Getenv("AI_AGENT_MULTIAGENT_RESUME_CONFIG_POLICY"); envPolicy != "" {
		cfg.ResumeConfigPolicy = parseResumeConfigPolicy(envPolicy)
	} else {
		cfg.ResumeConfigPolicy = parseResumeConfigPolicy(string(cfg.ResumeConfigPolicy))
	}

	return cfg
}

func parseResumeConfigPolicy(value string) ResumeConfigPolicy {
	normalized := strings.NewReplacer("-", "_", " ", "_").Replace(strings.ToLower(strings.TrimSpace(value)))
	switch normalized {
	case "require_match", "strict", "locked":
		return ResumeConfigRequireMatch
	case "", "use_latest", "latest":
		return ResumeConfigUseLatest
	default:
		return ResumeConfigRequireMatch
	}
}

// ActiveWorkflow returns the selected orchestration mode. Empty and
// unrecognised values deliberately preserve the original three-role workflow.
func (c *TeamsConfig) ActiveWorkflow() Workflow {
	return parseWorkflow(string(c.GetActiveTeam().Workflow))
}

// ActiveRuntime returns the rollout mode for graph execution. Unknown and
// empty values remain on the established Coordinator path.
func (c *TeamsConfig) ActiveRuntime() OrchestrationRuntime {
	return parseOrchestrationRuntime(string(c.GetActiveTeam().Runtime))
}

func parseOrchestrationRuntime(value string) OrchestrationRuntime {
	normalized := strings.NewReplacer("-", "_", " ", "_").Replace(strings.ToLower(strings.TrimSpace(value)))
	switch normalized {
	case "dag", "graph", "graph_runtime":
		return RuntimeDAG
	default:
		return RuntimeLegacy
	}
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
		if persisted.Effective == WorkflowResearch {
			reevaluated := resolveAdaptiveWorkflow(routing, task, plan)
			if reevaluated.Effective == WorkflowReviewed {
				reevaluated.Reason = "resume_escalation:" + reevaluated.Reason
				return reevaluated
			}
		}
		return persisted
	}
	return resolveAdaptiveWorkflow(routing, task, plan)
}

// resolveAdaptiveWorkflow evaluates a newly generated plan without consulting
// a persisted route. Replans use it to support one-way escalation when later
// steps are riskier than the initial plan.
func resolveAdaptiveWorkflow(routing WorkflowRoutingConfig, task *types.Task, plan *ResearchPlan) workflowRouteDecision {
	configured := WorkflowAdaptive
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
		return applyReviewedBudgetGate(workflowRouteDecision{Configured: configured, Effective: WorkflowReviewed, Reason: "complexity:" + complexity}, routing, task)
	}
	if containsRoutingValue(routing.ReviewedIntents, intent) {
		return applyReviewedBudgetGate(workflowRouteDecision{Configured: configured, Effective: WorkflowReviewed, Reason: "intent:" + intent}, routing, task)
	}
	if routing.ReviewedMinPlanSteps > 0 && plan != nil && len(plan.Steps) >= routing.ReviewedMinPlanSteps {
		return applyReviewedBudgetGate(workflowRouteDecision{Configured: configured, Effective: WorkflowReviewed, Reason: "plan_steps"}, routing, task)
	}
	return workflowRouteDecision{Configured: configured, Effective: WorkflowResearch, Reason: "default_research"}
}

func applyReviewedBudgetGate(decision workflowRouteDecision, routing WorkflowRoutingConfig, task *types.Task) workflowRouteDecision {
	if task == nil {
		return decision
	}
	if routing.ReviewedMinRemainingLLMCalls > 0 && task.LLMCallBudget > 0 {
		remaining := task.LLMCallBudget - task.LLMCalls
		if remaining < routing.ReviewedMinRemainingLLMCalls {
			decision.Effective = WorkflowResearch
			decision.Reason = "budget_fallback:llm_calls:" + decision.Reason
			return decision
		}
	}
	if routing.ReviewedMinRemainingTokens > 0 && task.TokenBudget > 0 {
		remaining := task.TokenBudget - totalTokensUsed(task)
		if remaining < routing.ReviewedMinRemainingTokens {
			decision.Effective = WorkflowResearch
			decision.Reason = "budget_fallback:tokens:" + decision.Reason
		}
	}
	return decision
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
		sceneProvider := cfg.Provider
		cfg.Provider = planner.ProviderType(agentCfg.Provider)
		// A redundant role-level provider must not discard the selected scene's
		// gateway credentials and URL. This is especially important for LiteLLM,
		// whose deployment-specific BaseURL cannot have a useful registry default.
		if cfg.Provider != sceneProvider {
			globalCfg := config.Get()
			resolved := globalCfg.ResolveLLMProviderConfig(agentCfg.Provider)
			cfg.APIKey = resolved.APIKey
			cfg.BaseURL = resolved.BaseURL
			if agentCfg.Model == "" {
				cfg.Model = resolved.Model
			}
		}
	}
	if agentCfg.Model != "" {
		cfg.Model = agentCfg.Model
	}
	return cfg
}
