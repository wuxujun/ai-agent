package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/wuxujun/ai-agent/internal/brain"
	"github.com/wuxujun/ai-agent/internal/config"
	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/logger"
	"github.com/wuxujun/ai-agent/internal/memory"
	"github.com/wuxujun/ai-agent/internal/sanitize"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/tools"
	"github.com/wuxujun/ai-agent/internal/types"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

type Mode string

const (
	ModeEino       Mode = "eino"
	ModeLegacy     Mode = "legacy"
	ModeAdk        Mode = "adk"
	ModeStep       Mode = "step"
	ModeMultiAgent Mode = "multiagent"
)

func IsSupportedMode(mode Mode) bool {
	switch mode {
	case ModeEino, ModeLegacy, ModeAdk, ModeStep, ModeMultiAgent:
		return true
	default:
		return false
	}
}

func (e *Engine) effectiveMode(task *types.Task) Mode {
	mode := e.Mode
	if task != nil && strings.TrimSpace(task.Mode) != "" {
		mode = Mode(strings.ToLower(strings.TrimSpace(task.Mode)))
	}
	if mode == "" {
		mode = ModeEino
	}
	return mode
}

func (e *Engine) Next(ctx context.Context, task *types.Task) (err error) {
	if err := executionLeaseError(ctx); err != nil {
		return err
	}
	ctx = logger.WithTaskID(ctx, task.ID)
	effectiveMode := e.effectiveMode(task)
	resumingMultiAgent := effectiveMode == ModeMultiAgent && e.CanResumeTask(task)
	engineLog.Info("running next execution step", "task_id", task.ID, "session_id", task.SessionID, "mode", string(effectiveMode))
	defer func() {
		if types.IsTerminalTaskStatus(task.Status) {
			tools.ClearRetrievalContext(task.ID)
			tools.ReleaseWikiTaskCache(task.ID, task.TenantID)
		}
	}()
	wasCompleted := task.Status == types.StatusCompleted
	if types.IsTerminalTaskStatus(task.Status) && !resumingMultiAgent {
		return nil
	}
	ctx = store.WithTenantScope(ctx, task.TenantID)
	if e.BrainPinner != nil {
		oldProject, oldSnapshot, oldDigest := task.BrainProjectID, task.BrainSnapshotID, task.BrainConfigDigest
		brainContext, changed, pinErr := e.BrainPinner.Pin(ctx, task)
		if pinErr != nil {
			return pinErr
		}
		if changed && e.Store != nil {
			if saveErr := e.Store.SaveFullTask(ctx, task); saveErr != nil {
				task.BrainProjectID, task.BrainSnapshotID, task.BrainConfigDigest = oldProject, oldSnapshot, oldDigest
				return saveErr
			}
		}
		ctx = brain.WithTaskContext(ctx, brainContext)
		ctx = tools.WithRetrievalExecutionContext(ctx, task.ID, task.TenantID, tools.WithBrainScope(brainContext.Ref.ProjectID, brainContext.SnapshotID))
	}
	if task.SessionID != "" {
		ctx = store.WithSessionScope(ctx, task.SessionID)
	}
	if e.Store != nil {
		if ledger, ok := e.Store.(types.TenantUsageLedger); ok {
			ctx = llmcore.WithTenantUsageLedger(ctx, ledger)
		}
	}
	ctx = llmcore.WithTaskBudget(ctx, task)
	if pipelineCfg := config.Get().AnswerPipeline; e.AnswerPipeline != nil && pipelineCfg.Enabled {
		ctx = llmcore.WithAnswerAuditReserve(ctx, pipelineCfg.AuditTokenReserve)
	}
	ctx = llmcore.WithTaskRoutingHints(ctx, task)
	defer func() {
		if e.AnswerPipeline == nil || strings.TrimSpace(task.FinalAnswer) == "" || !types.IsTerminalTaskStatus(task.Status) {
			return
		}
		if task.Status == types.StatusCompleted {
			e.runCodeQualityGates(ctx, task)
		}
		if _, pipelineErr := e.AnswerPipeline.Process(ctx, task, string(effectiveMode)); pipelineErr != nil {
			engineLog.Warn("answer pipeline failed; execution outcome preserved", "task_id", task.ID, "error", pipelineErr)
		}
	}()
	defer func() {
		if err != nil {
			var budgetErr *llmcore.TaskBudgetError
			if errors.As(err, &budgetErr) {
				engineLog.Info("task reached LLM budget", "task_id", task.ID, "kind", budgetErr.Kind, "current", budgetErr.Current, "limit", budgetErr.Limit)
				limitReason := limitReasonForTaskBudgetError(budgetErr)
				_ = SetTaskPartial(task, finalAnswerForLimit(task, limitReason), limitReason)
				if e.Metrics != nil {
					e.Metrics.IncCompleted()
				}
				err = nil
				return
			}
			if e.preserveDurableApprovalOnCancellation(task, err) {
				engineLog.Info("preserving awaiting approval during cancellation", "task_id", task.ID)
				return
			}
			safeFailure := sanitize.Secrets(err.Error())
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				engineLog.Info("task execution canceled", "task_id", task.ID, "error", safeFailure)
				code, message := cancellationResult(ctx)
				_ = SetTaskCanceled(task, code, message)
				return
			}
			engineLog.Error("step execution failed", "task_id", task.ID, "error", safeFailure)
			_ = SetTaskFailed(task, safeFailure)
			if ctx.Err() == nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				e.diagnoseFailure(ctx, task, errors.New(safeFailure))
			}
		}
	}()
	if !wasCompleted && !resumingMultiAgent && !e.guardInput(ctx, task) {
		return nil
	}
	if !wasCompleted && !resumingMultiAgent {
		e.routeIntent(ctx, task)
		ctx = llmcore.WithTaskRoutingHints(ctx, task)
	}

	prefetchContext := strings.EqualFold(strings.TrimSpace(config.Get().RAG.ContextMode), "prefetch")
	if !resumingMultiAgent && task.StepCount == 0 && len(task.Memories) == 0 && task.SessionID != "" {
		task.Memories = e.recentSessionMemories(ctx, task, config.Get().RAG.SessionRecentTaskLimit)
	}
	if !resumingMultiAgent && prefetchContext && task.StepCount == 0 {
		retrievedMems := append([]types.Memory(nil), task.Memories...)
		retrievalQuery := task.Goal
		var ragUsage types.TokenUsage
		if llmcore.AllowedForTask(config.LLMSceneRAGQueryRewriter, task) {
			retrievalQuery, ragUsage = memory.RewriteRAGQuery(ctx, task.Goal)
		}

		// 1. Try querying third-party RAG search URL if configured (query up to 5 candidates)
		if config.Get().RAG.SearchURL != "" {
			if extMems, extErr := memory.SearchThirdPartyRAG(ctx, retrievalQuery); extErr == nil && len(extMems) > 0 {
				extMems = e.inspectExternalMemories(ctx, task, extMems)
				retrievedMems = append(retrievedMems, extMems...)
				engineLog.Info("retrieved memories from third-party RAG URL", "task_id", task.ID, "count", len(extMems))
			} else if extErr != nil {
				engineLog.Warn("failed to query third-party RAG URL", "task_id", task.ID, "error", extErr)
			}
		}

		// 2. Query local Store for up to 5 candidates
		if e.Store != nil {
			engineLog.Info("querying local long-term memory", "task_id", task.ID, "goal", task.Goal)
			if emb, embErr := memory.GetEmbedding(ctx, retrievalQuery); embErr == nil {
				if mems, queryErr := e.Store.QueryMemories(ctx, retrievalQuery, emb, 5); queryErr == nil && len(mems) > 0 {
					for _, m := range mems {
						retrievedMems = append(retrievedMems, *m)
					}
					engineLog.Info("retrieved relevant local historical memories", "task_id", task.ID, "count", len(mems))
				}
			}
		}

		// 3. Deduplicate and limit to top 3 unique memories
		if len(retrievedMems) > 0 {
			deduped := memory.DeduplicateMemories(retrievedMems)
			var rerankUsage types.TokenUsage
			if llmcore.AllowedForTask(config.LLMSceneRAGReranker, task) {
				deduped, rerankUsage = memory.RerankMemories(ctx, retrievalQuery, deduped)
			}
			ragUsage.PromptTokens += rerankUsage.PromptTokens
			ragUsage.CompletionTokens += rerankUsage.CompletionTokens
			ragUsage.TotalTokens += rerankUsage.TotalTokens
			if len(deduped) > 3 {
				deduped = deduped[:3]
			}
			task.Memories = deduped
			engineLog.Info("final RAG memories after deduplication", "task_id", task.ID, "count", len(task.Memories))
		}
		if ragUsage.TotalTokens > 0 {
			task.Trace = append(task.Trace, types.StepTrace{Step: task.StepCount, Action: "rag_prepare", Observation: "LLM-assisted query preparation and memory ranking", TokenUsage: ragUsage})
			if e.Metrics != nil {
				e.Metrics.ObserveTokens(ragUsage.PromptTokens, ragUsage.CompletionTokens, ragUsage.TotalTokens, "rag")
			}
		}
	}
	if !wasCompleted && !resumingMultiAgent && prefetchContext {
		e.ResolveMemoryConflicts(ctx, task)
		ctx = llmcore.WithTaskRoutingHints(ctx, task)
	}

	switch effectiveMode {
	case "", ModeEino:
		err = e.runEinoNext(ctx, task)
	case ModeLegacy:
		err = e.runLegacyNext(ctx, task)
	case ModeAdk:
		err = e.runAdkNext(ctx, task)
	case ModeStep:
		err = e.runStepNext(ctx, task)
	case ModeMultiAgent:
		err = e.runMultiAgentNext(ctx, task)
	default:
		err = fmt.Errorf("unsupported orchestrator mode: %s", effectiveMode)
	}
	if e.AnswerPipeline == nil && err == nil && !wasCompleted && task.Status == types.StatusCompleted && strings.TrimSpace(task.FinalAnswer) != "" {
		e.verifyCitations(ctx, task)
		e.runCodeQualityGates(ctx, task)
		e.checkFactFreshness(ctx, task)
		e.checkNumericConsistency(ctx, task)
		e.calibrateAnswerUncertainty(ctx, task)
		e.guardOutput(ctx, task)
	}
	return err
}

// recentSessionMemories provides read-after-completion semantics even while
// asynchronous embedding/indexing for a previous task is still in flight.
func (e *Engine) recentSessionMemories(ctx context.Context, task *types.Task, limit int) []types.Memory {
	if e.Store == nil || task == nil || task.SessionID == "" || limit <= 0 {
		return nil
	}
	tasks, err := e.Store.ListTasks(ctx, store.ListFilter{TenantID: task.TenantID, SessionID: task.SessionID, Status: types.StatusCompleted, Limit: 500})
	if err != nil {
		engineLog.Warn("failed to load recent session tasks", "task_id", task.ID, "session_id", task.SessionID, "error", err)
		return nil
	}
	sort.Slice(tasks, func(i, j int) bool {
		if tasks[i].SequenceNo == tasks[j].SequenceNo {
			return tasks[i].UpdatedAt.After(tasks[j].UpdatedAt)
		}
		return tasks[i].SequenceNo > tasks[j].SequenceNo
	})
	items := make([]types.Memory, 0, limit)
	for _, previous := range tasks {
		if previous.ID == task.ID || strings.TrimSpace(previous.FinalAnswer) == "" {
			continue
		}
		items = append(items, types.Memory{ID: "session-task-" + previous.ID, TenantID: previous.TenantID, SessionID: previous.SessionID, TaskID: previous.ID, Goal: previous.Goal, FinalAnswer: previous.FinalAnswer, KeyFindings: memory.SummarizeTask(previous), Timestamp: previous.UpdatedAt})
		if len(items) >= limit {
			break
		}
	}
	return items
}

func (e *Engine) runLegacyNext(ctx context.Context, task *types.Task) error {
	ctx, span := tracer.Start(ctx, "engine.next")
	defer span.End()

	engineLog.Info("running step", "step", task.StepCount+1, "max_steps", task.MaxSteps, "budget", task.ToolBudget, "task_id", task.ID)

	span.SetAttributes(
		attribute.String("agent.task.id", task.ID),
		attribute.String("agent.task.status", string(task.Status)),
		attribute.Int("agent.task.step_count", task.StepCount),
		attribute.Int("agent.task.max_steps", task.MaxSteps),
		attribute.Int("agent.task.tool_budget", task.ToolBudget),
		attribute.String("agent.orchestrator", "legacy"),
	)

	totalTokens := 0
	for _, tr := range task.Trace {
		totalTokens += tr.TokenUsage.TotalTokens
	}

	if task.StepCount >= task.MaxSteps || task.ToolBudget <= 0 || (task.TokenBudget > 0 && totalTokens >= task.TokenBudget) {
		engineLog.Info("task reached limit", "task_id", task.ID, "step", task.StepCount, "max_steps", task.MaxSteps, "budget", task.ToolBudget, "tokens", totalTokens, "token_budget", task.TokenBudget)
		if task.TokenBudget > 0 && totalTokens >= task.TokenBudget {
			_ = SetTaskPartial(task, finalAnswerForLimit(task, limitReasonTokenBudget), limitReasonTokenBudget)
		} else {
			e.completeAtExecutionLimit(ctx, task)
		}
		if e.Metrics != nil {
			e.Metrics.IncCompleted()
		}
		span.SetAttributes(attribute.String("agent.task.final_reason", "budget_or_max_steps"))
		return nil
	}

	pStart := time.Now()
	answerStream := newFinalAnswerStream(func(chunk string) {
		if e.TokenCallback != nil {
			e.TokenCallback(task.ID, chunk)
		}
	})
	decision, err := e.Planner.PlanNext(ctx, task, func(chunk string) {
		answerStream.Write(chunk)
	})
	if e.Metrics != nil {
		e.Metrics.ObservePlanner(time.Since(pStart), err)
	}
	if err != nil {
		engineLog.Error("planner failed", "task_id", task.ID, "error", err)
		span.RecordError(err)
		span.SetStatus(codes.Error, "planner failure")
		return err
	}
	if e.finalizeBeforeRetrievalExpansion(ctx, task, decision) {
		return nil
	}
	e.critiqueDecision(ctx, task, decision)

	if e.Metrics != nil {
		e.Metrics.ObserveTokens(
			decision.TokenUsage.PromptTokens,
			decision.TokenUsage.CompletionTokens,
			decision.TokenUsage.TotalTokens,
			"planner",
		)
	}

	var actionNames []string
	for _, ac := range decision.Actions {
		actionNames = append(actionNames, ac.Action)
	}

	engineLog.Info("planner decided", "task_id", task.ID, "thought", decision.ThoughtSummary, "actions", actionNames)

	task.Hypothesis = decision.ThoughtSummary
	span.SetAttributes(
		attribute.StringSlice("agent.planner.actions", actionNames),
		attribute.Bool("agent.planner.stop", decision.Stop),
	)

	if decision.Stop {
		engineLog.Info("planner decided to stop", "task_id", task.ID, "final_answer", decision.FinalAnswer)
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
		span.SetAttributes(attribute.String("agent.task.final_reason", "planner_stop"))
		return nil
	}

	rejected, err := e.enforceApprovals(ctx, task, decision)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "approval failed")
		return err
	}

	if rejected {
		// Action rejected by user. The rejection trace has already been appended
		// in SuspendForApproval. We skip executing the tools and return nil to
		// allow the planner to adapt in the next cycle.
		return nil
	}

	engineLog.Info("executing actions", "task_id", task.ID, "actions", actionNames)
	xStart := time.Now()
	if err := executionLeaseError(ctx); err != nil {
		return err
	}
	traces, err := e.Executor.Execute(ctx, task, decision)
	traces, injectionAudit := e.inspectExternalTraces(ctx, task, traces)
	traces, relevanceAudits := e.filterExternalTraces(ctx, task, traces)
	conflictAudits := e.resolveEvidenceConflicts(ctx, task, traces)

	// Tool failures are recorded in the traces and are non-fatal (the executor
	// only returns err on context cancellation); surface them to metrics but
	// keep the task running so the planner can observe and recover next turn.
	failed := countFailedTraces(traces)
	obsErr := err
	if obsErr == nil && failed > 0 {
		obsErr = fmt.Errorf("%d of %d actions failed", failed, len(traces))
	}
	if e.Metrics != nil {
		e.Metrics.ObserveExecutor(time.Since(xStart), obsErr, "batch")
	}
	if err != nil {
		engineLog.Error("executor aborted", "task_id", task.ID, "actions", actionNames, "error", err)
		span.RecordError(err)
		span.SetStatus(codes.Error, "executor aborted")
		return err
	}

	if failed > 0 {
		engineLog.Warn("some actions failed; recorded as observations, continuing", "task_id", task.ID, "failed", failed, "total", len(traces))
		span.SetAttributes(attribute.Int("agent.executor.failed_actions", failed))
	} else {
		engineLog.Info("action execution success", "task_id", task.ID, "traces", len(traces))
	}

	task.StepCount += len(traces)
	task.ToolBudget -= len(traces)
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

	engineLog.Info("step completed", "step", task.StepCount, "task_id", task.ID, "remaining_budget", task.ToolBudget)

	span.SetAttributes(
		attribute.Int("agent.task.step_count_after", task.StepCount),
		attribute.Int("agent.task.tool_budget_after", task.ToolBudget),
	)

	return nil
}

func (e *Engine) RunAll(ctx context.Context, task *types.Task) error {
	ctx = logger.WithTaskID(ctx, task.ID)
	ctx, span := tracer.Start(ctx, "engine.run_all")
	defer span.End()

	engineLog.Info("starting task to completion", "task_id", task.ID, "goal", task.Goal)

	span.SetAttributes(
		attribute.String("agent.task.id", task.ID),
		attribute.Int("agent.task.goal_chars", len([]rune(task.Goal))),
	)

	if e.Metrics != nil {
		e.Metrics.IncRunAll()
	}

	resumeTerminal := e.CanResumeTask(task)
	for !types.IsTerminalTaskStatus(task.Status) || resumeTerminal {
		resumeTerminal = false
		if err := executionLeaseError(ctx); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			engineLog.Warn("task canceled", "task_id", task.ID, "error", ctx.Err())
			span.RecordError(ctx.Err())
			span.SetStatus(codes.Error, "context canceled")
			if !e.preserveDurableApprovalOnCancellation(task, ctx.Err()) {
				code, message := cancellationResult(ctx)
				_ = SetTaskCanceled(task, code, message)
			}
			return ctx.Err()
		default:
		}

		traceStart := len(task.Trace)
		if err := e.Next(ctx, task); err != nil {
			engineLog.Error("execution step failed", "task_id", task.ID, "error", err)
			span.RecordError(err)
			span.SetStatus(codes.Error, "run_all failed")
			return err
		}
		if ctx.Err() != nil {
			engineLog.Warn("task canceled after execution step", "task_id", task.ID, "error", ctx.Err())
			if !e.preserveDurableApprovalOnCancellation(task, ctx.Err()) {
				code, message := cancellationResult(ctx)
				_ = SetTaskCanceled(task, code, message)
			}
			return ctx.Err()
		}

		if err := executionLeaseError(ctx); err != nil {
			return err
		}
		if e.Store != nil {
			if err := e.Store.SaveFullTask(ctx, task); err != nil {
				engineLog.Error("failed to persist run-all progress", "task_id", task.ID, "error", err)
				span.RecordError(err)
				span.SetStatus(codes.Error, "persist progress failed")
				return err
			}
		}

		if e.StepCallback != nil {
			for i := traceStart; i < len(task.Trace); i++ {
				step := task.Trace[i]
				// A step callback reports execution progress. Task completion is
				// published separately with the authoritative final answer after
				// persistence; marking the last trace terminal can make SSE clients
				// close before they receive that final event.
				e.StepCallback(task.ID, types.StatusRunning, &step)
			}
		}
	}

	engineLog.Info("task finished", "task_id", task.ID, "status", string(task.Status), "final_answer", task.FinalAnswer)
	span.SetAttributes(attribute.Int("agent.task.final_answer_chars", len([]rune(task.FinalAnswer))))
	return nil
}

func cancellationResult(ctx context.Context) (string, string) {
	if errors.Is(executionCancellationCause(ctx), ErrClientDisconnected) {
		return "client_disconnected", "Task was canceled because the streaming client disconnected."
	}
	if errors.Is(executionCancellationCause(ctx), ErrTaskCanceledViaAPI) {
		return "task_canceled", "Task was canceled via API."
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "execution_timeout", "Task execution timed out."
	}
	return "task_canceled", "Task was canceled."
}
