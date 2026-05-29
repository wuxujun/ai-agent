package orchestrator

import (
	"context"
	"log"
	"time"

	"github.com/cloudwego/eino/compose"
	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/types"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

type einoStepState struct {
	Task     *types.Task
	Decision *planner.PlanDecision
}

func (e *Engine) runEinoNext(ctx context.Context, task *types.Task) error {
	ctx, span := tracer.Start(ctx, "engine.next")
	defer span.End()

	log.Printf("[Engine-Eino] Running step %d/%d (budget: %d) for task %s", task.StepCount+1, task.MaxSteps, task.ToolBudget, task.ID)

	span.SetAttributes(
		attribute.String("agent.task.id", task.ID),
		attribute.String("agent.task.status", string(task.Status)),
		attribute.Int("agent.task.step_count", task.StepCount),
		attribute.Int("agent.task.max_steps", task.MaxSteps),
		attribute.Int("agent.task.tool_budget", task.ToolBudget),
		attribute.String("agent.orchestrator", "eino"),
	)

	runner, err := e.compileEinoStepChain(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "compile eino chain failed")
		return err
	}

	output, err := runner.Invoke(ctx, &einoStepState{Task: task})
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "eino chain failed")
		return err
	}

	span.SetAttributes(
		attribute.String("agent.task.status_after", string(output.Status)),
		attribute.Int("agent.task.step_count_after", output.StepCount),
		attribute.Int("agent.task.tool_budget_after", output.ToolBudget),
	)

	return nil
}

func (e *Engine) compileEinoStepChain(ctx context.Context) (compose.Runnable[*einoStepState, *types.Task], error) {
	chain := compose.NewChain[*einoStepState, *types.Task]()

	chain.
		AppendLambda(compose.InvokableLambda(e.checkBudget), compose.WithNodeName("budget_guard")).
		AppendLambda(compose.InvokableLambda(e.planNext), compose.WithNodeName("planner")).
		AppendLambda(compose.InvokableLambda(e.executeDecision), compose.WithNodeName("executor")).
		AppendLambda(compose.InvokableLambda(func(ctx context.Context, state *einoStepState) (*types.Task, error) {
			return state.Task, nil
		}), compose.WithNodeName("task_output"))

	return chain.Compile(ctx)
}

func (e *Engine) checkBudget(ctx context.Context, state *einoStepState) (*einoStepState, error) {
	task := state.Task
	if task.StepCount < task.MaxSteps && task.ToolBudget > 0 {
		return state, nil
	}

	log.Printf("[Engine-Eino] Task %s reached step limit (%d/%d) or budget limit (%d)", task.ID, task.StepCount, task.MaxSteps, task.ToolBudget)
	finalAnswer := task.FinalAnswer
	if finalAnswer == "" {
		finalAnswer = "stopped by budget or max steps"
	}
	_ = SetTaskCompleted(task, finalAnswer)
	if e.Metrics != nil {
		e.Metrics.IncCompleted()
	}
	return state, nil
}

func (e *Engine) planNext(ctx context.Context, state *einoStepState) (*einoStepState, error) {
	task := state.Task
	if task.Status == types.StatusCompleted {
		return state, nil
	}

	pStart := time.Now()
	decision, err := e.Planner.PlanNext(ctx, task)
	if e.Metrics != nil {
		e.Metrics.ObservePlanner(time.Since(pStart), err)
	}
	if err != nil {
		log.Printf("[Engine-Eino] Planner failed for task %s: %v", task.ID, err)
		return state, err
	}

	log.Printf("[Engine-Eino] Task %s - Planner thought: %q | Action chosen: %q", task.ID, decision.ThoughtSummary, decision.Action)

	task.Hypothesis = decision.ThoughtSummary
	state.Decision = decision

	if decision.Stop {
		log.Printf("[Engine-Eino] Task %s - Planner decided to stop. FinalAnswer: %q", task.ID, decision.FinalAnswer)
		_ = SetTaskCompleted(task, decision.FinalAnswer)
		if e.Metrics != nil {
			e.Metrics.IncCompleted()
		}
	}

	return state, nil
}

func (e *Engine) executeDecision(ctx context.Context, state *einoStepState) (*einoStepState, error) {
	task := state.Task
	decision := state.Decision
	if task.Status == types.StatusCompleted || decision == nil {
		return state, nil
	}

	log.Printf("[Engine-Eino] Task %s - Executing action %q with parameters: %+v", task.ID, decision.Action, decision.Parameters)
	xStart := time.Now()
	trace, err := e.Executor.Execute(ctx, task, decision)
	if e.Metrics != nil {
		e.Metrics.ObserveExecutor(time.Since(xStart), err, decision.Action)
	}
	if err != nil {
		log.Printf("[Engine-Eino] Executor failed for task %s, action %q: %v", task.ID, decision.Action, err)
		return state, err
	}

	log.Printf("[Engine-Eino] Task %s - Action execution success. Observation: %q", task.ID, trace.Observation)

	task.StepCount++
	task.ToolBudget--
	task.Trace = append(task.Trace, *trace)
	_ = SetTaskRunning(task)

	log.Printf("[Engine-Eino] Step %d completed for task %s. Remaining budget: %d", task.StepCount, task.ID, task.ToolBudget)
	return state, nil
}
