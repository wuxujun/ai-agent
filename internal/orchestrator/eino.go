package orchestrator

import (
	"context"
	"fmt"
	"time"

	"github.com/cloudwego/eino/compose"
	"github.com/wuxujun/ai-agent/internal/logger"
	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/types"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// olog is the shared structured logger for the orchestrator package.
// Named "olog" to avoid conflicting with adk.go's stdlib "log" import.
var olog = logger.Component("orchestrator")

// log is a package-level logger alias to olog to support other files.
var log = olog

type einoStepState struct {
	Task     *types.Task
	Decision *planner.PlanDecision
}

func (e *Engine) runEinoNext(ctx context.Context, task *types.Task) error {
	ctx, span := tracer.Start(ctx, "engine.next")
	defer span.End()

	olog.Info("running step", "step", task.StepCount+1, "max_steps", task.MaxSteps, "budget", task.ToolBudget, "task_id", task.ID)

	span.SetAttributes(
		attribute.String("agent.task.id", task.ID),
		attribute.String("agent.task.status", string(task.Status)),
		attribute.Int("agent.task.step_count", task.StepCount),
		attribute.Int("agent.task.max_steps", task.MaxSteps),
		attribute.Int("agent.task.tool_budget", task.ToolBudget),
		attribute.String("agent.orchestrator", "eino"),
	)

	// Get (or lazily compile) the cached Eino runner.
	runner, err := e.getEinoRunner(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "compile eino chain failed")
		olog.Error("failed to compile eino chain", "task_id", task.ID, "error", err)
		return err
	}

	olog.Info("invoking eino step chain", "task_id", task.ID)
	output, err := runner.Invoke(ctx, &einoStepState{Task: task})
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "eino chain failed")
		olog.Error("eino chain invocation failed", "task_id", task.ID, "error", err)
		return err
	}

	span.SetAttributes(
		attribute.String("agent.task.status_after", string(output.Status)),
		attribute.Int("agent.task.step_count_after", output.StepCount),
		attribute.Int("agent.task.tool_budget_after", output.ToolBudget),
	)

	return nil
}

// getEinoRunner returns the compiled Eino chain runner, compiling it lazily on
// the first successful call and caching it for all subsequent calls.
//
// Concurrency model — sync.RWMutex with double-check locking:
//
//  1. Hot path (runner already compiled, the common case):
//     Acquires only an RLock, so any number of concurrent requests can read
//     the cached runner simultaneously without blocking each other.
//
//  2. Cold path (first call or retry after a previous failure):
//     Releases the RLock, acquires a full Lock, then re-checks einoReady
//     (double-check) before compiling. This prevents two goroutines that both
//     saw einoReady==false from compiling the chain twice.
//
// Unlike sync.Once, a compilation failure does NOT permanently poison the
// cache: einoReady stays false so subsequent requests will retry, which is
// useful when a transient error (e.g. a missing env var or network blip) is
// corrected at runtime without restarting the server.
func (e *Engine) getEinoRunner(ctx context.Context) (compose.Runnable[*einoStepState, *types.Task], error) {
	// ── Hot path: read lock allows full concurrency when runner is ready ──────
	e.einoMu.RLock()
	if e.einoReady {
		runner := e.einoRunner.(compose.Runnable[*einoStepState, *types.Task])
		e.einoMu.RUnlock()
		return runner, nil
	}
	e.einoMu.RUnlock()

	// ── Cold path: upgrade to write lock for compilation ──────────────────────
	e.einoMu.Lock()
	defer e.einoMu.Unlock()

	// Double-check: another goroutine may have compiled while we waited for Lock.
	if e.einoReady {
		return e.einoRunner.(compose.Runnable[*einoStepState, *types.Task]), nil
	}

	// Still not ready — compile now. On failure we return the error but leave
	// einoReady=false so the next request can retry.
	olog.Info("compiling eino step chain (first use or retry after failure)")
	r, err := e.compileEinoStepChain(ctx)
	if err != nil {
		return nil, err
	}
	e.einoRunner = r
	e.einoReady = true
	olog.Info("eino step chain compiled and cached successfully")
	return r, nil
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

	// Token budget gate: sum TokenUsage across the trace and stop if the cap is
	// hit. TokenBudget <= 0 means "unlimited" (matches the field's zero value
	// and existing behavior for clients that don't set it). Mirrors the legacy
	// orchestrator's gate at engine.go runLegacyNext.
	if task.TokenBudget > 0 {
		totalTokens := 0
		for _, tr := range task.Trace {
			totalTokens += tr.TokenUsage.TotalTokens
		}
		if totalTokens >= task.TokenBudget {
			olog.Info("task reached token budget", "task_id", task.ID, "tokens", totalTokens, "token_budget", task.TokenBudget)
			finalAnswer := task.FinalAnswer
			if finalAnswer == "" {
				finalAnswer = "stopped by token budget"
			}
			_ = SetTaskCompleted(task, finalAnswer)
			if e.Metrics != nil {
				e.Metrics.IncCompleted()
			}
			return state, nil
		}
	}

	if task.StepCount < task.MaxSteps && task.ToolBudget > 0 {
		return state, nil
	}

	olog.Info("task reached step or budget limit", "task_id", task.ID, "step", task.StepCount, "max_steps", task.MaxSteps, "budget", task.ToolBudget)
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
	decision, err := e.Planner.PlanNext(ctx, state.Task, func(chunk string) {
		if e.TokenCallback != nil {
			e.TokenCallback(state.Task.ID, chunk)
		}
	})
	if e.Metrics != nil {
		e.Metrics.ObservePlanner(time.Since(pStart), err)
	}
	if err != nil {
		olog.Error("planner failed", "task_id", task.ID, "error", err)
		return state, err
	}

	var actionNames []string
	for _, ac := range decision.Actions {
		actionNames = append(actionNames, ac.Action)
	}
	olog.Info("planner decided", "task_id", task.ID, "thought", decision.ThoughtSummary, "actions", actionNames)

	task.Hypothesis = decision.ThoughtSummary
	state.Decision = decision

	if decision.Stop {
		olog.Info("planner decided to stop", "task_id", task.ID, "final_answer", decision.FinalAnswer)
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

	var actionNames []string
	for _, ac := range decision.Actions {
		actionNames = append(actionNames, ac.Action)
	}

	olog.Info("executing actions", "task_id", task.ID, "actions", actionNames)
	xStart := time.Now()
	traces, err := e.Executor.Execute(ctx, task, decision)

	// Count per-action failures. Tool failures are recorded in the traces and
	// are non-fatal (the executor only returns err on context cancellation), so
	// we surface them to metrics but keep the task running.
	failed := countFailedTraces(traces)
	obsErr := err
	if obsErr == nil && failed > 0 {
		obsErr = fmt.Errorf("%d of %d actions failed", failed, len(traces))
	}
	if e.Metrics != nil {
		e.Metrics.ObserveExecutor(time.Since(xStart), obsErr, "batch")
	}
	if err != nil {
		olog.Error("executor aborted", "task_id", task.ID, "actions", actionNames, "error", err)
		return state, err
	}

	if failed > 0 {
		olog.Warn("some actions failed; recorded as observations, continuing", "task_id", task.ID, "failed", failed, "total", len(traces))
	} else {
		olog.Info("action execution success", "task_id", task.ID, "traces", len(traces))
	}

	task.StepCount += len(traces)
	task.ToolBudget -= len(traces)
	// Propagate the planner's TokenUsage onto each trace entry so the token
	// budget gate (checkBudget) can see cumulative usage on subsequent steps.
	// Legacy mode does the same at engine.go runLegacyNext; without this,
	// task.Trace entries always carry zero TokenUsage and the gate is dead.
	for i := range traces {
		traces[i].TokenUsage = decision.TokenUsage
	}
	task.Trace = append(task.Trace, traces...)
	_ = SetTaskRunning(task)

	olog.Info("step completed", "step", task.StepCount, "task_id", task.ID, "remaining_budget", task.ToolBudget)
	return state, nil
}

// countFailedTraces returns how many traces recorded a tool failure.
func countFailedTraces(traces []types.StepTrace) int {
	failed := 0
	for i := range traces {
		if traces[i].Error != "" {
			failed++
		}
	}
	return failed
}
