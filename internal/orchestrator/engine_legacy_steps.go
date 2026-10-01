package orchestrator

import (
	"context"
	"fmt"
	"strings"

	"github.com/wuxujun/ai-agent/internal/tools"
	"github.com/wuxujun/ai-agent/internal/types"
)

func stepFindTextFiles(ctx context.Context, task *types.Task) error {
	engineLog.Info("legacy static path - finding text files", "task_id", task.ID)
	task.Hypothesis = "Relevant evidence is likely inside text or markdown files"

	txtFiles, err := tools.FindFiles(ctx, task.Workspace, "*.txt")
	if err != nil {
		engineLog.Error("legacy static path - FindFiles (*.txt) failed", "task_id", task.ID, "error", err)
		return err
	}
	mdFiles, err := tools.FindFiles(ctx, task.Workspace, "*.md")
	if err != nil {
		engineLog.Error("legacy static path - FindFiles (*.md) failed", "task_id", task.ID, "error", err)
		return err
	}

	files := append(txtFiles, mdFiles...)
	if len(files) > 20 {
		files = files[:20]
	}

	engineLog.Info("legacy static path - found text/markdown files", "task_id", task.ID, "count", len(files))

	task.Trace = append(task.Trace, types.StepTrace{
		Step:        task.StepCount,
		Goal:        task.Goal,
		Action:      "find_files",
		Query:       "*.txt, *.md",
		Observation: fmt.Sprintf("found %d candidate files", len(files)),
	})

	if len(files) == 0 {
		task.Unresolved = append(task.Unresolved, "no candidate text or markdown files found")
	}
	_ = SetTaskRunning(task)
	return nil
}

func stepSearchKeyword(ctx context.Context, task *types.Task) error {
	query, err := lastWord(task.Goal)
	if err != nil {
		engineLog.Error("legacy static path - failed to extract keyword", "task_id", task.ID, "error", err)
		return err
	}
	engineLog.Info("legacy static path - searching keyword", "keyword", query, "task_id", task.ID)
	task.Hypothesis = "Search the most likely keyword in candidate text files"

	evidence, _, err := tools.SearchWithRG(ctx, task.Workspace, query, "*.txt")
	if err != nil {
		engineLog.Error("legacy static path - SearchWithRG failed", "task_id", task.ID, "error", err)
		return err
	}

	engineLog.Info("legacy static path - found evidence items", "task_id", task.ID, "count", len(evidence))

	task.Trace = append(task.Trace, types.StepTrace{
		Step:        task.StepCount,
		Goal:        task.Goal,
		Action:      "search_text",
		Query:       query,
		Observation: fmt.Sprintf("found %d evidence items", len(evidence)),
		Evidence:    evidence,
	})

	if len(evidence) == 0 {
		task.Unresolved = append(task.Unresolved, "keyword not found")
	}
	_ = SetTaskRunning(task)
	return nil
}

func stepReadBestFile(ctx context.Context, task *types.Task) error {
	engineLog.Info("legacy static path - reading best file", "task_id", task.ID)
	if len(task.Trace) < 2 || len(task.Trace[1].Evidence) == 0 {
		engineLog.Info("legacy static path - not enough evidence to select a file", "task_id", task.ID)
		task.Trace = append(task.Trace, types.StepTrace{
			Step:        task.StepCount,
			Goal:        task.Goal,
			Action:      "read_file",
			Observation: "skipped: not enough evidence to select a file",
		})
		_ = SetTaskCompleted(task, "not enough evidence to select a file")
		return nil
	}

	target := task.Trace[1].Evidence[0].Path
	engineLog.Info("legacy static path - target best file identified", "task_id", task.ID, "file", target)
	content, err := tools.ReadFile(task.Workspace, target)
	if err != nil {
		engineLog.Error("legacy static path - ReadFile failed", "task_id", task.ID, "error", err)
		return err
	}

	snippet := content
	if len(snippet) > 220 {
		snippet = snippet[:220]
	}

	task.Trace = append(task.Trace, types.StepTrace{
		Step:        task.StepCount,
		Goal:        task.Goal,
		Action:      "read_file",
		Query:       target,
		Observation: "read file snippet: " + snippet,
	})

	_ = SetTaskCompleted(task, fmt.Sprintf("completed search; best candidate file: %s", target))
	return nil
}

func lastWord(s string) (string, error) {
	parts := strings.Fields(s)
	if len(parts) == 0 {
		return "", fmt.Errorf("empty input, cannot extract keyword")
	}
	return parts[len(parts)-1], nil
}
