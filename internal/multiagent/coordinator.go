package multiagent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/wuxujun/ai-agent/internal/evidenceconflict"
	"github.com/wuxujun/ai-agent/internal/evidencefilter"
	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/logger"
	"github.com/wuxujun/ai-agent/internal/metrics"
	"github.com/wuxujun/ai-agent/internal/plancritic"
	"github.com/wuxujun/ai-agent/internal/promptguard"
	"github.com/wuxujun/ai-agent/internal/promptmanager"
	"github.com/wuxujun/ai-agent/internal/sourcecredibility"
	"github.com/wuxujun/ai-agent/internal/tools"
	"github.com/wuxujun/ai-agent/internal/types"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

var tracer = otel.Tracer("ai-agent/multiagent")

var log = logger.Component("multiagent")

type Planner interface {
	Plan(ctx context.Context, goal, workspace string, memories []types.Memory) (*ResearchPlan, error)
	Replan(ctx context.Context, goal, workspace string, traces []types.StepTrace, memories []types.Memory) (*ResearchPlan, error)
}

type Researcher interface {
	Research(ctx context.Context, workspace string, step ResearchStep) (*StepEvidence, error)
}

type Executor interface {
	Execute(ctx context.Context, workspace string, step ResearchStep) (*StepEvidence, error)
}

type Writer interface {
	Write(ctx context.Context, goal string, evidence []StepEvidence, memories []types.Memory) (*WriterOutput, error)
}

type researchPhaseResult struct {
	Evidence []StepEvidence
	Complete bool
	Reason   string
	Workflow Workflow
	Err      error
}

// Coordinator supports both configured collaboration topologies:
//
//	PlannerAgent → ResearcherAgent (×N) → WriterAgent
//	PlannerAgent → CriticAgent → ExecutorAgent (×N) → VerifierAgent
//
// It updates task.Trace and task.Status in-place. Published-answer auditing and
// final confidence are owned by orchestrator.AnswerPipeline.
type Coordinator struct {
	Planner                  Planner
	Researcher               Researcher
	Executor                 Executor
	Writer                   Writer
	Verifier                 AnswerVerifier
	FinalVerifier            FinalVerifier
	Metrics                  *metrics.Collector
	SuspendForApproval       func(ctx context.Context, task *types.Task, action string, params map[string]any) (bool, map[string]any, error)
	ResolveMemoryConflicts   func(ctx context.Context, task *types.Task)
	PlanCritic               plancritic.Critic
	PromptInjectionDetector  promptguard.Detector
	EvidenceRelevanceFilter  evidencefilter.Filter
	EvidenceConflictResolver evidenceconflict.Resolver
	SourceCredibilityScorer  sourcecredibility.Scorer
	EventCallback            func(taskID string, status types.TaskStatus)
	TokenCallback            func(taskID string, token string)
	PersistTask              func(ctx context.Context, task *types.Task) error
}

func (c *Coordinator) persistTaskDetached(task *types.Task) error {
	if c.PersistTask == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(logger.WithTaskID(context.Background(), task.ID), 10*time.Second)
	defer cancel()
	return c.PersistTask(ctx, task)
}

// Run executes the full multi-agent workflow for the given task, updating
// task.Trace, task.StepCount, task.ToolBudget, and task.Status in-place.
//
// The selected team's workflow controls whether tool steps and final synthesis
// are owned by Researcher/Writer or Executor/Verifier. Both paths retain the
// same policy, approval, cancellation, and budget gates.
func (c *Coordinator) Run(ctx context.Context, task *types.Task) (runErr error) {
	ctx = logger.WithTaskID(ctx, task.ID)
	_, _, projectID, snapshotID, watermark, ok := tools.RetrievalContextScope(ctx)
	if ok && projectID != "" && snapshotID != "" {
		ctx = tools.WithRetrievalExecutionContext(ctx, task.ID, task.TenantID, tools.WithBrainScope(projectID, snapshotID), tools.WithBrainWatermark(watermark))
	} else {
		ctx = tools.WithRetrievalExecutionContext(ctx, task.ID, task.TenantID)
	}
	ctx = llmcore.WithTaskBudget(ctx, task)
	ctx = llmcore.WithTaskRoutingHints(ctx, task)
	ctx, span := tracer.Start(ctx, "multiagent.coordinator.run")
	defer span.End()

	span.SetAttributes(
		attribute.String("agent.task.id", task.ID),
		attribute.Int("agent.task.goal_chars", len([]rune(task.Goal))),
	)

	teamsCfg, err := teamsConfigForTask(task)
	if err != nil {
		task.Status = types.StatusFailed
		return err
	}
	teamCfg := teamsCfg.GetActiveTeam()
	teamSnapshot := newTeamConfigSnapshot(teamsCfg.ActiveTeam, teamCfg)
	teamSnapshot.ResumePolicy = teamsCfg.ResumeConfigPolicy
	ctx = withTeamConfigSnapshot(ctx, teamSnapshot)
	log := teamLogger(ctx)
	configuredWorkflow := teamsCfg.ActiveWorkflow()
	configuredRuntime := teamsCfg.ActiveRuntime()
	configuredGraphSummary, err := annotateWorkflowGraph(span, "multiagent.workflow.configured_graph", configuredWorkflow)
	if err != nil {
		task.Status = types.StatusFailed
		span.RecordError(err)
		span.SetStatus(codes.Error, "configured workflow graph is invalid")
		return err
	}
	planningWorkflow := configuredWorkflow
	if planningWorkflow == WorkflowAdaptive {
		planningWorkflow = WorkflowResearch
	}
	ctx = withWorkflow(ctx, planningWorkflow)
	log.Info("Starting multi-agent workflow", "task_id", task.ID, "goal", task.Goal, "active_team", teamsCfg.ActiveTeam, "team_config_digest", teamSnapshot.Digest, "configured_workflow", configuredWorkflow, "configured_runtime", configuredRuntime, "dag_canary_percent", teamCfg.DAGCanaryPercent, "configured_graph_digest", configuredGraphSummary.Digest)
	span.SetAttributes(
		attribute.String("multiagent.team", teamSnapshot.ActiveTeam),
		attribute.String("multiagent.team_config_digest", teamSnapshot.Digest),
		attribute.String("multiagent.resume_config_policy", string(teamSnapshot.ResumePolicy)),
		attribute.String("multiagent.runtime.configured", string(configuredRuntime)),
	)
	if err := enforceTeamLifecycleResumePolicy(task, teamSnapshot); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "Team is retired")
		log.Warn("Multi-agent resume blocked because Team is retired", "task_id", task.ID, "error", err)
		return nil
	}
	traceCountBeforeConfigCheck := len(task.Trace)
	if err := enforceTeamConfigResumePolicy(task, teamSnapshot); err != nil {
		if c.Metrics != nil {
			c.Metrics.ObserveMultiAgentConfigChange(string(teamSnapshot.ResumePolicy), "blocked")
		}
		span.SetAttributes(attribute.Bool("multiagent.team_config_changed", true))
		log.Warn("Multi-agent resume blocked by team configuration change", "task_id", task.ID, "error", err)
		return nil
	}
	if c.Metrics != nil && len(task.Trace) > traceCountBeforeConfigCheck && task.Trace[len(task.Trace)-1].Action == TeamConfigChangeTraceAction {
		c.Metrics.ObserveMultiAgentConfigChange(string(teamSnapshot.ResumePolicy), "migrated")
	}
	runtimeDecision := resolveTaskRuntime(task, teamSnapshot, configuredRuntime)
	runtimeMode := runtimeDecision.Runtime
	if forced, _ := ctx.Value(forceLegacyRuntimeContextKey{}).(bool); forced {
		runtimeMode = RuntimeLegacy
	}
	span.SetAttributes(
		attribute.String("multiagent.runtime.selected", string(runtimeMode)),
		attribute.String("multiagent.runtime.selection_source", runtimeDecision.Source),
		attribute.Int("multiagent.runtime.canary_bucket", runtimeDecision.Bucket),
		attribute.Int("multiagent.runtime.canary_percent", runtimeDecision.Percent),
	)
	runtimeStarted := time.Now()
	runtimeTraceStart := len(task.Trace)
	runtimeTraceEnd := -1
	defer func() {
		if c.Metrics == nil {
			return
		}
		outcome := "success"
		if runErr != nil {
			outcome = "failure"
			if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
				outcome = "canceled"
			}
		} else if task.Status != types.StatusCompleted {
			outcome = "partial"
		}
		c.Metrics.ObserveMultiAgentRuntime(string(runtimeMode), outcome, time.Since(runtimeStarted))
		c.Metrics.ObserveMultiAgentRuntimeEvent(string(runtimeMode), "observed")
		traceEnd := len(task.Trace)
		approvalRequired := task.Status == types.StatusAwaitingApproval
		if runtimeTraceEnd >= runtimeTraceStart && runtimeTraceEnd <= traceEnd {
			traceEnd = runtimeTraceEnd
			approvalRequired = false
		}
		if approvalRequired {
			c.Metrics.ObserveMultiAgentRuntimeEvent(string(runtimeMode), "approval_required")
		}
		if runtimeInvocationReplanned(task.Trace, runtimeTraceStart, traceEnd) {
			c.Metrics.ObserveMultiAgentRuntimeEvent(string(runtimeMode), "replanned")
		}
	}()
	persistedPins := persistedPromptVersionPins(task, teamSnapshot.Digest)
	var promptBindingMu sync.Mutex
	pinRegistry, err := promptmanager.NewVersionPinRegistry(persistedPins, func(pin promptmanager.VersionPin) {
		promptBindingMu.Lock()
		appendPromptVersionBinding(task, teamSnapshot.Digest, pin)
		promptBindingMu.Unlock()
	})
	if err != nil {
		task.Status = types.StatusFailed
		span.RecordError(err)
		span.SetStatus(codes.Error, "prompt version bindings are invalid")
		return fmt.Errorf("load prompt version bindings: %w", err)
	}
	ctx = promptmanager.WithVersionPinRegistry(ctx, pinRegistry)
	span.SetAttributes(attribute.Int("multiagent.prompt_version_pin_count", len(persistedPins)))
	if checkpoint, ok := pendingVerifierDraft(task); ok && task.Status != types.StatusFailed && task.Status != types.StatusCompleted {
		ctx = withWorkflow(ctx, WorkflowReviewed)
		resumeRuntime := RuntimeLegacy
		if runtimeMode == RuntimeDAG && (configuredWorkflow == WorkflowReviewed || configuredWorkflow == WorkflowAdaptive) {
			resumeRuntime = RuntimeDAG
		}
		if _, graphErr := annotateWorkflowGraph(span, "multiagent.workflow.effective_graph", WorkflowReviewed); graphErr != nil {
			task.Status = types.StatusFailed
			span.RecordError(graphErr)
			span.SetStatus(codes.Error, "resume workflow graph is invalid")
			return graphErr
		}
		log.Info("Resuming multi-agent task from verifier draft checkpoint", "task_id", task.ID)
		err := c.resumeVerifierCheckpoint(ctx, task, checkpoint)
		if err == nil && runtimeMode == RuntimeDAG && (configuredWorkflow == WorkflowReviewed || configuredWorkflow == WorkflowAdaptive) {
			if checkpointErr := c.completeReviewedDAGVerifierCheckpoint(ctx, task); checkpointErr != nil {
				err = checkpointErr
			}
		}
		span.SetAttributes(
			attribute.Bool("multiagent.verifier.resumed", true),
			attribute.String("multiagent.runtime.effective", string(resumeRuntime)),
			attribute.String("agent.task.final_status", string(task.Status)),
		)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "verifier checkpoint resume failed")
		}
		return err
	}
	if runtimeMode == RuntimeDAG && (configuredWorkflow == WorkflowResearch || configuredWorkflow == WorkflowReviewed) {
		effectiveWorkflow := configuredWorkflow
		route := workflowRouteDecision{Configured: configuredWorkflow, Effective: effectiveWorkflow, Reason: "configured"}
		if c.Metrics != nil {
			c.Metrics.ObserveMultiAgentRoute(string(route.Configured), string(route.Effective), route.Reason)
		}
		if _, graphErr := annotateWorkflowGraph(span, "multiagent.workflow.effective_graph", effectiveWorkflow); graphErr != nil {
			task.Status = types.StatusFailed
			span.RecordError(graphErr)
			span.SetStatus(codes.Error, "effective workflow graph is invalid")
			return graphErr
		}
		span.SetAttributes(
			attribute.String("multiagent.runtime.effective", string(RuntimeDAG)),
			attribute.String("multiagent.workflow.configured", string(configuredWorkflow)),
			attribute.String("multiagent.workflow.effective", string(effectiveWorkflow)),
			attribute.String("multiagent.workflow.route_reason", route.Reason),
		)
		log.Info("Executing fixed workflow with DAG runtime", "task_id", task.ID, "workflow", effectiveWorkflow)
		var err error
		if effectiveWorkflow == WorkflowReviewed {
			err = c.runReviewedWorkflowDAG(withWorkflow(ctx, effectiveWorkflow), task, teamCfg)
		} else {
			err = c.runResearchWorkflowDAG(withWorkflow(ctx, effectiveWorkflow), task, teamCfg)
		}
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "DAG runtime failed")
		}
		log.Info("Workflow complete", "task_id", task.ID, "status", task.Status, "runtime", RuntimeDAG)
		span.SetAttributes(
			attribute.String("agent.task.final_status", string(task.Status)),
			attribute.Int("agent.task.final_answer_chars", len([]rune(task.FinalAnswer))),
		)
		return err
	}
	if runtimeMode == RuntimeDAG && configuredWorkflow == WorkflowAdaptive {
		plan, planErr := c.runPlanPhase(ctx, task)
		if planErr != nil {
			span.RecordError(planErr)
			span.SetStatus(codes.Error, "adaptive DAG plan phase failed")
			return planErr
		}
		route := resolveWorkflow(configuredWorkflow, teamCfg.Routing, task, plan)
		if c.Metrics != nil {
			c.Metrics.ObserveMultiAgentRoute(string(route.Configured), string(route.Effective), route.Reason)
		}
		if configuredWorkflow == WorkflowAdaptive {
			recordWorkflowRoute(task, route)
		}
		if _, graphErr := annotateWorkflowGraph(span, "multiagent.workflow.effective_graph", route.Effective); graphErr != nil {
			task.Status = types.StatusFailed
			span.RecordError(graphErr)
			span.SetStatus(codes.Error, "adaptive effective workflow graph is invalid")
			return graphErr
		}
		span.SetAttributes(
			attribute.String("multiagent.runtime.effective", string(RuntimeDAG)),
			attribute.String("multiagent.workflow.configured", string(configuredWorkflow)),
			attribute.String("multiagent.workflow.effective", string(route.Effective)),
			attribute.String("multiagent.workflow.route_reason", route.Reason),
		)
		var dagErr error
		if route.Effective == WorkflowReviewed {
			dagErr = c.runReviewedWorkflowDAGFromPlan(withWorkflow(ctx, WorkflowReviewed), task, teamCfg, plan)
		} else {
			dagErr = c.runResearchWorkflowDAGFromPlan(withWorkflow(ctx, WorkflowResearch), task, teamCfg, plan, WorkflowAdaptive)
		}
		var escalationErr *adaptiveDAGEscalationError
		if dagErr != nil && errors.As(dagErr, &escalationErr) {
			span.SetAttributes(attribute.String("multiagent.runtime.fallback_reason", escalationErr.reason))
			if c.Metrics != nil {
				c.Metrics.ObserveMultiAgentRoute(string(WorkflowAdaptive), string(WorkflowReviewed), "dag_fallback:"+escalationErr.reason)
				c.Metrics.ObserveMultiAgentRuntimeFallback("dag_fallback:" + escalationErr.reason)
			}
			log.Warn("Adaptive DAG escalated during Research; continuing with Legacy runtime", "task_id", task.ID, "reason", escalationErr.reason)
			runtimeTraceEnd = len(task.Trace)
			return c.Run(withForceLegacyRuntime(ctx), task)
		}
		if dagErr != nil {
			span.RecordError(dagErr)
			span.SetStatus(codes.Error, "adaptive DAG runtime failed")
		}
		span.SetAttributes(
			attribute.String("agent.task.final_status", string(task.Status)),
			attribute.Int("agent.task.final_answer_chars", len([]rune(task.FinalAnswer))),
		)
		return dagErr
	}
	if runtimeMode == RuntimeDAG {
		span.SetAttributes(
			attribute.String("multiagent.runtime.effective", string(RuntimeLegacy)),
			attribute.String("multiagent.runtime.fallback_reason", "workflow_not_migrated"),
		)
		log.Warn("DAG runtime requested for a workflow that has not migrated; using legacy runtime", "task_id", task.ID, "workflow", configuredWorkflow)
		if c.Metrics != nil {
			c.Metrics.ObserveMultiAgentRuntimeFallback("dag_fallback:workflow_not_migrated")
		}
	} else {
		span.SetAttributes(attribute.String("multiagent.runtime.effective", string(RuntimeLegacy)))
	}

	// ── Phase 1: Plan ──────────────────────────────────────────────────────────
	plan, err := c.runPlanPhase(ctx, task)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "plan phase failed")
		return err
	}
	route := resolveWorkflow(configuredWorkflow, teamCfg.Routing, task, plan)
	if c.Metrics != nil {
		c.Metrics.ObserveMultiAgentRoute(string(route.Configured), string(route.Effective), route.Reason)
	}
	workflow := route.Effective
	ctx = withWorkflow(ctx, workflow)
	effectiveGraphSummary, err := annotateWorkflowGraph(span, "multiagent.workflow.effective_graph", workflow)
	if err != nil {
		task.Status = types.StatusFailed
		span.RecordError(err)
		span.SetStatus(codes.Error, "effective workflow graph is invalid")
		return err
	}
	if configuredWorkflow == WorkflowAdaptive {
		recordWorkflowRoute(task, route)
	}
	log.Info("Selected multi-agent workflow", "task_id", task.ID, "configured_workflow", configuredWorkflow, "workflow", workflow, "reason", route.Reason, "effective_graph_digest", effectiveGraphSummary.Digest)
	span.SetAttributes(
		attribute.String("multiagent.workflow.configured", string(configuredWorkflow)),
		attribute.String("multiagent.workflow.effective", string(workflow)),
		attribute.String("multiagent.workflow.route_reason", route.Reason),
	)
	if workflow == WorkflowReviewed {
		plan, err = c.requireCriticApproval(ctx, task, plan)
		if err != nil {
			task.Status = types.StatusFailed
			span.RecordError(err)
			span.SetStatus(codes.Error, "critic phase failed")
			return err
		}
	} else {
		c.critiqueResearchPlan(ctx, task, plan)
	}
	span.SetAttributes(attribute.Int("multiagent.plan.step_count", len(plan.Steps)))
	if c.EventCallback != nil {
		c.EventCallback(task.ID, task.Status)
	}

	allEvidence := recoverStepEvidence(task.Trace)
	currentSteps := plan.Steps
	depthIterations := 0
	maxDepthIterations := 2
	executionComplete := true
	executionReason := ""
	finalSufficient := false

	for {
		// ── Phase 2: Research / Execute ────────────────────────────────────────────
		researchResult := c.runResearchPhase(ctx, task, currentSteps, configuredWorkflow, teamCfg.Routing)
		allEvidence = append(allEvidence, researchResult.Evidence...)
		if researchResult.Workflow == WorkflowReviewed && workflow != WorkflowReviewed {
			workflow = WorkflowReviewed
			ctx = withWorkflow(ctx, workflow)
		}
		if researchResult.Err != nil {
			task.Status = types.StatusFailed
			span.RecordError(researchResult.Err)
			span.SetStatus(codes.Error, "research phase failed")
			return researchResult.Err
		}
		if !researchResult.Complete {
			executionComplete = false
			if executionReason == "" {
				executionReason = researchResult.Reason
			}
		}
		span.SetAttributes(attribute.Int("multiagent.research.evidence_items", len(allEvidence)))

		select {
		case <-ctx.Done():
			log.Info("Context cancelled during execution flow", "task_id", task.ID)
			return ctx.Err()
		default:
		}

		// ── Phase 3: Write / Verify ────────────────────────────────────────────────
		phaseCtx := llmcore.WithTaskRoutingHints(ctx, task)
		writerEvidence := append([]StepEvidence(nil), allEvidence...)
		if annotation := c.resolveEvidenceConflicts(phaseCtx, task, allEvidence); annotation != nil {
			writerEvidence = append(writerEvidence, *annotation)
		}
		if c.ResolveMemoryConflicts != nil {
			c.ResolveMemoryConflicts(phaseCtx, task)
			phaseCtx = llmcore.WithTaskRoutingHints(ctx, task)
		}
		var draftConfidence string
		var writeErr error
		if workflow == WorkflowReviewed {
			var supported bool
			draftConfidence, supported, writeErr = c.runVerifyPhase(phaseCtx, task, writerEvidence, executionComplete, executionReason)
			finalSufficient = supported
		} else {
			draftConfidence, writeErr = c.runWritePhase(phaseCtx, task, writerEvidence)
			finalSufficient = draftConfidence != "low"
		}
		if writeErr != nil {
			break // fallback happened or writer failed
		}

		// Adaptive Step Depth expansion: draft confidence is a generation-only
		// signal and never becomes the published answer confidence.
		if draftConfidence == "low" && depthIterations < maxDepthIterations && task.ToolBudget > 0 && multiAgentToolStepCount(task) < task.MaxSteps && !tokenBudgetExhausted(task) {
			depthIterations++
			log.Info("Draft confidence is LOW (evidence is insufficient). Triggering adaptive step depth expansion", "task_id", task.ID, "iteration", depthIterations, "max_iterations", maxDepthIterations)

			// Record adaptive depth trace
			task.Trace = append(task.Trace, types.StepTrace{
				Step:        task.StepCount,
				Goal:        task.Goal,
				Action:      "plan",
				Query:       "adaptive_depth",
				Observation: "[coordinator] draft confidence was low; requesting additional steps for deeper investigation",
				AgentRole:   RolePlanner,
			})
			task.StepCount++

			// Re-plan additional steps based on traces history
			replanCtx := llmcore.WithTaskRoutingHints(ctx, task)
			replanStart := time.Now()
			newPlan, replanErr := c.Planner.Replan(replanCtx, task.Goal, task.Workspace, task.Trace, task.Memories)
			if c.Metrics != nil {
				outcome := "success"
				if replanErr != nil {
					outcome = "error"
				}
				c.Metrics.ObserveMultiAgentPhase("replanner", outcome, time.Since(replanStart))
			}
			if replanErr != nil || len(newPlan.Steps) == 0 {
				log.Error("Adaptive replan failed or returned empty steps — stopping loop", "task_id", task.ID)
				break
			}
			enforceJITResearchPlan(task, newPlan)
			enforceWorkspaceResearchPlan(task, newPlan)
			ensureExplicitWorkspaceFileReads(task, newPlan)
			if configuredWorkflow == WorkflowAdaptive && workflow != WorkflowReviewed {
				escalation := resolveAdaptiveWorkflow(teamCfg.Routing, task, newPlan)
				if escalation.Effective == WorkflowReviewed {
					escalation.Reason = "adaptive_replan:" + escalation.Reason
					workflow = WorkflowReviewed
					ctx = withWorkflow(ctx, workflow)
					replanCtx = withWorkflow(replanCtx, workflow)
					recordWorkflowEscalation(task, escalation)
					if c.Metrics != nil {
						c.Metrics.ObserveMultiAgentRoute(string(escalation.Configured), string(escalation.Effective), escalation.Reason)
					}
					log.Info("Escalated adaptive workflow after depth replan", "task_id", task.ID, "workflow", workflow, "reason", escalation.Reason)
				}
			}

			if c.Metrics != nil {
				c.Metrics.ObserveTokens(newPlan.TokenUsage.PromptTokens, newPlan.TokenUsage.CompletionTokens, newPlan.TokenUsage.TotalTokens, "replanner")
			}
			// Record the re-plan trace so the writer knows the plan changed.
			task.Trace = append(task.Trace, types.StepTrace{
				Step:        task.StepCount,
				Goal:        task.Goal,
				Action:      "plan",
				Query:       "replanner",
				Observation: fmt.Sprintf("[replanner] %s — %d replacement step(s) planned", newPlan.ThoughtSummary, len(newPlan.Steps)),
				AgentRole:   RolePlanner,
				TokenUsage:  newPlan.TokenUsage,
			})
			task.StepCount++
			if workflow == WorkflowReviewed {
				newPlan, replanErr = c.requireCriticApproval(replanCtx, task, newPlan)
				if replanErr != nil {
					log.Error("Adaptive plan rejected by critic", "task_id", task.ID, "error", replanErr)
					task.Status = types.StatusFailed
					break
				}
			} else {
				c.critiqueResearchPlan(replanCtx, task, newPlan)
			}

			// Prepare new steps for the next iteration
			currentSteps = newPlan.Steps
			task.Status = types.StatusRunning
			if c.EventCallback != nil {
				c.EventCallback(task.ID, task.Status)
			}
			continue
		}

		break
	}

	if task.Status != types.StatusFailed {
		if HasPendingVerifierDraft(task) {
			task.Status = types.StatusPartial
			appendUnresolvedReason(task, verifierRetryReason)
		} else if executionComplete && finalSufficient {
			task.Status = types.StatusCompleted
		} else {
			task.Status = types.StatusPartial
			reason := executionReason
			if reason == "" {
				reason = "final_answer_not_fully_supported"
			}
			appendUnresolvedReason(task, reason)
		}
		if c.Metrics != nil {
			c.Metrics.IncCompleted()
		}
	}

	log.Info("Workflow complete", "task_id", task.ID, "status", task.Status)
	span.SetAttributes(
		attribute.String("agent.task.final_status", string(task.Status)),
		attribute.Int("agent.task.final_answer_chars", len([]rune(task.FinalAnswer))),
	)
	return nil
}

func runtimeInvocationReplanned(traces []types.StepTrace, start, end int) bool {
	if start < 0 {
		start = 0
	}
	if end > len(traces) {
		end = len(traces)
	}
	if start > end {
		return false
	}
	for _, trace := range traces[start:end] {
		if trace.Query == "replanner" || trace.Query == "critic_replan" {
			return true
		}
	}
	return false
}
