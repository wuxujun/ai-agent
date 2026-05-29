package orchestrator

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/wuxujun/ai-agent/internal/executor"
	"github.com/wuxujun/ai-agent/internal/metrics"
	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/tools"
	"github.com/wuxujun/ai-agent/internal/types"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"google.golang.org/adk/model"
)

type Engine struct {
	Planner  planner.Planner
	Executor executor.Executor
	Metrics  *metrics.Collector
	Mode     Mode
	AdkModel model.LLM
}

var tracer = otel.Tracer("ai-agent/orchestrator")

type Mode string

const (
	ModeEino   Mode = "eino"
	ModeLegacy Mode = "legacy"
	ModeAdk    Mode = "adk"
)

func (e *Engine) Next(ctx context.Context, task *types.Task) (err error) {
	defer func() {
		if err != nil {
			_ = SetTaskFailed(task, err.Error())
		}
	}()

	switch e.Mode {
	case "", ModeEino:
		err = e.runEinoNext(ctx, task)
	case ModeLegacy:
		err = e.runLegacyNext(ctx, task)
	case ModeAdk:
		err = e.runAdkNext(ctx, task)
	default:
		err = fmt.Errorf("unsupported orchestrator mode: %s", e.Mode)
	}
	return err
}

func (e *Engine) runLegacyNext(ctx context.Context, task *types.Task) error {
	ctx, span := tracer.Start(ctx, "engine.next")
	defer span.End()

	log.Printf("[Engine] Running step %d/%d (budget: %d) for task %s", task.StepCount+1, task.MaxSteps, task.ToolBudget, task.ID)

	span.SetAttributes(
		attribute.String("agent.task.id", task.ID),
		attribute.String("agent.task.status", string(task.Status)),
		attribute.Int("agent.task.step_count", task.StepCount),
		attribute.Int("agent.task.max_steps", task.MaxSteps),
		attribute.Int("agent.task.tool_budget", task.ToolBudget),
		attribute.String("agent.orchestrator", "legacy"),
	)

	if task.StepCount >= task.MaxSteps || task.ToolBudget <= 0 {
		log.Printf("[Engine] Task %s reached step limit (%d/%d) or budget limit (%d)", task.ID, task.StepCount, task.MaxSteps, task.ToolBudget)
		finalAnswer := task.FinalAnswer
		if finalAnswer == "" {
			finalAnswer = "stopped by budget or max steps"
		}
		_ = SetTaskCompleted(task, finalAnswer)
		if e.Metrics != nil {
			e.Metrics.IncCompleted()
		}
		span.SetAttributes(attribute.String("agent.task.final_reason", "budget_or_max_steps"))
		return nil
	}

	pStart := time.Now()
	decision, err := e.Planner.PlanNext(ctx, task)
	if e.Metrics != nil {
		e.Metrics.ObservePlanner(time.Since(pStart), err)
	}
	if err != nil {
		log.Printf("[Engine Error] Planner failed for task %s: %v", task.ID, err)
		span.RecordError(err)
		span.SetStatus(codes.Error, "planner failure")
		return err
	}

	log.Printf("[Engine] Task %s - Planner thought: %q | Action chosen: %q", task.ID, decision.ThoughtSummary, decision.Action)

	task.Hypothesis = decision.ThoughtSummary
	span.SetAttributes(
		attribute.String("agent.planner.action", decision.Action),
		attribute.Bool("agent.planner.stop", decision.Stop),
	)

	if decision.Stop {
		log.Printf("[Engine] Task %s - Planner decided to stop. FinalAnswer: %q", task.ID, decision.FinalAnswer)
		_ = SetTaskCompleted(task, decision.FinalAnswer)
		if e.Metrics != nil {
			e.Metrics.IncCompleted()
		}
		span.SetAttributes(attribute.String("agent.task.final_reason", "planner_stop"))
		return nil
	}

	log.Printf("[Engine] Task %s - Executing action %q with parameters: %+v", task.ID, decision.Action, decision.Parameters)
	xStart := time.Now()
	trace, err := e.Executor.Execute(ctx, task, decision)
	if e.Metrics != nil {
		e.Metrics.ObserveExecutor(time.Since(xStart), err, decision.Action)
	}
	if err != nil {
		log.Printf("[Engine Error] Executor failed for task %s, action %q: %v", task.ID, decision.Action, err)
		span.RecordError(err)
		span.SetStatus(codes.Error, "executor failure")
		return err
	}

	log.Printf("[Engine] Task %s - Action execution success. Observation: %q", task.ID, trace.Observation)

	task.StepCount++
	task.ToolBudget--
	task.Trace = append(task.Trace, *trace)
	_ = SetTaskRunning(task)

	log.Printf("[Engine] Step %d completed for task %s. Remaining budget: %d", task.StepCount, task.ID, task.ToolBudget)

	span.SetAttributes(
		attribute.Int("agent.task.step_count_after", task.StepCount),
		attribute.Int("agent.task.tool_budget_after", task.ToolBudget),
	)

	return nil
}

func (e *Engine) RunAll(ctx context.Context, task *types.Task) error {
	ctx, span := tracer.Start(ctx, "engine.run_all")
	defer span.End()

	log.Printf("[Engine] Starting task %s to completion. Goal: %q", task.ID, task.Goal)

	span.SetAttributes(
		attribute.String("agent.task.id", task.ID),
		attribute.String("agent.task.goal", task.Goal),
	)

	if e.Metrics != nil {
		e.Metrics.IncRunAll()
	}

	for task.Status != types.StatusCompleted && task.Status != types.StatusFailed {
		select {
		case <-ctx.Done():
			log.Printf("[Engine Warning] Task %s canceled: %v", task.ID, ctx.Err())
			span.RecordError(ctx.Err())
			span.SetStatus(codes.Error, "context canceled")
			return ctx.Err()
		default:
		}

		if err := e.Next(ctx, task); err != nil {
			log.Printf("[Engine Error] Execution step failed for task %s: %v", task.ID, err)
			span.RecordError(err)
			span.SetStatus(codes.Error, "run_all failed")
			return err
		}
	}

	log.Printf("[Engine] Task %s finished. Status: %s | FinalAnswer: %q", task.ID, task.Status, task.FinalAnswer)
	span.SetAttributes(attribute.String("agent.task.final_answer", task.FinalAnswer))
	return nil
}

func stepFindTextFiles(task *types.Task) error {
	task.Hypothesis = "Relevant evidence is likely inside text or markdown files"

	txtFiles, err := tools.FindFiles(task.Workspace, "*.txt")
	if err != nil {
		return err
	}
	mdFiles, err := tools.FindFiles(task.Workspace, "*.md")
	if err != nil {
		return err
	}

	files := append(txtFiles, mdFiles...)
	if len(files) > 20 {
		files = files[:20]
	}

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

func stepSearchKeyword(task *types.Task) error {
	query := lastWord(task.Goal)
	task.Hypothesis = "Search the most likely keyword in candidate text files"

	evidence, _, err := tools.SearchWithRG(task.Workspace, query, "*.txt")
	if err != nil {
		return err
	}

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

func stepReadBestFile(task *types.Task) error {
	if len(task.Trace) < 2 || len(task.Trace[1].Evidence) == 0 {
		_ = SetTaskCompleted(task, "not enough evidence to select a file")
		return nil
	}

	target := task.Trace[1].Evidence[0].Path
	content, err := tools.ReadFile(task.Workspace, target)
	if err != nil {
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

func lastWord(s string) string {
	parts := strings.Fields(s)
	if len(parts) == 0 {
		return "TODO"
	}
	return parts[len(parts)-1]
}
