package orchestrator

import (
	"context"
	"fmt"
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

	// Get (or lazily compile) the cached Eino runner.
	runner, err := e.getEinoRunner(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "compile eino chain failed")
		log.Printf("[Engine-Eino Error] Task %s - failed to compile Eino chain: %v", task.ID, err)
		return err
	}

	log.Printf("[Engine-Eino] Invoking Eino step chain for task %s", task.ID)
	output, err := runner.Invoke(ctx, &einoStepState{Task: task})
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "eino chain failed")
		log.Printf("[Engine-Eino Error] Task %s - Eino chain invocation failed: %v", task.ID, err)
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
	log.Printf("[Engine-Eino] Compiling Eino step chain (first use or retry after failure)")
	r, err := e.compileEinoStepChain(ctx)
	if err != nil {
		return nil, err
	}
	e.einoRunner = r
	e.einoReady = true
	log.Printf("[Engine-Eino] Eino step chain compiled and cached successfully")
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
	decision, err := e.Planner.PlanNext(ctx, state.Task, func(chunk string) {
		if e.TokenCallback != nil {
			e.TokenCallback(state.Task.ID, chunk)
		}
	})
	if e.Metrics != nil {
		e.Metrics.ObservePlanner(time.Since(pStart), err)
	}
	if err != nil {
		log.Printf("[Engine-Eino] Planner failed for task %s: %v", task.ID, err)
		return state, err
	}

	var actionNames []string
	for _, ac := range decision.Actions {
		actionNames = append(actionNames, ac.Action)
	}
	log.Printf("[Engine-Eino] Task %s - Planner thought: %q | Actions chosen: %v", task.ID, decision.ThoughtSummary, actionNames)

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

	var actionNames []string
	for _, ac := range decision.Actions {
		actionNames = append(actionNames, ac.Action)
	}

	log.Printf("[Engine-Eino] Task %s - Executing actions %v", task.ID, actionNames)
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
		log.Printf("[Engine-Eino] Executor aborted for task %s, actions %v: %v", task.ID, actionNames, err)
		return state, err
	}

	if failed > 0 {
		log.Printf("[Engine-Eino] Task %s - %d/%d actions failed; recorded as observations, continuing", task.ID, failed, len(traces))
	} else {
		log.Printf("[Engine-Eino] Task %s - Action execution success. %d traces produced", task.ID, len(traces))
	}

	task.StepCount += len(traces)
	task.ToolBudget -= len(traces)
	task.Trace = append(task.Trace, traces...)
	_ = SetTaskRunning(task)

	log.Printf("[Engine-Eino] Step %d completed for task %s. Remaining budget: %d", task.StepCount, task.ID, task.ToolBudget)
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
