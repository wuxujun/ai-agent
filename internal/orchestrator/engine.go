package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/wuxujun/ai-agent/internal/config"

	"github.com/wuxujun/ai-agent/internal/executor"
	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/logger"
	"github.com/wuxujun/ai-agent/internal/memory"
	"github.com/wuxujun/ai-agent/internal/metrics"
	"github.com/wuxujun/ai-agent/internal/multiagent"
	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/tools"
	"github.com/wuxujun/ai-agent/internal/types"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"google.golang.org/adk/model"
)

type Engine struct {
	Planner         planner.Planner
	Finalizer       planner.TaskFinalizer
	Executor        executor.Executor
	Metrics         *metrics.Collector
	Mode            Mode
	AdkModel        model.LLM
	LLMSceneEnabled func(string) bool
	// Coordinator is required when Mode == ModeMultiAgent.
	Coordinator *multiagent.Coordinator
	// Store handles database persistence and long-term memory.
	Store store.Store

	// Approvals is the in-memory approval state store for this engine instance.
	// When nil, the engine falls back to defaultApprovals (the process-wide
	// singleton). Tests that require isolation should set this to a fresh
	// NewApprovalStore() before use.
	//
	// P1-1: replacing the implicit global-state coupling with an explicit field
	// so SuspendForApproval no longer depends on package-level variables.
	Approvals *ApprovalStore

	// ApprovalBus enables cross-instance approval and cancel signalling via
	// Redis Pub/Sub. When nil the engine operates with in-process channels only
	// (single-instance mode). When set, remote approve/reject signals published
	// by any peer instance are forwarded into the local approval channel by the
	// bus's background loop, so SuspendForApproval's select sees them without
	// any extra logic.
	ApprovalBus *ApprovalBus

	// einoRunner is compiled once and cached for the lifetime of the Engine.
	// A sync.RWMutex guards lazy initialisation; unlike sync.Once, a failed
	// compilation attempt can be retried on the next call.
	//
	// Hot path (runner already compiled): acquired as RLock so concurrent
	// requests do not block each other at all.
	// Cold path (first compile or retry): upgraded to full Lock with a
	// double-check to prevent redundant compilations.
	einoMu     sync.RWMutex
	einoRunner any  // compose.Runnable[*einoStepState, *types.Task] after successful compile
	einoReady  bool // true once einoRunner has been successfully compiled

	EventCallback    func(taskID string, status types.TaskStatus)
	ApprovalCallback func(taskID string, approval *types.ApprovalRequest)
	StepCallback     func(taskID string, status types.TaskStatus, step *types.StepTrace)
	TokenCallback    func(taskID string, token string)

	// adkRunner is compiled once and cached for reuse across all runAdkNext calls.
	adkOnce   sync.Once
	adkRunner any // *runner.Runner
	adkErr    error
}

func (e *Engine) llmSceneEnabled(scene string) bool {
	if e.LLMSceneEnabled != nil {
		return e.LLMSceneEnabled(scene)
	}
	_, enabled := config.Get().LLM.Scenes[scene]
	return enabled
}

func (e *Engine) finalizeAnswer(ctx context.Context, task *types.Task, fallback string) (string, types.TokenUsage) {
	if e.Finalizer == nil {
		return fallback, types.TokenUsage{}
	}
	if !e.llmSceneEnabled(config.LLMSceneTaskFinalizer) {
		return fallback, types.TokenUsage{}
	}
	if !llmcore.AllowedForTask(config.LLMSceneTaskFinalizer, task) {
		return fallback, types.TokenUsage{}
	}
	answer, usage, err := e.Finalizer.Finalize(ctx, task)
	if err != nil {
		engineLog.Warn("task finalizer failed; using planner answer", "task_id", task.ID, "error", err)
		return fallback, types.TokenUsage{}
	}
	if e.Metrics != nil {
		e.Metrics.ObserveTokens(usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens, "finalizer")
	}
	return answer, usage
}

var tracer = otel.Tracer("ai-agent/orchestrator")

// engineLog is the package-level logger for engine.go; the package-level "log"
// var is declared in eino.go and shared across the package.
var engineLog = logger.Component("orchestrator")

type Mode string

const (
	ModeEino       Mode = "eino"
	ModeLegacy     Mode = "legacy"
	ModeAdk        Mode = "adk"
	ModeStep       Mode = "step"
	ModeMultiAgent Mode = "multiagent"
)

func (e *Engine) Next(ctx context.Context, task *types.Task) (err error) {
	engineLog.Info("running next execution step", "task_id", task.ID, "mode", string(e.Mode))
	ctx = store.WithTenantScope(ctx, task.TenantID)
	if e.Store != nil {
		if ledger, ok := e.Store.(types.TenantUsageLedger); ok {
			ctx = llmcore.WithTenantUsageLedger(ctx, ledger)
		}
	}
	ctx = llmcore.WithTaskBudget(ctx, task)
	ctx = llmcore.WithTaskRoutingHints(ctx, task)
	defer func() {
		if err != nil {
			var budgetErr *llmcore.TaskBudgetError
			if errors.As(err, &budgetErr) {
				engineLog.Info("task reached LLM budget", "task_id", task.ID, "kind", budgetErr.Kind, "current", budgetErr.Current, "limit", budgetErr.Limit)
				_ = SetTaskCompleted(task, finalAnswerForLimit(task, limitReasonLLMBudget))
				if e.Metrics != nil {
					e.Metrics.IncCompleted()
				}
				err = nil
				return
			}
			engineLog.Error("step execution failed", "task_id", task.ID, "error", err)
			_ = SetTaskFailed(task, err.Error())
		}
	}()

	if task.StepCount == 0 && len(task.Memories) == 0 {
		var retrievedMems []types.Memory
		retrievalQuery := task.Goal
		var ragUsage types.TokenUsage
		if llmcore.AllowedForTask(config.LLMSceneRAGQueryRewriter, task) {
			retrievalQuery, ragUsage = memory.RewriteRAGQuery(ctx, task.Goal)
		}

		// 1. Try querying third-party RAG search URL if configured (query up to 5 candidates)
		if config.Get().RAG.SearchURL != "" {
			if extMems, extErr := memory.SearchThirdPartyRAG(ctx, retrievalQuery); extErr == nil && len(extMems) > 0 {
				retrievedMems = append(retrievedMems, extMems...)
				engineLog.Info("retrieved memories from third-party RAG URL", "task_id", task.ID, "count", len(extMems))
			} else if extErr != nil {
				engineLog.Warn("failed to query third-party RAG URL", "error", extErr)
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

	switch e.Mode {
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
		err = fmt.Errorf("unsupported orchestrator mode: %s", e.Mode)
	}
	return err
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
		finalAnswer := task.FinalAnswer
		if finalAnswer == "" {
			reason := limitReasonStepOrToolBudget
			if task.TokenBudget > 0 && totalTokens >= task.TokenBudget {
				reason = limitReasonTokenBudget
			}
			finalAnswer = finalAnswerForLimit(task, reason)
		}
		_ = SetTaskCompleted(task, finalAnswer)
		if e.Metrics != nil {
			e.Metrics.IncCompleted()
		}
		span.SetAttributes(attribute.String("agent.task.final_reason", "budget_or_max_steps"))
		return nil
	}

	pStart := time.Now()
	decision, err := e.Planner.PlanNext(ctx, task, func(chunk string) {
		if e.TokenCallback != nil {
			e.TokenCallback(task.ID, chunk)
		}
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
		var finalizerUsage types.TokenUsage
		decision.FinalAnswer, finalizerUsage = e.finalizeAnswer(ctx, task, decision.FinalAnswer)
		decision.TokenUsage.PromptTokens += finalizerUsage.PromptTokens
		decision.TokenUsage.CompletionTokens += finalizerUsage.CompletionTokens
		decision.TokenUsage.TotalTokens += finalizerUsage.TotalTokens
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
	traces, err := e.Executor.Execute(ctx, task, decision)

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
			traces[i].TokenUsage = decision.TokenUsage
		}
	}
	task.Trace = append(task.Trace, traces...)
	_ = SetTaskRunning(task)

	engineLog.Info("step completed", "step", task.StepCount, "task_id", task.ID, "remaining_budget", task.ToolBudget)

	span.SetAttributes(
		attribute.Int("agent.task.step_count_after", task.StepCount),
		attribute.Int("agent.task.tool_budget_after", task.ToolBudget),
	)

	return nil
}

// enforceApprovals gates every RiskLevelHigh action in the decision behind the
// human approval flow before any tool runs. It returns rejected=true if the user
// rejected an action (the rejection trace is appended by SuspendForApproval, so
// the caller should skip execution and let the planner adapt next cycle); err is
// non-nil only on approval-flow failure (e.g. context cancellation).
//
// This is the single source of truth for high-risk gating across orchestrator
// modes. It MUST be called before Executor.Execute in every mode that runs an
// LLM-chosen PlanDecision — previously the gate lived only in runLegacyNext, so
// the default eino mode (and step/adk) executed write_file/execute_code with no
// approval at all (see BUG_REPORT.md #1).
func (e *Engine) enforceApprovals(ctx context.Context, task *types.Task, decision *planner.PlanDecision) (rejected bool, err error) {
	for i := range decision.Actions {
		ac := &decision.Actions[i]
		tool, ok := tools.Get(ac.Action)
		if !ok || tool.RiskLevel() != types.RiskLevelHigh {
			continue
		}
		approved, newParams, apErr := e.SuspendForApproval(ctx, task, ac.Action, ac.Parameters)
		if apErr != nil {
			engineLog.Error("action approval error", "task_id", task.ID, "action", ac.Action, "error", apErr)
			return false, apErr
		}
		if !approved {
			return true, nil
		}
		if newParams != nil {
			ac.Parameters = newParams
		}
	}
	return false, nil
}

// approvalStore returns the ApprovalStore this engine should use. When the
// Approvals field is set (e.g. in tests) that instance is used; otherwise the
// process-wide defaultApprovals singleton is returned.
func (e *Engine) approvalStore() *ApprovalStore {
	if e.Approvals != nil {
		return e.Approvals
	}
	return defaultApprovals
}

func (e *Engine) SuspendForApproval(ctx context.Context, task *types.Task, action string, params map[string]any) (bool, map[string]any, error) {
	approval := e.BuildApprovalRequest(task, action, params)
	store := e.approvalStore()
	approvalID, ch := store.Register(task.ID, approval)
	defer store.Remove(approvalID)

	task.Status = types.StatusAwaitingApproval
	if e.Store != nil {
		// Persistence must survive caller ctx expiry: the caller's ctx carries
		// the run-all wall-clock budget and the user's approval-wait window,
		// neither of which should be allowed to abort the awaiting_approval
		// write. Without this, a task could observe ctx.Done() below and leave
		// the DB row in a pre-suspend state, looking lost across restarts.
		saveCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := e.Store.SaveFullTask(saveCtx, task)
		cancel()
		if err != nil {
			return false, nil, err
		}
	}
	if e.EventCallback != nil {
		e.EventCallback(task.ID, types.StatusAwaitingApproval)
	}
	if e.ApprovalCallback != nil {
		e.ApprovalCallback(task.ID, approval)
	}

	var res types.ApprovalResult
	select {
	case res = <-ch:
	case <-ctx.Done():
		select {
		case res = <-ch:
		default:
			return false, nil, ctx.Err()
		}
	}

	if !res.Approved {
		msg := res.Message
		if msg == "" {
			msg = "No reason provided"
		}
		role := types.AgentRoleSingle
		if e.Mode == ModeMultiAgent {
			role = types.AgentRoleResearcher
		}
		task.Trace = append(task.Trace, types.StepTrace{
			Step:        task.StepCount + 1,
			Goal:        task.Goal,
			Action:      action,
			Observation: fmt.Sprintf("Action rejected by user. Reason: %s", msg),
			Error:       fmt.Sprintf("Action %s rejected by user: %s", action, msg),
			Evidence: []types.Evidence{{
				Path:  "user_feedback",
				Lines: []string{msg},
				Query: "disapproval",
			}},
			AgentRole: role,
		})
		task.StepCount += 1

		task.Status = types.StatusRunning
		if e.Store != nil {
			saveCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = e.Store.SaveFullTask(saveCtx, task)
			cancel()
		}
		if e.EventCallback != nil {
			e.EventCallback(task.ID, types.StatusRunning)
		}
		return false, nil, nil
	}

	task.Status = types.StatusRunning
	if e.Store != nil {
		saveCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = e.Store.SaveFullTask(saveCtx, task)
		cancel()
	}
	if e.EventCallback != nil {
		e.EventCallback(task.ID, types.StatusRunning)
	}
	return true, res.Parameters, nil
}

func (e *Engine) RunAll(ctx context.Context, task *types.Task) error {
	ctx, span := tracer.Start(ctx, "engine.run_all")
	defer span.End()

	engineLog.Info("starting task to completion", "task_id", task.ID, "goal", task.Goal)

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
			engineLog.Warn("task canceled", "task_id", task.ID, "error", ctx.Err())
			span.RecordError(ctx.Err())
			span.SetStatus(codes.Error, "context canceled")
			_ = SetTaskFailed(task, "task canceled: "+ctx.Err().Error())
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
				e.StepCallback(task.ID, task.Status, &step)
			}
		}
	}

	engineLog.Info("task finished", "task_id", task.ID, "status", string(task.Status), "final_answer", task.FinalAnswer)
	span.SetAttributes(attribute.String("agent.task.final_answer", task.FinalAnswer))
	return nil
}

func stepFindTextFiles(ctx context.Context, task *types.Task) error {
	engineLog.Info("legacy static path - finding text files", "task_id", task.ID)
	task.Hypothesis = "Relevant evidence is likely inside text or markdown files"

	txtFiles, err := tools.FindFiles(ctx, task.Workspace, "*.txt")
	if err != nil {
		engineLog.Error("legacy static path - FindFiles (*.txt) failed", "error", err)
		return err
	}
	mdFiles, err := tools.FindFiles(ctx, task.Workspace, "*.md")
	if err != nil {
		engineLog.Error("legacy static path - FindFiles (*.md) failed", "error", err)
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
		engineLog.Error("legacy static path - failed to extract keyword", "error", err)
		return err
	}
	engineLog.Info("legacy static path - searching keyword", "keyword", query, "task_id", task.ID)
	task.Hypothesis = "Search the most likely keyword in candidate text files"

	evidence, _, err := tools.SearchWithRG(ctx, task.Workspace, query, "*.txt")
	if err != nil {
		engineLog.Error("legacy static path - SearchWithRG failed", "error", err)
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
		_ = SetTaskCompleted(task, "not enough evidence to select a file")
		return nil
	}

	target := task.Trace[1].Evidence[0].Path
	engineLog.Info("legacy static path - target best file identified", "task_id", task.ID, "file", target)
	content, err := tools.ReadFile(task.Workspace, target)
	if err != nil {
		engineLog.Error("legacy static path - ReadFile failed", "error", err)
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
