package multiagent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/types"
)

func (c *Coordinator) runPlanPhase(ctx context.Context, task *types.Task) (*ResearchPlan, error) {
	log := teamLogger(ctx)
	log.Info("Phase 1 — Planning", "task_id", task.ID)

	start := time.Now()
	var plan *ResearchPlan
	var err error
	if decision, ok := planner.NextJITRetrievalDecision(task); ok && !decision.Stop {
		plan = researchPlanFromJITDecision(task, decision)
		log.Info("Planning resolved by JIT retrieval router", "task_id", task.ID, "action", plan.Steps[0].Action, "rag_configured", strings.TrimSpace(config.Get().RAG.SearchURL) != "")
	} else {
		plan, err = c.Planner.Plan(ctx, task.Goal, task.Workspace, task.Memories)
	}
	elapsed := time.Since(start)

	if c.Metrics != nil {
		c.Metrics.ObservePlanner(elapsed, err)
		outcome := "success"
		if err != nil {
			outcome = "error"
		}
		c.Metrics.ObserveMultiAgentPhase("planner", outcome, elapsed)
	}
	if err != nil {
		return nil, fmt.Errorf("PlannerAgent: %w", err)
	}
	if enforceJITResearchPlan(task, plan) {
		log.Info("Adjusted research plan to JIT retrieval route", "task_id", task.ID, "action", plan.Steps[0].Action)
	}
	if enforceControlledWikiInitialPlan(ctx, task, plan) {
		log.Info("Reduced controlled Wiki initial plan to one search", "task_id", task.ID)
	}
	if enforceWorkspaceResearchPlan(task, plan) {
		log.Info("Adjusted research plan to workspace discovery route", "task_id", task.ID)
	}
	ensureExplicitWorkspaceFileReads(task, plan)

	if c.Metrics != nil {
		c.Metrics.ObserveTokens(plan.TokenUsage.PromptTokens, plan.TokenUsage.CompletionTokens, plan.TokenUsage.TotalTokens, "planner")
	}
	task.Hypothesis = plan.ThoughtSummary
	planTrace := types.StepTrace{
		Step:   task.StepCount,
		Goal:   task.Goal,
		Action: "plan",
		Query:  "planner",
		Observation: fmt.Sprintf("[planner] %s — %d step(s) planned",
			plan.ThoughtSummary, len(plan.Steps)),
		AgentRole:  RolePlanner,
		TokenUsage: plan.TokenUsage,
	}
	teamSnapshot := teamConfigFromContext(ctx)
	if teamSnapshot.Digest != "" {
		planTrace.Evidence = []types.Evidence{{
			Path:  "team_config",
			Query: teamSnapshot.ActiveTeam,
			Lines: []string{"digest:" + teamSnapshot.Digest},
		}}
	}
	task.Trace = append(task.Trace, planTrace)
	task.StepCount++
	task.Status = types.StatusRunning

	log.Info("Phase 1 done", "task_id", task.ID, "steps_planned", len(plan.Steps), "elapsed", elapsed)
	return plan, nil
}

func enforceControlledWikiInitialPlan(ctx context.Context, task *types.Task, plan *ResearchPlan) bool {
	if task == nil || plan == nil {
		return false
	}
	team := teamConfigFromContext(ctx).ActiveTeam
	if team != "wiki" && team != "wiki_graph" && team != "wiki_suggest" {
		return false
	}
	for _, trace := range task.Trace {
		if strings.HasPrefix(trace.Action, "wiki_") {
			return false
		}
	}
	if len(plan.Steps) == 1 && plan.Steps[0].Action == "wiki_search" {
		return false
	}
	query := task.Goal
	for _, step := range plan.Steps {
		if step.Action == "wiki_search" && strings.TrimSpace(step.SearchQuery) != "" {
			query = step.SearchQuery
			break
		}
	}
	plan.ThoughtSummary = "Search the configured Wiki before bounded read-only follow-up"
	plan.Steps = []ResearchStep{{
		ID: "step-1", Description: "Search the configured Wiki for the target page", Action: "wiki_search", SearchQuery: query,
	}}
	return true
}

func researchPlanFromJITDecision(task *types.Task, decision *planner.PlanDecision) *ResearchPlan {
	action := decision.Actions[0]
	query, _ := action.Parameters["query"].(string)
	if query == "" {
		query = task.Goal
	}
	return &ResearchPlan{
		ThoughtSummary: decision.ThoughtSummary,
		Steps: []ResearchStep{{
			ID:                 "step-1",
			Description:        "Retrieve authoritative evidence before factual synthesis",
			Action:             action.Action,
			SearchQuery:        query,
			RepairedParameters: action.Parameters,
		}},
	}
}

// buildStepQuery constructs a structured, human-readable query string for
// StepTrace.Query. Only the fields relevant to the step's action are included,
// preventing the previous issue where all parameters were blindly concatenated
// into an unreadable string like "keyword*.go/path/file python3 args".
func buildStepQuery(step ResearchStep) string {
	switch step.Action {
	case "search_text":
		q := fmt.Sprintf("query=%q", step.SearchQuery)
		if step.FileGlob != "" {
			q += fmt.Sprintf(" glob=%q", step.FileGlob)
		}
		return q
	case "find_files":
		return fmt.Sprintf("glob=%q", step.FileGlob)
	case "read_file":
		return fmt.Sprintf("path=%q", step.FilePath)
	case "write_file":
		return fmt.Sprintf("path=%q", step.FilePath)
	case "execute_code":
		if step.Args != "" {
			return fmt.Sprintf("cmd=%q args=%q", step.Command, step.Args)
		}
		return fmt.Sprintf("cmd=%q", step.Command)
	case "git_diff":
		if step.FilePath != "" {
			return fmt.Sprintf("path=%q", step.FilePath)
		}
		return "workspace"
	case "http_fetch":
		return fmt.Sprintf("url=%q", step.URL)
	case "web_search":
		return fmt.Sprintf("query=%q", step.SearchQuery)
	case "wiki_search", "rag_search", "memory_search":
		return fmt.Sprintf("query=%q", step.SearchQuery)
	case "wiki_fetch", "rag_fetch", "memory_get":
		if ids, ok := step.RepairedParameters["ids"]; ok {
			return fmt.Sprintf("ids=%v", ids)
		}
		return "ids=[]"
	case "wiki_graph":
		return fmt.Sprintf("uri=%q depth=%d direction=%q", step.GraphURI, step.GraphDepth, step.GraphDirection)
	case "wiki_suggest":
		return fmt.Sprintf("uri=%q limit=%d", step.SuggestURI, step.SuggestLimit)
	case "analyze_image":
		return fmt.Sprintf("path=%q prompt=%q", step.FilePath, step.Prompt)
	default:
		return fmt.Sprintf("action=%q", step.Action)
	}
}

func paramsToStep(params map[string]any, step *ResearchStep) {
	step.RepairedParameters = make(map[string]any, len(params))
	for name, value := range params {
		step.RepairedParameters[name] = value
	}
	if pattern, ok := params["pattern"].(string); ok {
		step.FileGlob = pattern
	}
	if glob, ok := params["glob"].(string); ok {
		step.FileGlob = glob
	}
	if query, ok := params["query"].(string); ok {
		step.SearchQuery = query
	}
	if path, ok := params["path"].(string); ok {
		step.FilePath = path
	}
	if prompt, ok := params["prompt"].(string); ok {
		step.Prompt = prompt
	}
	if content, ok := params["content"].(string); ok {
		step.Content = content
	}
	if command, ok := params["command"].(string); ok {
		step.Command = command
	}
	if args, ok := params["args"].(string); ok {
		step.Args = args
	}
	if url, ok := params["url"].(string); ok {
		step.URL = url
	}
	if uri, ok := params["uri"].(string); ok {
		step.GraphURI = uri
		step.SuggestURI = uri
	}
	if depth, ok := params["depth"].(int); ok {
		step.GraphDepth = depth
	} else if depth, ok := params["depth"].(float64); ok {
		step.GraphDepth = int(depth)
	}
	if direction, ok := params["direction"].(string); ok {
		step.GraphDirection = direction
	}
	if limit, ok := params["limit"].(int); ok {
		step.SuggestLimit = limit
	} else if limit, ok := params["limit"].(float64); ok {
		step.SuggestLimit = int(limit)
	}
}
