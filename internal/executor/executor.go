package executor

import (
	"context"
	"fmt"

	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/policy"
	"github.com/wuxujun/ai-agent/internal/tools"
	"github.com/wuxujun/ai-agent/internal/types"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

type Executor interface {
	Execute(ctx context.Context, task *types.Task, decision *planner.PlanDecision) (*types.StepTrace, error)
}

type DefaultExecutor struct{}

var tracer = otel.Tracer("agent-runtime/executor")

func (e *DefaultExecutor) Execute(ctx context.Context, task *types.Task, d *planner.PlanDecision) (*types.StepTrace, error) {
	ctx, span := tracer.Start(ctx, "executor.execute")
	defer span.End()

	span.SetAttributes(
		attribute.String("agent.task.id", task.ID),
		attribute.String("agent.executor.action", d.Action),
	)

	trace := &types.StepTrace{
		Step:   task.StepCount + 1,
		Goal:   task.Goal,
		Action: d.Action,
	}

	switch d.Action {
	case "find_files":
		pattern, _ := d.Parameters["pattern"].(string)
		// Validate workspace boundary before executing
		if err := policy.ValidateWorkspace(task.Workspace); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "workspace policy violation")
			return nil, fmt.Errorf("find_files policy violation: %w", err)
		}
		files, err := tools.FindFiles(task.Workspace, pattern)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "find_files failed")
			return nil, err
		}
		trace.Query = pattern
		trace.Observation = fmt.Sprintf("found %d candidate files", len(files))
		span.SetAttributes(attribute.Int("agent.executor.file_count", len(files)))
		return trace, nil

	case "search_text":
		query, _ := d.Parameters["query"].(string)
		glob, _ := d.Parameters["glob"].(string)
		// Validate workspace boundary before executing
		if err := policy.ValidateWorkspace(task.Workspace); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "workspace policy violation")
			return nil, fmt.Errorf("search_text policy violation: %w", err)
		}
		evidence, _, err := tools.SearchWithRG(task.Workspace, query, glob)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "search_text failed")
			return nil, err
		}
		trace.Query = query
		trace.Observation = fmt.Sprintf("found %d evidence items", len(evidence))
		trace.Evidence = evidence
		span.SetAttributes(attribute.Int("agent.executor.evidence_count", len(evidence)))
		return trace, nil

	case "read_file":
		path, _ := d.Parameters["path"].(string)
		content, err := tools.ReadFile(task.Workspace, path)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "read_file failed")
			return nil, err
		}
		// tools.ReadFile already caps content at 4000 chars; no further truncation needed.
		trace.Query = path
		trace.Observation = "read file content: " + content
		span.SetAttributes(
			attribute.String("agent.executor.path", path),
			attribute.Int("agent.executor.content_len", len(content)),
		)
		return trace, nil

	case "write_file":
		path, _ := d.Parameters["path"].(string)
		content, _ := d.Parameters["content"].(string)
		if err := policy.ValidateWorkspace(task.Workspace); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "workspace policy violation")
			return nil, fmt.Errorf("write_file policy violation: %w", err)
		}
		err := tools.WriteFile(task.Workspace, path, content)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "write_file failed")
			return nil, err
		}
		trace.Query = path
		trace.Observation = fmt.Sprintf("successfully wrote %d characters to %s", len(content), path)
		span.SetAttributes(
			attribute.String("agent.executor.path", path),
			attribute.Int("agent.executor.write_len", len(content)),
		)
		return trace, nil

	case "execute_code":
		command, _ := d.Parameters["command"].(string)
		args, _ := d.Parameters["args"].(string)
		if err := policy.ValidateWorkspace(task.Workspace); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "workspace policy violation")
			return nil, fmt.Errorf("execute_code policy violation: %w", err)
		}
		output, err := tools.ExecuteCode(task.Workspace, command, args)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "execute_code failed")
			return nil, fmt.Errorf("execute_code error: %w. Output: %s", err, output)
		}
		trace.Query = command + " " + args
		obs := output
		if len(obs) > 4000 {
			obs = obs[:4000]
		}
		trace.Observation = "command executed. Output:\n" + obs
		span.SetAttributes(
			attribute.String("agent.executor.command", command),
			attribute.String("agent.executor.args", args),
			attribute.Int("agent.executor.output_len", len(output)),
		)
		return trace, nil

	default:
		err := fmt.Errorf("unsupported action: %s", d.Action)
		span.RecordError(err)
		span.SetStatus(codes.Error, "unsupported action")
		return nil, err
	}
}
