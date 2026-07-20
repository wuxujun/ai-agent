package multiagent

import (
	"context"
	"fmt"

	"github.com/wuxujun/ai-agent/internal/policy"
	"github.com/wuxujun/ai-agent/internal/tools"
)

// ResearcherAgent executes ResearchSteps using local file-system tools.
// It contains no LLM — it is a pure tool executor that bridges the PlannerAgent's
// directives to the underlying tools package.
type ResearcherAgent struct{}

// ExecutorAgent is the tool-running role used by the reviewed four-role
// workflow. It shares the policy-enforced execution implementation with the
// legacy ResearcherAgent but has independent team identity and trace roles.
type ExecutorAgent struct{}

func (e *ExecutorAgent) Execute(ctx context.Context, workspace string, step ResearchStep) (*StepEvidence, error) {
	agentName := "Executor"
	activeTeam := GetTeamsConfig().GetActiveTeam()
	if activeTeam.Executor.Name != "" {
		agentName = activeTeam.Executor.Name
	}
	log.Info("Execution step starting", "step_id", step.ID, "action", step.Action, "desc", step.Description, "agent_name", agentName)
	return executeResearchStep(ctx, workspace, step, "ExecutorAgent")
}

// Research executes a single ResearchStep and returns gathered evidence.
// Errors inside individual tool calls are treated as non-fatal: the observation
// records the error and the caller can decide whether to continue.
func (r *ResearcherAgent) Research(ctx context.Context, workspace string, step ResearchStep) (*StepEvidence, error) {
	agentName := "Researcher"
	teamsCfg := GetTeamsConfig()
	activeTeam := teamsCfg.GetActiveTeam()
	if activeTeam.Researcher.Name != "" {
		agentName = activeTeam.Researcher.Name
	}
	log.Info("Research step starting", "step_id", step.ID, "action", step.Action, "desc", step.Description, "agent_name", agentName)
	return executeResearchStep(ctx, workspace, step, "ResearcherAgent")
}

func executeResearchStep(ctx context.Context, workspace string, step ResearchStep, roleName string) (*StepEvidence, error) {
	// Validate workspace boundary before any operation.
	if err := policy.ValidateWorkspace(workspace); err != nil {
		return nil, fmt.Errorf("%s workspace policy: %w", roleName, err)
	}

	ev := &StepEvidence{
		StepID:   step.ID,
		StepDesc: step.Description,
		Action:   step.Action,
	}

	tool, ok := tools.Get(step.Action)
	if !ok {
		ev.Observation = fmt.Sprintf("unsupported action %q — skipping", step.Action)
		ev.Failed = true
		log.Info("Unsupported action", "step_id", step.ID, "action", step.Action)
		return ev, nil
	}

	params := stepToParams(step)
	// Apply default for find_files if glob is empty
	if step.Action == "find_files" && params["pattern"] == "" {
		params["pattern"] = "*"
	}

	result, err := tool.Execute(ctx, workspace, params)
	if err != nil {
		ev.Observation = fmt.Sprintf("%s error: %v", step.Action, err)
		ev.Failed = true
		log.Info("Step tool error", "step_id", step.ID, "action", step.Action, "error", err)
		return ev, nil // non-fatal
	}
	ev.Observation = result.Observation
	ev.Evidence = result.Evidence
	ev.TokenUsage = result.TokenUsage

	log.Info("Research step done", "step_id", step.ID, "observation", ev.Observation, "evidence_count", len(ev.Evidence))
	return ev, nil
}

// stepToParams translates a ResearchStep's semantic fields into the generic
// parameter map expected by tools.Tool.Execute. Every possible key is populated;
// each tool reads only the keys it needs, so the superset mapping is safe.
func stepToParams(step ResearchStep) map[string]interface{} {
	if step.RepairedParameters != nil {
		params := make(map[string]interface{}, len(step.RepairedParameters))
		for name, value := range step.RepairedParameters {
			params[name] = value
		}
		return params
	}
	return map[string]interface{}{
		"pattern": step.FileGlob,    // find_files
		"glob":    step.FileGlob,    // search_text filter
		"query":   step.SearchQuery, // search_text / web_search
		"path":    step.FilePath,    // read_file / write_file / git_diff
		"content": step.Content,     // write_file
		"command": step.Command,     // execute_code
		"args":    step.Args,        // execute_code
		"url":     step.URL,         // http_fetch
		"prompt":  step.Prompt,      // analyze_image
	}
}
