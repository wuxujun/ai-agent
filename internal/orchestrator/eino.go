package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cloudwego/eino/compose"
	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/logger"
	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/tools"
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

	if task.StepCount >= task.MaxSteps || task.ToolBudget <= 0 {
		olog.Info("checking execution limits", "completed_steps", task.StepCount, "max_steps", task.MaxSteps, "remaining_budget", task.ToolBudget, "task_id", task.ID)
	} else {
		olog.Info("running step", "step", task.StepCount+1, "max_steps", task.MaxSteps, "budget", task.ToolBudget, "task_id", task.ID)
	}

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
				finalAnswer = finalAnswerForLimit(task, limitReasonTokenBudget)
			}
			_ = SetTaskPartial(task, finalAnswer, limitReasonTokenBudget)
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
	e.completeAtExecutionLimit(ctx, task)
	if e.Metrics != nil {
		e.Metrics.IncCompleted()
	}
	return state, nil
}

func (e *Engine) completeAtExecutionLimit(ctx context.Context, task *types.Task) {
	fallback := finalAnswerForLimit(task, limitReasonStepOrToolBudget)
	if planner.HasSupportingEvidence(task.Trace) {
		answer, usage := e.finalizeAnswer(ctx, task, fallback)
		if strings.TrimSpace(answer) != "" && answer != fallback {
			task.Trace = append(task.Trace, types.StepTrace{
				Step:        task.StepCount + 1,
				Goal:        task.Goal,
				Action:      "budget_finalize",
				Query:       limitReasonStepOrToolBudget,
				Observation: answer,
				TokenUsage:  usage,
			})
			olog.Info("synthesized final answer at execution budget boundary", "task_id", task.ID, "evidence_available", true)
			_ = SetTaskCompleted(task, answer)
			return
		}
	}
	_ = SetTaskPartial(task, fallback, limitReasonStepOrToolBudget)
}

func (e *Engine) planNext(ctx context.Context, state *einoStepState) (*einoStepState, error) {
	task := state.Task
	if types.IsTerminalTaskStatus(task.Status) {
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
	if e.finalizeBeforeRetrievalExpansion(ctx, task, decision) {
		state.Decision = nil
		return state, nil
	}
	e.critiqueDecision(ctx, task, decision)

	var actionNames []string
	for _, ac := range decision.Actions {
		actionNames = append(actionNames, ac.Action)
	}
	olog.Info("planner decided", "task_id", task.ID, "thought", decision.ThoughtSummary, "actions", actionNames)

	task.Hypothesis = decision.ThoughtSummary
	state.Decision = decision

	if decision.Stop {
		var finalizerUsage types.TokenUsage
		decision.FinalAnswer, finalizerUsage = e.finalizeAnswer(ctx, task, decision.FinalAnswer)
		decision.TokenUsage.PromptTokens += finalizerUsage.PromptTokens
		decision.TokenUsage.CompletionTokens += finalizerUsage.CompletionTokens
		decision.TokenUsage.TotalTokens += finalizerUsage.TotalTokens
		olog.Info("planner decided to stop", "task_id", task.ID, "final_answer", decision.FinalAnswer)
		task.Trace = append(task.Trace, types.StepTrace{
			Step:        task.StepCount + 1,
			Action:      "stop",
			Observation: decision.FinalAnswer,
			TokenUsage:  decision.TokenUsage,
		})
		_ = SetTaskCompleted(task, decision.FinalAnswer)
		if e.Metrics != nil {
			e.Metrics.IncCompleted()
		}
	}

	return state, nil
}

func (e *Engine) finalizeBeforeRetrievalExpansion(ctx context.Context, task *types.Task, decision *planner.PlanDecision) bool {
	if decision == nil || decision.Stop {
		return false
	}
	hasEvidence := planner.HasSupportingEvidence(task.Trace)
	hardCapacityGuard := hasEvidence && remainingExecutionCapacity(task) <= 2
	mustFinalize := false
	guardReason := ""
	for _, action := range decision.Actions {
		switch action.Action {
		case "rag_search", "memory_search":
			kind := strings.TrimSuffix(action.Action, "_search")
			query, _ := action.Parameters["query"].(string)
			state := tools.RetrievalStateForTask(task.ID)
			maxCycles := config.Get().RAG.JITRetrievalMaxCycles
			if maxCycles <= 0 {
				maxCycles = 2
			}
			if hardCapacityGuard {
				mustFinalize = true
				guardReason = "retrieval_capacity_reserved"
			} else if tools.RetrievalQueryKnown(task.ID, kind, query) {
				mustFinalize = true
				guardReason = "equivalent_query_already_retrieved"
			} else if state.RetrievalCycles[kind] >= maxCycles {
				mustFinalize = true
				guardReason = "retrieval_cycle_limit_reached"
			}
		case "rag_fetch", "memory_get":
			ids := retrievalActionIDs(action.Parameters["ids"])
			pending := tools.UnfetchedRetrievalIDs(task.ID, ids)
			if len(pending) < len(ids) {
				action.Parameters["ids"] = pending
			}
			if hardCapacityGuard {
				mustFinalize = true
				guardReason = "retrieval_capacity_reserved"
			} else if len(ids) > 0 && len(pending) == 0 {
				mustFinalize = true
				guardReason = "retrieval_candidates_already_fetched"
			}
		}
	}
	if !mustFinalize {
		return false
	}
	fallback := finalAnswerForLimit(task, limitReasonFinalizerUnavailable)
	answer, usage, finalizerReason := e.finalizeAnswerDetailed(ctx, task, fallback)
	usage.PromptTokens += decision.TokenUsage.PromptTokens
	usage.CompletionTokens += decision.TokenUsage.CompletionTokens
	usage.TotalTokens += decision.TokenUsage.TotalTokens
	if strings.TrimSpace(answer) == "" || answer == fallback {
		task.Trace = append(task.Trace, types.StepTrace{
			Step: task.StepCount + 1, Goal: task.Goal, Action: "retrieval_guard",
			Query: guardReason, Observation: "finalizer unavailable; retrieval action was not executed", TokenUsage: decision.TokenUsage,
		})
		olog.Warn("retrieval blocked but finalizer unavailable; marking task partial", "task_id", task.ID, "reason", guardReason, "finalizer_reason", finalizerReason, "remaining_action_capacity", remainingExecutionCapacity(task))
		_ = SetTaskPartial(task, fallback, limitReasonFinalizerUnavailable)
		if e.Metrics != nil {
			e.Metrics.IncCompleted()
		}
		return true
	}
	task.Trace = append(task.Trace, types.StepTrace{
		Step: task.StepCount + 1, Goal: task.Goal, Action: "budget_finalize",
		Query: guardReason, Observation: answer, TokenUsage: usage,
	})
	olog.Info("stopped retrieval expansion and synthesized final answer", "task_id", task.ID, "reason", guardReason, "remaining_action_capacity", remainingExecutionCapacity(task))
	_ = SetTaskCompleted(task, answer)
	if e.Metrics != nil {
		e.Metrics.IncCompleted()
	}
	return true
}

func retrievalActionIDs(value any) []string {
	switch ids := value.(type) {
	case []string:
		return append([]string(nil), ids...)
	case []any:
		result := make([]string, 0, len(ids))
		for _, item := range ids {
			if id, ok := item.(string); ok {
				result = append(result, id)
			}
		}
		return result
	default:
		return nil
	}
}

func remainingExecutionCapacity(task *types.Task) int {
	remaining := task.MaxSteps - task.StepCount
	if task.ToolBudget < remaining {
		remaining = task.ToolBudget
	}
	if remaining < 0 {
		return 0
	}
	return remaining
}

func (e *Engine) executeDecision(ctx context.Context, state *einoStepState) (*einoStepState, error) {
	task := state.Task
	decision := state.Decision
	if types.IsTerminalTaskStatus(task.Status) || decision == nil {
		return state, nil
	}

	var actionNames []string
	for _, ac := range decision.Actions {
		actionNames = append(actionNames, ac.Action)
	}

	// Gate high-risk actions behind human approval before any tool runs. This
	// must mirror runLegacyNext — without it the default eino mode executed
	// write_file/execute_code with no approval gate at all (BUG_REPORT.md #1).
	rejected, apErr := e.enforceApprovals(ctx, task, decision)
	if apErr != nil {
		return state, apErr
	}
	if rejected {
		// Action rejected by user. SuspendForApproval already appended the
		// rejection trace; skip execution so the planner can adapt next cycle.
		return state, nil
	}

	olog.Info("executing actions", "task_id", task.ID, "actions", actionNames)
	xStart := time.Now()
	traces, err := e.Executor.Execute(ctx, task, decision)
	traces, injectionAudit := e.inspectExternalTraces(ctx, task, traces)
	traces, relevanceAudits := e.filterExternalTraces(ctx, task, traces)
	conflictAudits := e.resolveEvidenceConflicts(ctx, task, traces)

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
		if i == 0 {
			traces[i].TokenUsage.PromptTokens += decision.TokenUsage.PromptTokens
			traces[i].TokenUsage.CompletionTokens += decision.TokenUsage.CompletionTokens
			traces[i].TokenUsage.TotalTokens += decision.TokenUsage.TotalTokens
		}
	}
	task.Trace = append(task.Trace, traces...)
	if injectionAudit != nil {
		task.Trace = append(task.Trace, *injectionAudit)
	}
	task.Trace = append(task.Trace, relevanceAudits...)
	task.Trace = append(task.Trace, conflictAudits...)
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
