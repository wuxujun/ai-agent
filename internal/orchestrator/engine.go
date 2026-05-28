package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/wuxujun/ai-agent/internal/executor"
	"github.com/wuxujun/ai-agent/internal/metrics"
	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/tools"
	"github.com/wuxujun/ai-agent/pkg/types"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

type Engine struct {
	Planner  planner.Planner
	Executor executor.Executor
	Metrics  *metrics.Collector
}

var tracer = otel.Tracer("agent-runtime/orchestrator")

func (e *Engine) Next(ctx context.Context, task *types.Task) error {
	ctx, span := tracer.Start(ctx, "engine.next")
	defer span.End()

	span.SetAttributes(
		attribute.String("agent.task.id", task.ID),
		attribute.String("agent.task.status", task.Status),
		attribute.Int("agent.task.step_count", task.StepCount),
		attribute.Int("agent.task.max_steps", task.MaxSteps),
		attribute.Int("agent.task.tool_budget", task.ToolBudget),
	)

	if task.StepCount >= task.MaxSteps || task.ToolBudget <= 0 {
		task.Status = "completed"
		if task.FinalAnswer == "" {
			task.FinalAnswer = "stopped by budget or max steps"
		}
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
		span.RecordError(err)
		span.SetStatus(codes.Error, "planner failure")
		return err
	}

	task.Hypothesis = decision.ThoughtSummary
	span.SetAttributes(
		attribute.String("agent.planner.action", decision.Action),
		attribute.Bool("agent.planner.stop", decision.Stop),
	)

	if decision.Stop {
		task.Status = "completed"
		task.FinalAnswer = decision.FinalAnswer
		if e.Metrics != nil {
			e.Metrics.IncCompleted()
		}
		span.SetAttributes(attribute.String("agent.task.final_reason", "planner_stop"))
		return nil
	}

	xStart := time.Now()
	trace, err := e.Executor.Execute(ctx, task, decision)
	if e.Metrics != nil {
		e.Metrics.ObserveExecutor(time.Since(xStart), err, decision.Action)
	}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "executor failure")
		return err
	}

	task.StepCount++
	task.ToolBudget--
	task.Trace = append(task.Trace, *trace)
	task.Status = "running"

	span.SetAttributes(
		attribute.Int("agent.task.step_count_after", task.StepCount),
		attribute.Int("agent.task.tool_budget_after", task.ToolBudget),
	)

	return nil
}

func (e *Engine) RunAll(ctx context.Context, task *types.Task) error {
	ctx, span := tracer.Start(ctx, "engine.run_all")
	defer span.End()

	span.SetAttributes(
		attribute.String("agent.task.id", task.ID),
		attribute.String("agent.task.goal", task.Goal),
	)

	if e.Metrics != nil {
		e.Metrics.IncRunAll()
	}

	for task.Status != "completed" {
		select {
		case <-ctx.Done():
			span.RecordError(ctx.Err())
			span.SetStatus(codes.Error, "context canceled")
			return ctx.Err()
		default:
		}

		if err := e.Next(ctx, task); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "run_all failed")
			return err
		}
	}

	span.SetAttributes(attribute.String("agent.task.final_answer", task.FinalAnswer))
	return nil
}

func stepFindTextFiles(task *types.Task) error {
	task.Hypothesis = "Relevant evidence is likely inside text files"

	files, err := tools.FindFiles(task.Workspace, "*.txt")
	if err != nil {
		return err
	}

	task.Trace = append(task.Trace, types.StepTrace{
		Step:        task.StepCount,
		Goal:        task.Goal,
		Action:      "find_files",
		Query:       "*.txt",
		Observation: fmt.Sprintf("found %d candidate files", len(files)),
	})

	if len(files) == 0 {
		task.Unresolved = append(task.Unresolved, "no candidate text files found")
	}
	task.Status = "running"
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
	task.Status = "running"
	return nil
}

func stepReadBestFile(task *types.Task) error {
	if len(task.Trace) < 2 || len(task.Trace[1].Evidence) == 0 {
		task.Status = "completed"
		task.FinalAnswer = "not enough evidence to select a file"
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

	task.Status = "completed"
	task.FinalAnswer = fmt.Sprintf("completed search; best candidate file: %s", target)
	return nil
}

func lastWord(s string) string {
	parts := strings.Fields(s)
	if len(parts) == 0 {
		return "TODO"
	}
	return parts[len(parts)-1]
}
