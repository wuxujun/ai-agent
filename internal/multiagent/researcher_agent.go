package multiagent

import (
	"context"
	"fmt"
	"log"
	"path/filepath"

	"github.com/wuxujun/ai-agent/internal/policy"
	"github.com/wuxujun/ai-agent/internal/tools"
	"github.com/wuxujun/ai-agent/internal/types"
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

	switch step.Action {

	case "find_files":
		glob := step.FileGlob
		if glob == "" {
			glob = "*" // default: all files
		}
		files, err := tools.FindFiles(workspace, glob)
		if err != nil {
			ev.Observation = fmt.Sprintf("find_files error: %v", err)
			log.Printf("[ResearcherAgent] Step %s find_files error: %v", step.ID, err)
			return ev, nil // non-fatal
		}
		ev.Observation = fmt.Sprintf("found %d file(s) matching %q", len(files), glob)
		for _, f := range files {
			ev.Evidence = append(ev.Evidence, types.Evidence{
				Path:  f,
				Lines: []string{"<file found>"},
				Query: glob,
			})
		}

	case "search_text":
		query := step.SearchQuery
		if query == "" {
			ev.Observation = "search_text skipped: empty search_query"
			return ev, nil
		}
		evidence, _, err := tools.SearchWithRG(workspace, query, step.FileGlob)
		if err != nil {
			ev.Observation = fmt.Sprintf("search_text error: %v", err)
			log.Printf("[ResearcherAgent] Step %s search_text error: %v", step.ID, err)
			return ev, nil // non-fatal
		}
		ev.Observation = fmt.Sprintf("found %d match(es) for %q", len(evidence), query)
		ev.Evidence = evidence

	case "read_file":
		path := step.FilePath
		if path == "" {
			ev.Observation = "read_file skipped: empty file_path"
			return ev, nil
		}
		// Validate the specific target path.
		full := filepath.Join(workspace, path)
		if err := policy.ValidateReadPath(workspace, full); err != nil {
			ev.Observation = fmt.Sprintf("read_file policy violation: %v", err)
			log.Printf("[ResearcherAgent] Step %s policy violation: %v", step.ID, err)
			return ev, nil // non-fatal; report and continue
		}
		content, err := tools.ReadFile(workspace, path)
		if err != nil {
			ev.Observation = fmt.Sprintf("read_file error: %v", err)
			log.Printf("[ResearcherAgent] Step %s read_file error: %v", step.ID, err)
			return ev, nil // non-fatal
		}
		ev.Observation = fmt.Sprintf("read %d char(s) from %q", len(content), path)
		ev.Evidence = []types.Evidence{{
			Path:  path,
			Lines: []string{content},
			Query: path,
		}}

	case "write_file":
		path := step.FilePath
		if path == "" {
			ev.Observation = "write_file skipped: empty file_path"
			return ev, nil
		}
		full := filepath.Join(workspace, path)
		if err := policy.ValidateWritePath(workspace, full); err != nil {
			ev.Observation = fmt.Sprintf("write_file policy violation: %v", err)
			log.Printf("[ResearcherAgent] Step %s policy violation: %v", step.ID, err)
			return ev, nil
		}
		err := tools.WriteFile(workspace, path, step.Content)
		if err != nil {
			ev.Observation = fmt.Sprintf("write_file error: %v", err)
			log.Printf("[ResearcherAgent] Step %s write_file error: %v", step.ID, err)
			return ev, nil
		}
		ev.Observation = fmt.Sprintf("successfully wrote %d char(s) to %q", len(step.Content), path)

	case "execute_code":
		command := step.Command
		if command == "" {
			ev.Observation = "execute_code skipped: empty command"
			return ev, nil
		}
		output, err := tools.ExecuteCode(workspace, command, step.Args)
		if err != nil {
			ev.Observation = fmt.Sprintf("execute_code error: %v. Output: %s", err, output)
			log.Printf("[ResearcherAgent] Step %s execute_code error: %v", step.ID, err)
			return ev, nil
		}
		obs := output
		if len(obs) > 4000 {
			obs = obs[:4000]
		}
		ev.Observation = fmt.Sprintf("command executed. Output:\n%s", obs)

	default:
		ev.Observation = fmt.Sprintf("unsupported action %q — skipping", step.Action)
		log.Printf("[ResearcherAgent] Step %s unsupported action: %s", step.ID, step.Action)
	}

	log.Printf("[ResearcherAgent] Step %s done: %s (evidence=%d)", step.ID, ev.Observation, len(ev.Evidence))
	return ev, nil
}
