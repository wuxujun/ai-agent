package multiagent

import (
	"context"
	"fmt"
	"strings"

	"github.com/wuxujun/ai-agent/internal/tools"
	"github.com/wuxujun/ai-agent/internal/types"
)

// plannerSystemPrompt instructs the LLM to produce a structured research plan.
const plannerSystemPrompt = `You are a research planning agent. Given a user goal, decompose it into
concrete, ordered research steps. Each step specifies exactly one tool action.

Available actions:
  find_files   – discover files by glob pattern  (set file_glob, e.g. "*.yaml")
  search_text  – search for keywords/text        (set search_query; optionally file_glob)
  read_file    – read a specific file's content  (set file_path)
  write_file   – write content to a file         (set file_path, content)
  execute_code – run script execution command    (set command, args; allowed commands: python3, python, go, node, bash, sh)
  git_diff     – show git diff of the workspace   (optionally set file_path for a single file)
  http_fetch   – fetch content from a public URL  (set url; private/loopback addresses are blocked)
  web_search   – search the web for keywords      (set search_query)

Rules:
1. Produce between 2 and 8 steps — prefer fewer, higher-quality steps.
2. Start broad (find_files or search_text) then narrow down or execute (read_file, write_file, execute_code).
3. Each step builds on previous findings.
4. Step IDs must be "step-1", "step-2", etc.
5. Set every unused field to an empty string "".
6. Never include steps that cannot be executed with the actions above.`

// PlannerAgent decomposes a user goal into a structured ResearchPlan using an LLM.
type PlannerAgent struct {
	Config LLMConfig
}

// jsonSchema returns the JSON Schema used to enforce structured output.
func (p *PlannerAgent) jsonSchema() map[string]any {
	// Action enum is derived from the tool registry so newly registered tools
	// become selectable by the planner without editing this schema by hand.
	stepSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id":           map[string]any{"type": "string", "description": "Unique step ID (step-1, step-2, ...)"},
			"description":  map[string]any{"type": "string", "description": "What this step investigates"},
			"action":       map[string]any{"type": "string", "enum": tools.Names()},
			"search_query": map[string]any{"type": "string", "description": "Keyword or text to search (search_text / web_search)"},
			"file_glob":    map[string]any{"type": "string", "description": "Glob pattern for find_files or search_text filter"},
			"file_path":    map[string]any{"type": "string", "description": "Relative file path for read_file / write_file / git_diff"},
			"content":      map[string]any{"type": "string", "description": "Content to write (write_file only)"},
			"command":      map[string]any{"type": "string", "description": "Command/Interpreter to run (execute_code only)"},
			"args":         map[string]any{"type": "string", "description": "Space-separated arguments (execute_code only)"},
			"url":          map[string]any{"type": "string", "description": "Absolute http/https URL to fetch (http_fetch only)"},
		},
		"required":             []string{"id", "description", "action", "search_query", "file_glob", "file_path", "content", "command", "args", "url"},
		"additionalProperties": false,
	}

	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"thought_summary": map[string]any{
				"type":        "string",
				"description": "One-sentence summary of the overall research strategy (max 60 words)",
			},
			"steps": map[string]any{
				"type":        "array",
				"items":       stepSchema,
				"minItems":    2,
				"maxItems":    8,
				"description": "Ordered list of research steps",
			},
		},
		"required":             []string{"thought_summary", "steps"},
		"additionalProperties": false,
	}
}

// Plan calls the LLM to produce a ResearchPlan for the given goal.
func (p *PlannerAgent) Plan(ctx context.Context, goal, workspace string, memories []types.Memory) (*ResearchPlan, error) {
	log.Info("Decomposing goal into research plan", "goal", goal)

	memorySection := formatMemories(memories)

	userPrompt := fmt.Sprintf(
		"Goal: %s%s\n\nWorkspace root: %s\n\n"+
			"Produce a research plan with concrete steps to achieve the goal using the available file tools.",
		goal, memorySection, workspace,
	)

	var plan ResearchPlan
	cfg := p.Config
	if cfg.Provider == "" {
		cfg = DefaultLLMConfig()
	}

	systemPrompt := plannerSystemPrompt
	teamsCfg := GetTeamsConfig()
	activeTeam := teamsCfg.GetActiveTeam()
	if activeTeam.Planner.SystemPrompt != "" {
		systemPrompt = activeTeam.Planner.SystemPrompt
		log.Info("Using custom system prompt for PlannerAgent", "team", teamsCfg.ActiveTeam, "agent_name", activeTeam.Planner.Name)
	}
	if activeTeam.Planner.Provider != "" || activeTeam.Planner.Model != "" {
		cfg = GetLLMConfig(activeTeam.Planner)
	}

	usage, err := callLLMJSON(ctx, cfg, systemPrompt, userPrompt, p.jsonSchema(), &plan)
	if err != nil {
		return nil, fmt.Errorf("PlannerAgent LLM call failed: %w", err)
	}
	plan.TokenUsage = usage

	if len(plan.Steps) == 0 {
		return nil, fmt.Errorf("PlannerAgent returned an empty steps list")
	}

	log.Info("Plan created", "summary", plan.ThoughtSummary, "steps", len(plan.Steps))
	for i, s := range plan.Steps {
		log.Info("Planned step", "num", i+1, "action", s.Action, "desc", s.Description)
	}
	return &plan, nil
}

// replannerSystemPrompt instructs the LLM to produce a revised plan after failure.
const replannerSystemPrompt = `You are a research planning agent tasked with adjusting a research plan because a step has failed.

Given a user goal, the workspace, the trace of previous steps (including the failed step and its error/observation), your job is to analyze why it failed and generate a revised plan of ordered research steps to achieve the goal.

Available actions:
  find_files   – discover files by glob pattern  (set file_glob, e.g. "*.yaml")
  search_text  – search for keywords/text        (set search_query; optionally file_glob)
  read_file    – read a specific file's content  (set file_path)
  write_file   – write content to a file         (set file_path, content)
  execute_code – run script execution command    (set command, args; allowed commands: python3, python, go, node, bash, sh)
  git_diff     – show git diff of the workspace   (optionally set file_path for a single file)
  http_fetch   – fetch content from a public URL  (set url; private/loopback addresses are blocked)
  web_search   – search the web for keywords      (set search_query)

Rules:
1. Analyze the trace and explain why you think it failed in thought_summary.
2. Generate revised next steps (between 1 and 5 steps).
3. Do not repeat the exact same failed step unless you use different arguments or parameters.
4. Step IDs must be "step-1", "step-2", etc.
5. Set every unused field to an empty string "".`

// Replan calls the LLM to generate a revised ResearchPlan when a execution step fails.
func (p *PlannerAgent) Replan(ctx context.Context, goal, workspace string, traces []types.StepTrace, memories []types.Memory) (*ResearchPlan, error) {
	log.Info("Re-planning goal due to step execution failure", "goal", goal)

	memorySection := formatMemories(memories)

	userPrompt := fmt.Sprintf(
		"Goal: %s%s\n\nWorkspace root: %s\n\n"+
			"Execution History so far:\n%s\n\n"+
			"Please analyze the failure and generate a revised plan.",
		goal, memorySection, workspace, formatTracesForReplanner(traces),
	)

	var plan ResearchPlan
	cfg := p.Config
	if cfg.Provider == "" {
		cfg = DefaultLLMConfig()
	}

	systemPrompt := replannerSystemPrompt
	teamsCfg := GetTeamsConfig()
	activeTeam := teamsCfg.GetActiveTeam()
	if activeTeam.Planner.SystemPrompt != "" {
		systemPrompt = activeTeam.Planner.SystemPrompt + "\n\nCRITICAL: One of the previous execution steps has FAILED. You must analyze the execution history, explain in thought_summary why it failed, and generate revised next steps (between 1 and 5 steps) to achieve the goal. Do not repeat the exact same failed step unless you use different arguments or parameters."
		log.Info("Using custom system prompt for ReplannerAgent", "team", teamsCfg.ActiveTeam, "agent_name", activeTeam.Planner.Name)
	}
	if activeTeam.Planner.Provider != "" || activeTeam.Planner.Model != "" {
		cfg = GetLLMConfig(activeTeam.Planner)
	}

	usage, err := callLLMJSON(ctx, cfg, systemPrompt, userPrompt, p.jsonSchema(), &plan)
	if err != nil {
		return nil, fmt.Errorf("PlannerAgent Replan LLM call failed: %w", err)
	}
	plan.TokenUsage = usage

	log.Info("Revised plan created", "summary", plan.ThoughtSummary, "steps", len(plan.Steps))
	for i, s := range plan.Steps {
		log.Info("Revised step", "num", i+1, "action", s.Action, "desc", s.Description)
	}
	return &plan, nil
}

func formatTracesForReplanner(traces []types.StepTrace) string {
	var b strings.Builder
	for _, tr := range traces {
		role := tr.AgentRole
		if role == "" {
			role = "agent"
		}
		b.WriteString(fmt.Sprintf("- Step %d [%s]: Action: %s, Query: %s\n", tr.Step, role, tr.Action, tr.Query))
		b.WriteString(fmt.Sprintf("  Observation: %s\n", tr.Observation))
	}
	return b.String()
}

func formatMemories(memories []types.Memory) string {
	if len(memories) == 0 {
		return ""
	}
	var ms []string
	for i, mem := range memories {
		// Indent key findings
		lines := strings.Split(mem.KeyFindings, "\n")
		for j, line := range lines {
			lines[j] = "    " + line
		}
		indentedFindings := strings.Join(lines, "\n")

		ms = append(ms, fmt.Sprintf("- Memory %d:\n  * Goal: %s\n  * Key Findings:\n%s\n  * Final Answer: %s",
			i+1, mem.Goal, indentedFindings, mem.FinalAnswer))
	}
	return "\n\nRelated Historical Memories (RAG - Cross-task Knowledge Sharing):\n" + strings.Join(ms, "\n\n")
}
