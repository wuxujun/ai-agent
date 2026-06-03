package multiagent

import (
	"context"
	"fmt"
	"log"

	"github.com/wuxujun/ai-agent/internal/policy"
	"github.com/wuxujun/ai-agent/internal/tools"
)

// ResearcherAgent executes ResearchSteps using local file-system tools.
// It contains no LLM — it is a pure tool executor that bridges the PlannerAgent's
// directives to the underlying tools package.
type ResearcherAgent struct{}

// Research executes a single ResearchStep and returns gathered evidence.
// Errors inside individual tool calls are treated as non-fatal: the observation
// records the error and the caller can decide whether to continue.
func (r *ResearcherAgent) Research(ctx context.Context, workspace string, step ResearchStep) (*StepEvidence, error) {
	log.Printf("[ResearcherAgent] Step %s: action=%s  desc=%q", step.ID, step.Action, step.Description)

	// Validate workspace boundary before any operation.
	if err := policy.ValidateWorkspace(workspace); err != nil {
		return nil, fmt.Errorf("ResearcherAgent workspace policy: %w", err)
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
		log.Printf("[ResearcherAgent] Step %s unsupported action: %s", step.ID, step.Action)
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
		log.Printf("[ResearcherAgent] Step %s %s error: %v", step.ID, step.Action, err)
		return ev, nil // non-fatal
	}
	ev.Observation = result.Observation
	ev.Evidence = result.Evidence

	log.Printf("[ResearcherAgent] Step %s done: %s (evidence=%d)", step.ID, ev.Observation, len(ev.Evidence))
	return ev, nil
}

// stepToParams translates a ResearchStep's semantic fields into the generic
// parameter map expected by tools.Tool.Execute. Every possible key is populated;
// each tool reads only the keys it needs, so the superset mapping is safe.
func stepToParams(step ResearchStep) map[string]interface{} {
	return map[string]interface{}{
		"pattern": step.FileGlob,    // find_files
		"glob":    step.FileGlob,    // search_text filter
		"query":   step.SearchQuery, // search_text / web_search
		"path":    step.FilePath,    // read_file / write_file / git_diff
		"content": step.Content,     // write_file
		"command": step.Command,     // execute_code
		"args":    step.Args,        // execute_code
		"url":     step.URL,         // http_fetch
	}
}
