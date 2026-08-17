package multiagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/evidenceconflict"
	"github.com/wuxujun/ai-agent/internal/evidencefilter"
	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/logger"
	"github.com/wuxujun/ai-agent/internal/metrics"
	"github.com/wuxujun/ai-agent/internal/plancritic"
	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/promptguard"
	"github.com/wuxujun/ai-agent/internal/promptmanager"
	"github.com/wuxujun/ai-agent/internal/sourcecredibility"
	"github.com/wuxujun/ai-agent/internal/tools"
	"github.com/wuxujun/ai-agent/internal/types"
	"github.com/wuxujun/ai-agent/internal/wiki"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
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

type workflowContextKey struct{}
type approvalAgentRoleContextKey struct{}

func withWorkflow(ctx context.Context, workflow Workflow) context.Context {
	return context.WithValue(ctx, workflowContextKey{}, workflow)
}

func workflowFromContext(ctx context.Context) Workflow {
	if workflow, ok := ctx.Value(workflowContextKey{}).(Workflow); ok {
		return workflow
	}
	return WorkflowResearch
}

// WithApprovalAgentRole identifies the multi-agent execution role that is
// requesting a high-risk approval. The orchestrator uses it when recording a
// rejection trace without depending on unexported workflow context details.
func WithApprovalAgentRole(ctx context.Context, role AgentRole) context.Context {
	return context.WithValue(ctx, approvalAgentRoleContextKey{}, role)
}

func ApprovalAgentRoleFromContext(ctx context.Context) (AgentRole, bool) {
	if ctx == nil {
		return "", false
	}
	role, ok := ctx.Value(approvalAgentRoleContextKey{}).(AgentRole)
	return role, ok && role != ""
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

// NewCoordinator creates a Coordinator wired to the default LLM configuration
// derived from environment variables (same vars as the main planner).
func NewCoordinator(mc *metrics.Collector) *Coordinator {
	return &Coordinator{
		Planner:                  &PlannerAgent{ArgumentRepairer: planner.NewLLMToolArgumentRepairer(config.LLMSceneToolArgumentRepair)},
		Researcher:               &ResearcherAgent{},
		Executor:                 &ExecutorAgent{},
		Writer:                   &WriterAgent{},
		Verifier:                 &VerifierAgent{},
		FinalVerifier:            &VerifierAgent{},
		PlanCritic:               &CriticAgent{},
		PromptInjectionDetector:  promptguard.NewLLMDetector(config.LLMScenePromptInjectionDetector),
		EvidenceRelevanceFilter:  evidencefilter.NewLLMFilter(config.LLMSceneEvidenceRelevanceFilter),
		EvidenceConflictResolver: evidenceconflict.NewLLMResolver(config.LLMSceneEvidenceConflictResolver),
		SourceCredibilityScorer:  sourcecredibility.NewLLMScorer(config.LLMSceneSourceCredibilityScorer),
		Metrics:                  mc,
	}
}

// Run executes the full multi-agent workflow for the given task, updating
// task.Trace, task.StepCount, task.ToolBudget, and task.Status in-place.
//
// The selected team's workflow controls whether tool steps and final synthesis
// are owned by Researcher/Writer or Executor/Verifier. Both paths retain the
// same policy, approval, cancellation, and budget gates.
func (c *Coordinator) Run(ctx context.Context, task *types.Task) (runErr error) {
	ctx = logger.WithTaskID(ctx, task.ID)
	ctx = tools.WithRetrievalExecutionContext(ctx, task.ID, task.TenantID)
	ctx = llmcore.WithTaskBudget(ctx, task)
	ctx = llmcore.WithTaskRoutingHints(ctx, task)
	ctx, span := tracer.Start(ctx, "multiagent.coordinator.run")
	defer span.End()

	span.SetAttributes(
		attribute.String("agent.task.id", task.ID),
		attribute.Int("agent.task.goal_chars", len([]rune(task.Goal))),
	)

	teamsCfg := GetTeamsConfig()
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

func annotateWorkflowGraph(span trace.Span, prefix string, workflow Workflow) (WorkflowGraphSummary, error) {
	graph, err := BuildWorkflowGraph(workflow)
	if err != nil {
		return WorkflowGraphSummary{}, err
	}
	summary, err := graph.Summary()
	if err != nil {
		return WorkflowGraphSummary{}, fmt.Errorf("summarize workflow graph %q: %w", workflow, err)
	}
	span.SetAttributes(
		attribute.String(prefix+".digest", summary.Digest),
		attribute.Int(prefix+".node_count", summary.NodeCount),
		attribute.Int(prefix+".level_count", summary.LevelCount),
		attribute.Int(prefix+".max_level_width", summary.MaxLevelWidth),
		attribute.Int(prefix+".conditional_nodes", summary.ConditionalNodes),
	)
	return summary, nil
}

func recordWorkflowRoute(task *types.Task, decision workflowRouteDecision) {
	if task == nil {
		return
	}
	for i := len(task.Trace) - 1; i >= 0; i-- {
		trace := task.Trace[i]
		if trace.Action == WorkflowRouteTraceAction {
			if trace.Query == string(decision.Effective) {
				return
			}
			break
		}
	}
	appendWorkflowRouteTrace(task, decision)
}

func recordWorkflowEscalation(task *types.Task, decision workflowRouteDecision) {
	appendWorkflowRouteTrace(task, decision)
}

func appendWorkflowRouteTrace(task *types.Task, decision workflowRouteDecision) {
	if task == nil {
		return
	}
	observation, _ := json.Marshal(decision)
	task.Trace = append(task.Trace, types.StepTrace{
		Step:        task.StepCount,
		Goal:        task.Goal,
		Action:      WorkflowRouteTraceAction,
		Query:       string(decision.Effective),
		Observation: string(observation),
		AgentRole:   RolePlanner,
	})
	task.StepCount++
}

// ── phase helpers ─────────────────────────────────────────────────────────────

func (c *Coordinator) runPlanPhase(ctx context.Context, task *types.Task) (*ResearchPlan, error) {
	log := teamLogger(ctx)
	log.Info("Phase 1 — Planning", "task_id", task.ID)

	start := time.Now()
	var plan *ResearchPlan
	var err error
	if decision, ok := planner.NextJITRetrievalDecision(task); ok && !decision.Stop {
		plan = researchPlanFromJITDecision(task, decision)
		log.Info("Planning resolved by JIT retrieval router", "task_id", task.ID, "action", plan.Steps[0].Action, "rag_configured", strings.TrimSpace(config.Get().RAG.SearchURL) != "")
	} else {
		plan, err = c.Planner.Plan(ctx, task.Goal, task.Workspace, task.Memories)
	}
	elapsed := time.Since(start)

	if c.Metrics != nil {
		c.Metrics.ObservePlanner(elapsed, err)
		outcome := "success"
		if err != nil {
			outcome = "error"
		}
		c.Metrics.ObserveMultiAgentPhase("planner", outcome, elapsed)
	}
	if err != nil {
		return nil, fmt.Errorf("PlannerAgent: %w", err)
	}
	if enforceJITResearchPlan(task, plan) {
		log.Info("Adjusted research plan to JIT retrieval route", "task_id", task.ID, "action", plan.Steps[0].Action)
	}
	if enforceWorkspaceResearchPlan(task, plan) {
		log.Info("Adjusted research plan to workspace discovery route", "task_id", task.ID)
	}
	ensureExplicitWorkspaceFileReads(task, plan)

	if c.Metrics != nil {
		c.Metrics.ObserveTokens(plan.TokenUsage.PromptTokens, plan.TokenUsage.CompletionTokens, plan.TokenUsage.TotalTokens, "planner")
	}
	task.Hypothesis = plan.ThoughtSummary
	planTrace := types.StepTrace{
		Step:   task.StepCount,
		Goal:   task.Goal,
		Action: "plan",
		Query:  "planner",
		Observation: fmt.Sprintf("[planner] %s — %d step(s) planned",
			plan.ThoughtSummary, len(plan.Steps)),
		AgentRole:  RolePlanner,
		TokenUsage: plan.TokenUsage,
	}
	teamSnapshot := teamConfigFromContext(ctx)
	if teamSnapshot.Digest != "" {
		planTrace.Evidence = []types.Evidence{{
			Path:  "team_config",
			Query: teamSnapshot.ActiveTeam,
			Lines: []string{"digest:" + teamSnapshot.Digest},
		}}
	}
	task.Trace = append(task.Trace, planTrace)
	task.StepCount++
	task.Status = types.StatusRunning

	log.Info("Phase 1 done", "task_id", task.ID, "steps_planned", len(plan.Steps), "elapsed", elapsed)
	return plan, nil
}

func researchPlanFromJITDecision(task *types.Task, decision *planner.PlanDecision) *ResearchPlan {
	action := decision.Actions[0]
	query, _ := action.Parameters["query"].(string)
	if query == "" {
		query = task.Goal
	}
	return &ResearchPlan{
		ThoughtSummary: decision.ThoughtSummary,
		Steps: []ResearchStep{{
			ID:                 "step-1",
			Description:        "Retrieve authoritative evidence before factual synthesis",
			Action:             action.Action,
			SearchQuery:        query,
			RepairedParameters: action.Parameters,
		}},
	}
}

func (c *Coordinator) critiqueResearchPlan(ctx context.Context, task *types.Task, plan *ResearchPlan) {
	_, _ = c.reviewResearchPlan(ctx, task, plan, false, false)
}

func (c *Coordinator) reviewResearchPlan(ctx context.Context, task *types.Task, plan *ResearchPlan, required, countStep bool) (*plancritic.Result, error) {
	log := teamLogger(ctx)
	if c.PlanCritic == nil || plan == nil {
		if required {
			return nil, fmt.Errorf("reviewed workflow requires a Critic")
		}
		return nil, nil
	}
	if !required {
		if _, enabled := config.Get().LLM.Scenes[config.LLMScenePlanCritic]; !enabled || !llmcore.AllowedForTask(config.LLMScenePlanCritic, task) {
			return nil, nil
		}
	}
	neutral := criticPlanFromResearchPlan(plan)
	if !required && !plancritic.ShouldCritique(task, neutral) {
		return nil, nil
	}
	fingerprint := plancritic.Fingerprint(neutral)
	if !required && plancritic.AlreadyCritiqued(task, fingerprint) {
		return &plancritic.Result{Approved: true, Summary: "plan already reviewed"}, nil
	}
	criticStart := time.Now()
	result, usage, err := c.PlanCritic.Critique(ctx, task, neutral)
	criticElapsed := time.Since(criticStart)
	plancritic.ApplyResult(task, neutral, result, usage, err)
	if len(task.Trace) > 0 {
		trace := &task.Trace[len(task.Trace)-1]
		trace.Goal = task.Goal
		trace.AgentRole = RoleCritic
	}
	if countStep {
		task.StepCount++
	}
	if err != nil {
		log.Warn("Plan critic failed; deterministic controls remain active", "task_id", task.ID, "error", err)
	}
	if c.Metrics != nil {
		c.Metrics.ObserveTokens(usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens, "plan_critic")
		outcome := "error"
		if err == nil && result != nil && result.Approved {
			outcome = "approved"
		} else if err == nil && result != nil {
			outcome = "rejected"
		}
		c.Metrics.ObserveMultiAgentCriticReview(outcome)
		c.Metrics.ObserveMultiAgentPhase("critic", outcome, criticElapsed)
	}
	return result, err
}

func (c *Coordinator) requireCriticApproval(ctx context.Context, task *types.Task, plan *ResearchPlan) (*ResearchPlan, error) {
	maxReplans := 1
	policy := teamConfigFromContext(ctx).Team.CriticPolicy
	if policy.MaxReplans != nil {
		maxReplans = *policy.MaxReplans
	}
	if maxReplans < 0 {
		maxReplans = 0
	}
	if maxReplans > 5 {
		maxReplans = 5
	}

	current := plan
	seenPlans := make(map[string]struct{}, maxReplans+1)
	for replanCount := 0; ; replanCount++ {
		fingerprint := plancritic.Fingerprint(criticPlanFromResearchPlan(current))
		if _, repeated := seenPlans[fingerprint]; repeated {
			return nil, fmt.Errorf("CriticAgent convergence stopped: replanner repeated plan %s", fingerprint)
		}
		seenPlans[fingerprint] = struct{}{}

		result, err := c.reviewResearchPlan(ctx, task, current, true, true)
		if err != nil {
			return nil, fmt.Errorf("CriticAgent: %w", err)
		}
		if result != nil && result.Approved {
			return current, nil
		}
		if replanCount >= maxReplans {
			return nil, fmt.Errorf("CriticAgent rejected plan after %d replan(s)", replanCount)
		}

		replanStart := time.Now()
		if c.Metrics != nil {
			c.Metrics.IncMultiAgentCriticReplan()
		}
		revised, err := c.Planner.Replan(ctx, task.Goal, task.Workspace, task.Trace, task.Memories)
		if c.Metrics != nil {
			outcome := "success"
			if err != nil {
				outcome = "error"
			}
			c.Metrics.ObserveMultiAgentPhase("replanner", outcome, time.Since(replanStart))
		}
		if err != nil {
			return nil, fmt.Errorf("CriticAgent rejected plan and replanning failed: %w", err)
		}
		if revised == nil || len(revised.Steps) == 0 {
			return nil, fmt.Errorf("CriticAgent rejected plan and replanning returned no executable steps")
		}
		enforceJITResearchPlan(task, revised)
		enforceWorkspaceResearchPlan(task, revised)
		ensureExplicitWorkspaceFileReads(task, revised)
		task.Trace = append(task.Trace, types.StepTrace{
			Step:        task.StepCount,
			Goal:        task.Goal,
			Action:      "plan",
			Query:       "critic_replan",
			Observation: fmt.Sprintf("[planner] %s — %d revised step(s) after critic review", revised.ThoughtSummary, len(revised.Steps)),
			AgentRole:   RolePlanner,
			TokenUsage:  revised.TokenUsage,
		})
		task.StepCount++
		if c.Metrics != nil {
			c.Metrics.ObserveTokens(revised.TokenUsage.PromptTokens, revised.TokenUsage.CompletionTokens, revised.TokenUsage.TotalTokens, "replanner")
		}
		current = revised
	}
}

func criticPlanFromResearchPlan(plan *ResearchPlan) plancritic.Plan {
	if plan == nil {
		return plancritic.Plan{}
	}
	neutral := plancritic.Plan{Summary: plan.ThoughtSummary, Steps: make([]plancritic.Step, 0, len(plan.Steps))}
	for _, step := range plan.Steps {
		neutral.Steps = append(neutral.Steps, plancritic.Step{Action: step.Action, Description: step.Description, Parameters: stepToParams(step)})
	}
	return neutral
}

func (c *Coordinator) runResearchPhase(ctx context.Context, task *types.Task, steps []ResearchStep, configuredWorkflow Workflow, routing WorkflowRoutingConfig) researchPhaseResult {
	log := teamLogger(ctx)
	log.Info("Phase 2 — Researching", "task_id", task.ID)

	result := researchPhaseResult{Complete: true, Workflow: workflowFromContext(ctx)}

	currentSteps := make([]ResearchStep, len(steps))
	copy(currentSteps, steps)

	replansCount := 0
	maxReplans := 3

	for len(currentSteps) > 0 {
		for i := range currentSteps {
			normalizeStepWorkspacePath(task.Workspace, &currentSteps[i])
		}
		// Budget and step-count gate
		if task.ToolBudget <= 0 || tokenBudgetExhausted(task) {
			log.Info("Budget exhausted (tools or tokens) — stopping research early", "task_id", task.ID)
			result.Complete = false
			if tokenBudgetExhausted(task) {
				result.Reason = "token_budget_exhausted"
			} else {
				result.Reason = "tool_budget_exhausted"
			}
			break
		}
		toolSteps := multiAgentToolStepCount(task)
		if toolSteps >= task.MaxSteps {
			log.Info("Max steps reached — stopping research early", "task_id", task.ID)
			result.Complete = false
			result.Reason = "max_tool_steps_reached"
			break
		}

		// Context cancellation check
		select {
		case <-ctx.Done():
			log.Info("Context cancelled during research phase", "task_id", task.ID)
			result.Complete = false
			result.Reason = "context_cancelled"
			return result
		default:
		}

		// Partition: collect a batch of parallelisable (read-only) steps at the front,
		// or fall back to a single serial step.
		batch, remainder, isParallel := partitionBatch(currentSteps, task.ToolBudget, task.MaxSteps-toolSteps)
		currentSteps = remainder

		// Look-ahead token budget defense: clamp parallel batch size if remaining budget is tight
		if task.TokenBudget > 0 && isParallel && len(batch) > 1 {
			used := totalTokensUsed(task)
			remaining := task.TokenBudget - used
			if remaining > 0 {
				estPerStep := estimateTokensPerStep(task)
				maxParallel := remaining / estPerStep
				if maxParallel < 1 {
					maxParallel = 1
				}
				if len(batch) > maxParallel {
					log.Info("Look-ahead token budget defense triggered: clamping parallel batch size",
						"task_id", task.ID,
						"original_size", len(batch),
						"clamped_size", maxParallel,
						"remaining_budget", remaining,
						"estimated_tokens_per_step", estPerStep,
					)
					// Return the trimmed steps back to the front of currentSteps
					trimmed := batch[maxParallel:]
					batch = batch[:maxParallel]
					currentSteps = append(trimmed, currentSteps...)
				}
			}
		}

		var batchEvidence []StepEvidence
		var anyFailed bool
		var fatalErr error

		if isParallel && len(batch) > 1 {
			log.Info("Executing read-only steps in parallel", "task_id", task.ID, "count", len(batch))
			batchEvidence, anyFailed = c.runBatchParallel(ctx, task, batch)
		} else {
			log.Info("Executing steps serially", "task_id", task.ID, "count", len(batch))
			batchEvidence, anyFailed, fatalErr = c.runBatchSerial(ctx, task, batch)
		}

		result.Evidence = append(result.Evidence, batchEvidence...)
		if fatalErr != nil {
			result.Complete = false
			result.Reason = "approval_handler_unavailable"
			result.Err = fatalErr
			return result
		}
		if followups := retrievalFollowupSteps(ctx, batchEvidence); len(followups) > 0 {
			// Search candidates are intentionally compact. Fetch selected details
			// before unrelated remaining work so the Writer receives real evidence,
			// not just candidate snippets.
			currentSteps = append(followups, currentSteps...)
		}

		// Trigger re-planning if any step in the batch failed
		if anyFailed && replansCount < maxReplans {
			replansCount++
			log.Info("Triggering collaborative replan/error-correction loop", "task_id", task.ID, "replan_count", replansCount)

			replanStart := time.Now()
			newPlan, replanErr := c.Planner.Replan(ctx, task.Goal, task.Workspace, task.Trace, task.Memories)
			if c.Metrics != nil {
				outcome := "success"
				if replanErr != nil {
					outcome = "error"
				}
				c.Metrics.ObserveMultiAgentPhase("replanner", outcome, time.Since(replanStart))
			}
			if replanErr != nil {
				log.Error("Replanner failed — continuing with remaining steps", "task_id", task.ID, "error", replanErr)
			} else if len(newPlan.Steps) > 0 {
				enforceJITResearchPlan(task, newPlan)
				enforceWorkspaceResearchPlan(task, newPlan)
				ensureExplicitWorkspaceFileReads(task, newPlan)
				if configuredWorkflow == WorkflowAdaptive && workflowFromContext(ctx) != WorkflowReviewed {
					escalation := resolveAdaptiveWorkflow(routing, task, newPlan)
					if escalation.Effective == WorkflowReviewed {
						escalation.Reason = "execution_replan:" + escalation.Reason
						ctx = withWorkflow(ctx, WorkflowReviewed)
						result.Workflow = WorkflowReviewed
						recordWorkflowEscalation(task, escalation)
						if c.Metrics != nil {
							c.Metrics.ObserveMultiAgentRoute(string(escalation.Configured), string(escalation.Effective), escalation.Reason)
						}
						log.Info("Escalated adaptive workflow after execution replan", "task_id", task.ID, "workflow", WorkflowReviewed, "reason", escalation.Reason)
					}
				}
				log.Info("Replanner generated revised steps", "task_id", task.ID, "count", len(newPlan.Steps))
				if c.Metrics != nil {
					c.Metrics.ObserveTokens(newPlan.TokenUsage.PromptTokens, newPlan.TokenUsage.CompletionTokens, newPlan.TokenUsage.TotalTokens, "replanner")
				}
				task.Trace = append(task.Trace, types.StepTrace{
					Step:        task.StepCount,
					Goal:        task.Goal,
					Action:      "plan",
					Query:       "replanner",
					Observation: fmt.Sprintf("[replanner] %s — %d step(s) revised due to failure", newPlan.ThoughtSummary, len(newPlan.Steps)),
					AgentRole:   RolePlanner,
					TokenUsage:  newPlan.TokenUsage,
				})
				task.StepCount++
				if workflowFromContext(ctx) == WorkflowReviewed {
					newPlan, replanErr = c.requireCriticApproval(ctx, task, newPlan)
					if replanErr != nil {
						log.Error("Revised plan rejected by critic", "task_id", task.ID, "error", replanErr)
						task.Status = types.StatusFailed
						result.Complete = false
						result.Reason = "critic_rejected_recovery_plan"
						return result
					}
				} else {
					c.critiqueResearchPlan(ctx, task, newPlan)
				}
				currentSteps = newPlan.Steps
				continue
			}
		}
		if anyFailed {
			result.Complete = false
			if result.Reason == "" {
				result.Reason = "execution_step_failed"
			}
		}
	}

	log.Info("Phase 2 done", "task_id", task.ID, "evidence_items", len(result.Evidence), "complete", result.Complete, "reason", result.Reason)
	return result
}

// isReadOnlyAction returns true only for registered Low Risk tools. RiskLevel is
// the registry's authoritative concurrency contract: new read-only tools become
// parallelizable automatically, unknown tools stay serial, and High Risk tools
// are forced through the serial approval path.
func isReadOnlyAction(action string) bool {
	registered, ok := tools.Get(action)
	return ok && registered.RiskLevel() == types.RiskLevelLow
}

// requiresSequentialDiscovery identifies read-only tools whose output commonly
// supplies a path/query to a following step. Running their consumers in the
// same parallel batch violates the ordered ResearchPlan contract.
func requiresSequentialDiscovery(action string) bool {
	return action == "find_files" || action == "search_text"
}

// normalizeStepWorkspacePath accepts the common LLM form
// "<workspace>/<relative path>" while tools already execute relative to the
// task workspace. Removing that duplicate prefix prevents workspace/workspace
// lookups without weakening the tool layer's traversal checks.
func normalizeStepWorkspacePath(workspace string, step *ResearchStep) {
	if step == nil {
		return
	}
	workspace = filepath.Clean(strings.TrimSpace(workspace))
	if workspace != "." && workspace != "" && !filepath.IsAbs(workspace) && step.Action == "execute_code" {
		prefix := workspace + string(filepath.Separator)
		step.Args = strings.ReplaceAll(step.Args, "./"+prefix, "")
		step.Args = strings.ReplaceAll(step.Args, "'"+prefix, "'")
		step.Args = strings.ReplaceAll(step.Args, "\""+prefix, "\"")
		step.Args = strings.ReplaceAll(step.Args, " "+prefix, " ")
		step.Args = strings.TrimPrefix(step.Args, prefix)
		if step.RepairedParameters != nil {
			if _, exists := step.RepairedParameters["args"]; exists {
				step.RepairedParameters["args"] = step.Args
			}
		}
	}
	if strings.TrimSpace(step.FilePath) == "" || filepath.IsAbs(step.FilePath) {
		return
	}
	path := filepath.Clean(strings.TrimSpace(step.FilePath))
	if workspace == "." || workspace == "" || path == workspace {
		return
	}
	prefix := workspace + string(filepath.Separator)
	if !strings.HasPrefix(path, prefix) {
		return
	}
	relative := strings.TrimPrefix(path, prefix)
	if relative == "" || relative == "." {
		return
	}
	step.FilePath = relative
	if step.RepairedParameters != nil {
		if _, exists := step.RepairedParameters["path"]; exists {
			step.RepairedParameters["path"] = relative
		}
	}
}

// enforceJITResearchPlan prevents a multi-agent plan for an external factual
// lookup from drifting into workspace or execution tools before retrieval.
// Candidate detail steps are inserted later by retrievalFetchSteps.
func enforceJITResearchPlan(task *types.Task, plan *ResearchPlan) bool {
	if task == nil || plan == nil || planner.HasSupportingEvidence(task.Trace) {
		return false
	}
	action, ok := planner.PreferredJITSearchAction(task)
	if !ok {
		return false
	}
	// If the preferred JIT search action has already been attempted, we should not
	// override the plan again. This allows the replanner to fallback to other tools
	// (like web_search) if RAG returned no results.
	for _, tr := range task.Trace {
		if tr.Action == action {
			return false
		}
	}
	if len(plan.Steps) == 1 && plan.Steps[0].Action == action {
		return false
	}
	plan.ThoughtSummary = "Retrieve authoritative evidence before factual synthesis"
	plan.Steps = []ResearchStep{{
		ID:          "step-1",
		Description: "Search the configured retrieval source for evidence",
		Action:      action,
		SearchQuery: task.Goal,
	}}
	return true
}

// enforceWorkspaceResearchPlan repairs the inverse of JIT drift: an explicit
// local workspace/repository goal planned entirely as external retrieval. It is
// intentionally narrow so mixed plans and legitimate RAG-first goals remain
// under planner control.
func enforceWorkspaceResearchPlan(task *types.Task, plan *ResearchPlan) bool {
	if task == nil || plan == nil || !planner.GoalExplicitlyTargetsWorkspace(task.Goal) || len(plan.Steps) == 0 {
		return false
	}
	for _, step := range plan.Steps {
		switch step.Action {
		case "wiki_search", "wiki_fetch", "rag_search", "rag_fetch", "memory_search", "memory_get", "web_search", "http_fetch":
			// External-only plans are repaired below.
		default:
			return false
		}
	}
	plan.ThoughtSummary = "Discover and inspect relevant workspace files"
	plan.Steps = []ResearchStep{
		{ID: "step-1", Description: "Discover files in the task workspace", Action: "find_files", FileGlob: "*"},
		{ID: "step-2", Description: "Search workspace files for terms from the goal", Action: "search_text", SearchQuery: task.Goal},
	}
	return true
}

var explicitWorkspaceFilePattern = regexp.MustCompile(`(?i)(?:^|[^[:alnum:]_./-])([[:alnum:]_.-]+\.(?:md|go|json|ya?ml|txt|csv))(?:$|[^[:alnum:]_./-])`)

// ensureExplicitWorkspaceFileReads prevents discovery-only plans from claiming
// to answer questions about a specifically named local file without reading it.
func ensureExplicitWorkspaceFileReads(task *types.Task, plan *ResearchPlan) bool {
	if task == nil || plan == nil || !planner.GoalExplicitlyTargetsWorkspace(task.Goal) {
		return false
	}
	changed := false
	for _, match := range explicitWorkspaceFilePattern.FindAllStringSubmatch(task.Goal, -1) {
		name := filepath.Base(strings.TrimSpace(match[1]))
		if name == "." || name == "" {
			continue
		}
		found := false
		for _, step := range plan.Steps {
			if step.Action == "read_file" && filepath.Base(strings.TrimSpace(step.FilePath)) == name {
				found = true
				break
			}
		}
		if !found {
			for _, trace := range task.Trace {
				if trace.Action == "read_file" && strings.Contains(trace.Query, fmt.Sprintf("%q", name)) {
					found = true
					break
				}
			}
		}
		if found || (task.MaxSteps > 0 && len(plan.Steps) >= task.MaxSteps) {
			continue
		}
		plan.Steps = append(plan.Steps, ResearchStep{
			ID: fmt.Sprintf("step-%d", len(plan.Steps)+1), Description: "Read the explicitly named workspace file " + name,
			Action: "read_file", FilePath: name,
		})
		changed = true
	}
	return changed
}

func retrievalFetchSteps(evidence []StepEvidence) []ResearchStep {
	return retrievalFollowupSteps(context.Background(), evidence)
}

func retrievalFollowupSteps(ctx context.Context, evidence []StepEvidence) []ResearchStep {
	steps := make([]ResearchStep, 0, len(evidence))
	for _, item := range evidence {
		if item.Failed {
			continue
		}
		if item.Action == "wiki_fetch" && teamConfigFromContext(ctx).ActiveTeam == "wiki_graph" {
			for _, source := range item.Evidence {
				if strings.HasPrefix(source.Path, "wiki://") {
					steps = append(steps, ResearchStep{
						ID: item.StepID + "-graph", Description: "Read the bounded Wiki relationship graph", Action: "wiki_graph",
						GraphURI: source.Path, GraphDepth: 2, GraphDirection: "both",
					})
					break
				}
			}
			continue
		}
		if item.Action == "wiki_graph" && teamConfigFromContext(ctx).ActiveTeam == "wiki_graph" {
			var graph wiki.GraphResult
			if json.Unmarshal([]byte(item.Observation), &graph) != nil {
				continue
			}
			uris := make([]string, 0, 3)
			for _, node := range graph.Nodes {
				if node.URI == graph.RootURI || !strings.HasPrefix(node.URI, "wiki://") {
					continue
				}
				uris = append(uris, node.URI)
				if len(uris) == 3 {
					break
				}
			}
			if len(uris) > 0 {
				steps = append(steps, ResearchStep{
					ID: item.StepID + "-fetch", Description: "Fetch selected bounded Wiki graph neighbor pages", Action: "wiki_graph_fetch",
					RepairedParameters: map[string]any{"uris": uris},
				})
			}
			continue
		}
		if item.Action != "wiki_search" && item.Action != "rag_search" && item.Action != "memory_search" {
			continue
		}
		var payload struct {
			Results []struct {
				ID string `json:"id"`
			} `json:"results"`
		}
		if json.Unmarshal([]byte(item.Observation), &payload) != nil {
			continue
		}
		limit := config.Get().RAG.JITFetchMaxItems
		if item.Action == "wiki_search" {
			limit = config.Get().Wiki.FetchMaxItems
		}
		if limit <= 0 {
			limit = 3
		}
		ids := make([]string, 0, min(limit, len(payload.Results)))
		for _, candidate := range payload.Results {
			if candidate.ID != "" {
				ids = append(ids, candidate.ID)
			}
			if len(ids) >= limit {
				break
			}
		}
		if len(ids) == 0 {
			continue
		}
		action := "rag_fetch"
		if item.Action == "memory_search" {
			action = "memory_get"
		} else if item.Action == "wiki_search" {
			action = "wiki_fetch"
		}
		steps = append(steps, ResearchStep{
			ID:                 item.StepID + "-fetch",
			Description:        "Fetch selected evidence from " + item.Action,
			Action:             action,
			RepairedParameters: map[string]any{"ids": ids},
		})
	}
	return steps
}

// partitionBatch returns the largest safe batch from the front of steps.
// isParallel is true when the batch contains only read-only actions.
// budgetLeft and stepsLeft cap the batch size.
func partitionBatch(steps []ResearchStep, budgetLeft, stepsLeft int) (batch []ResearchStep, remainder []ResearchStep, isParallel bool) {
	if len(steps) == 0 {
		return nil, nil, false
	}
	// If first step is serial, return it alone
	if !isReadOnlyAction(steps[0].Action) || requiresSequentialDiscovery(steps[0].Action) {
		return steps[:1], steps[1:], false
	}
	// Collect consecutive read-only steps up to budget/step limits
	end := 0
	for end < len(steps) && isReadOnlyAction(steps[end].Action) && !requiresSequentialDiscovery(steps[end].Action) && end < budgetLeft && end < stepsLeft {
		end++
	}
	if end == 0 {
		end = 1
	}
	return steps[:end], steps[end:], true
}

func (c *Coordinator) executeWorkflowStep(ctx context.Context, workspace string, step ResearchStep) (*StepEvidence, error) {
	if workflowFromContext(ctx) == WorkflowReviewed {
		if c.Executor == nil {
			return nil, fmt.Errorf("reviewed workflow requires an Executor")
		}
		return c.Executor.Execute(ctx, workspace, step)
	}
	if c.Researcher == nil {
		return nil, fmt.Errorf("research workflow requires a Researcher")
	}
	return c.Researcher.Research(ctx, workspace, step)
}

func executionTraceIdentity(ctx context.Context) (AgentRole, string) {
	if workflowFromContext(ctx) == WorkflowReviewed {
		return RoleExecutor, "executor"
	}
	return RoleResearcher, "researcher"
}

// multiAgentToolStepCount derives the persisted tool-step count from role-tagged
// traces. StepCount remains the global trace sequence and is intentionally not
// used to enforce MaxSteps in multi-agent mode.
func multiAgentToolStepCount(task *types.Task) int {
	if task == nil {
		return 0
	}
	count := 0
	for _, trace := range task.Trace {
		if _, registered := tools.Get(trace.Action); registered && !isApprovalGateTrace(trace) {
			count++
		}
	}
	return count
}

func isApprovalGateTrace(trace types.StepTrace) bool {
	for _, evidence := range trace.Evidence {
		if evidence.Path == "user_feedback" && evidence.Query == "disapproval" {
			return true
		}
		if evidence.Path == "approval" && evidence.Query == "handler_unavailable" {
			return true
		}
	}
	return false
}

func appendUnresolvedReason(task *types.Task, reason string) {
	if task == nil || strings.TrimSpace(reason) == "" {
		return
	}
	for _, existing := range task.Unresolved {
		if existing == reason {
			return
		}
	}
	if len(task.Unresolved) < 10 {
		task.Unresolved = append(task.Unresolved, reason)
	}
}

// runBatchParallel executes a batch of read-only steps concurrently.
func (c *Coordinator) runBatchParallel(ctx context.Context, task *types.Task, batch []ResearchStep) (evidence []StepEvidence, anyFailed bool) {
	type result struct {
		ev      *StepEvidence
		tr      types.StepTrace
		failed  bool
		elapsed time.Duration
		action  string
		err     error
	}

	results := make([]result, len(batch))
	agentRole, agentLabel := executionTraceIdentity(ctx)

	var wg sync.WaitGroup
	for i, step := range batch {
		wg.Add(1)
		go func(idx int, s ResearchStep) {
			defer wg.Done()
			start := time.Now()
			ev, err := c.executeWorkflowStep(ctx, task.Workspace, s)
			elapsed := time.Since(start)

			var obs string
			failed := (err != nil) || (ev != nil && ev.Failed)
			if err != nil {
				obs = fmt.Sprintf("[%s] fatal error: %v", agentLabel, err)
			} else if ev != nil {
				obs = fmt.Sprintf("[%s] %s", agentLabel, ev.Observation)
			}

			var trEvidence []types.Evidence
			if ev != nil {
				trEvidence = ev.Evidence
			}
			var tokenUsage types.TokenUsage
			if ev != nil {
				tokenUsage = ev.TokenUsage
			}

			tr := types.StepTrace{
				Goal:        task.Goal,
				Action:      s.Action,
				Query:       buildStepQuery(s),
				Observation: obs,
				Evidence:    trEvidence,
				TokenUsage:  tokenUsage,
				AgentRole:   agentRole,
			}

			results[idx] = result{
				ev:      ev,
				tr:      tr,
				failed:  failed,
				elapsed: elapsed,
				action:  s.Action,
				err:     err,
			}
		}(i, step)
	}
	wg.Wait()

	// Merge results back into task state (in order)
	for _, r := range results {
		if c.Metrics != nil {
			c.Metrics.ObserveExecutor(r.elapsed, r.err, r.action)
		}
		audit := c.inspectStepEvidence(ctx, task, r.ev, r.failed)
		relevanceAudit := c.filterStepEvidence(ctx, task, r.ev, r.failed)
		if r.ev != nil {
			r.tr.Observation = fmt.Sprintf("[%s] %s", agentLabel, r.ev.Observation)
			r.tr.Evidence = r.ev.Evidence
		}
		r.tr.Step = task.StepCount
		task.Trace = append(task.Trace, r.tr)
		task.StepCount++
		if audit != nil {
			audit.Step = task.StepCount
			task.Trace = append(task.Trace, *audit)
			task.StepCount++
		}
		if relevanceAudit != nil {
			relevanceAudit.Step = task.StepCount
			task.Trace = append(task.Trace, *relevanceAudit)
			task.StepCount++
		}
		if r.ev != nil && !r.failed {
			evidence = append(evidence, *r.ev)
		}
		if r.failed {
			anyFailed = true
		}
	}
	task.ToolBudget -= len(batch)
	if c.EventCallback != nil {
		c.EventCallback(task.ID, task.Status)
	}
	task.Status = types.StatusRunning
	return
}

// runBatchSerial executes steps one at a time (used for write/execute steps).
func (c *Coordinator) runBatchSerial(ctx context.Context, task *types.Task, batch []ResearchStep) (evidence []StepEvidence, anyFailed bool, fatalErr error) {
	log := teamLogger(ctx)
	agentRole, agentLabel := executionTraceIdentity(ctx)
	for _, step := range batch {
		if task.ToolBudget <= 0 || multiAgentToolStepCount(task) >= task.MaxSteps {
			break
		}
		select {
		case <-ctx.Done():
			return evidence, anyFailed, nil
		default:
		}

		log.Info("Executing research step", "task_id", task.ID, "step_num", task.StepCount+1, "step_id", step.ID, "action", step.Action, "desc", step.Description)

		tool, ok := tools.Get(step.Action)
		if ok && tool.RiskLevel() == types.RiskLevelHigh {
			if c.SuspendForApproval == nil {
				fatalErr = fmt.Errorf("high-risk action %q requires an approval handler", step.Action)
				task.Trace = append(task.Trace, types.StepTrace{
					Step:        task.StepCount,
					Goal:        task.Goal,
					Action:      step.Action,
					Query:       buildStepQuery(step),
					Observation: fmt.Sprintf("[%s] blocked before execution: approval handler unavailable", agentLabel),
					Error:       fatalErr.Error(),
					Evidence: []types.Evidence{{
						Path:  "approval",
						Query: "handler_unavailable",
						Lines: []string{"High-risk action was not executed."},
					}},
					AgentRole: agentRole,
				})
				task.StepCount++
				anyFailed = true
				break
			}
			approvalCtx := WithApprovalAgentRole(ctx, agentRole)
			approved, newParams, err := c.SuspendForApproval(approvalCtx, task, step.Action, stepToParams(step))
			if err != nil {
				log.Error("Action approval error", "task_id", task.ID, "action", step.Action, "error", err)
				anyFailed = true
				break
			}
			if !approved {
				// Rejection trace is already appended inside SuspendForApproval.
				// We mark it as failed and break so that Phase 2's replanner triggers.
				anyFailed = true
				break
			}
			if newParams != nil {
				paramsToStep(newParams, &step)
			}
		}

		start := time.Now()
		ev, err := c.executeWorkflowStep(ctx, task.Workspace, step)
		elapsed := time.Since(start)

		if c.Metrics != nil {
			c.Metrics.ObserveExecutor(elapsed, err, step.Action)
		}

		var obs string
		if err != nil {
			obs = fmt.Sprintf("[%s] fatal error: %v", agentLabel, err)
		} else if ev != nil {
			obs = fmt.Sprintf("[%s] %s", agentLabel, ev.Observation)
		}

		failed := (err != nil) || (ev != nil && ev.Failed)
		audit := c.inspectStepEvidence(ctx, task, ev, failed)
		relevanceAudit := c.filterStepEvidence(ctx, task, ev, failed)
		if ev != nil && err == nil {
			obs = fmt.Sprintf("[%s] %s", agentLabel, ev.Observation)
		}

		var trEvidence []types.Evidence
		if ev != nil {
			trEvidence = ev.Evidence
		}
		var tokenUsage types.TokenUsage
		if ev != nil {
			tokenUsage = ev.TokenUsage
		}

		task.Trace = append(task.Trace, types.StepTrace{
			Step:        task.StepCount,
			Goal:        task.Goal,
			Action:      step.Action,
			Query:       buildStepQuery(step),
			Observation: obs,
			Evidence:    trEvidence,
			TokenUsage:  tokenUsage,
			AgentRole:   agentRole,
		})
		task.StepCount++
		if audit != nil {
			audit.Step = task.StepCount
			task.Trace = append(task.Trace, *audit)
			task.StepCount++
		}
		if relevanceAudit != nil {
			relevanceAudit.Step = task.StepCount
			task.Trace = append(task.Trace, *relevanceAudit)
			task.StepCount++
		}

		if ev != nil && !failed {
			evidence = append(evidence, *ev)
		}
		if failed {
			anyFailed = true
		}

		task.ToolBudget--
		task.Status = types.StatusRunning
	}
	return
}

func (c *Coordinator) runVerifyPhase(ctx context.Context, task *types.Task, evidence []StepEvidence, executionComplete bool, executionReason string) (string, bool, error) {
	log := teamLogger(ctx)
	log.Info("Phase 3 — Verifying execution result", "task_id", task.ID)
	if c.FinalVerifier == nil {
		err := fmt.Errorf("reviewed workflow requires a final Verifier")
		task.Status = types.StatusFailed
		return "low", false, err
	}

	start := time.Now()
	if verifier, ok := c.FinalVerifier.(CheckpointFinalVerifier); ok {
		draftStart := time.Now()
		var draft *VerificationDraft
		var answerChunks []string
		var draftUsage types.TokenUsage
		var err error
		for attempt := 1; attempt <= 2; attempt++ {
			draftCtx := ctx
			var attemptChunks []string
			if attempt > 1 {
				draftCtx = withAnswerRegeneration(draftCtx)
			}
			if c.TokenCallback != nil {
				draftCtx = withAnswerTokenCallback(draftCtx, func(token string) { attemptChunks = append(attemptChunks, token) })
			}
			candidate, draftErr := verifier.Draft(draftCtx, task.Goal, evidence, task.Memories)
			if candidate != nil {
				addMultiAgentUsage(&draftUsage, candidate.TokenUsage)
			}
			if draftErr != nil {
				err = draftErr
				break
			}
			if qualityErr := validateVerificationDraft(candidate); qualityErr == nil {
				draft = candidate
				draft.TokenUsage = draftUsage
				answerChunks = attemptChunks
				err = nil
				break
			} else {
				err = fmt.Errorf("invalid verifier draft: %w", qualityErr)
				log.Warn("Verifier draft rejected", "task_id", task.ID, "attempt", attempt, "error", qualityErr)
			}
		}
		if c.Metrics != nil {
			outcome := "success"
			if err != nil {
				outcome = "error"
			}
			c.Metrics.ObserveMultiAgentPhase("verifier_draft", outcome, time.Since(draftStart))
		}
		if err != nil {
			c.recordVerifierFailure(task, err, draftUsage, false)
			task.FinalAnswer = invalidAnswerFallback
			return "low", false, err
		}
		checkpoint := appendVerifierDraftCheckpoint(task, draft, evidence, executionComplete, executionReason)
		if c.Metrics != nil {
			c.Metrics.ObserveTokens(draft.TokenUsage.PromptTokens, draft.TokenUsage.CompletionTokens, draft.TokenUsage.TotalTokens, "verifier_draft")
		}
		if c.PersistTask != nil {
			if err := c.PersistTask(ctx, task); err != nil {
				if c.Metrics != nil {
					c.Metrics.ObserveMultiAgentVerifierCheckpoint("persist_error")
				}
				persistErr := fmt.Errorf("persist verifier draft checkpoint: %w", err)
				c.recordVerifierFailure(task, persistErr, types.TokenUsage{}, false)
				return "low", false, persistErr
			}
		}
		if c.Metrics != nil {
			outcome := "in_memory"
			if c.PersistTask != nil {
				outcome = "persisted"
			}
			c.Metrics.ObserveMultiAgentVerifierCheckpoint(outcome)
		}

		verifyStart := time.Now()
		verification, err := verifier.Verify(ctx, task.Goal, draft.FinalAnswer, checkpoint.Evidence)
		if c.Metrics != nil {
			outcome := "success"
			if err != nil {
				outcome = "error"
			}
			c.Metrics.ObserveMultiAgentPhase("verifier", outcome, time.Since(verifyStart))
		}
		if err != nil {
			usage := types.TokenUsage{}
			if verification != nil {
				usage = verification.TokenUsage
			}
			c.recordVerifierFailure(task, err, usage, true)
			return "low", false, err
		}
		if c.Metrics != nil {
			c.Metrics.ObserveTokens(verification.TokenUsage.PromptTokens, verification.TokenUsage.CompletionTokens, verification.TokenUsage.TotalTokens, "verifier")
		}
		output := finalVerificationOutput(draft, verification)
		confidence := c.applyFinalVerificationOutput(task, output, verification.TokenUsage, time.Since(start))
		if output.Supported && c.TokenCallback != nil {
			for _, token := range answerChunks {
				c.TokenCallback(task.ID, token)
			}
		}
		return confidence, output.Supported, nil
	}

	var output *FinalVerificationOutput
	var verifierUsage types.TokenUsage
	var err error
	for attempt := 1; attempt <= 2; attempt++ {
		finalizeCtx := ctx
		if attempt > 1 {
			finalizeCtx = withAnswerRegeneration(finalizeCtx)
		}
		candidate, finalizeErr := c.FinalVerifier.Finalize(finalizeCtx, task.Goal, evidence, task.Memories)
		if candidate != nil {
			addMultiAgentUsage(&verifierUsage, candidate.TokenUsage)
		}
		if finalizeErr != nil {
			err = finalizeErr
			break
		}
		if qualityErr := validateFinalVerificationOutput(candidate); qualityErr != nil {
			err = fmt.Errorf("invalid final verifier answer: %w", qualityErr)
			log.Warn("Final verifier answer rejected", "task_id", task.ID, "attempt", attempt, "error", qualityErr)
			continue
		}
		output = candidate
		output.TokenUsage = verifierUsage
		err = nil
		break
	}
	elapsed := time.Since(start)
	if c.Metrics != nil {
		outcome := "success"
		if err != nil {
			outcome = "error"
		}
		c.Metrics.ObserveMultiAgentPhase("verifier", outcome, elapsed)
	}
	if err != nil {
		c.recordVerifierFailure(task, err, verifierUsage, false)
		task.FinalAnswer = invalidAnswerFallback
		return "low", false, err
	}
	if c.Metrics != nil {
		c.Metrics.ObserveTokens(output.TokenUsage.PromptTokens, output.TokenUsage.CompletionTokens, output.TokenUsage.TotalTokens, "verifier")
	}
	confidence := c.applyFinalVerificationOutput(task, output, output.TokenUsage, elapsed)
	return confidence, output.Supported, nil
}

func finalVerificationOutput(draft *VerificationDraft, verification *VerificationResult) *FinalVerificationOutput {
	output := &FinalVerificationOutput{
		FinalAnswer:     draft.FinalAnswer,
		EvidenceSummary: draft.EvidenceSummary,
		DraftConfidence: draft.DraftConfidence,
		Supported:       verification.Supported,
		Issues:          verification.Issues,
		TokenUsage:      draft.TokenUsage,
	}
	addMultiAgentUsage(&output.TokenUsage, verification.TokenUsage)
	if !output.Supported {
		output.DraftConfidence = "low"
	}
	return output
}

func (c *Coordinator) applyFinalVerificationOutput(task *types.Task, output *FinalVerificationOutput, traceUsage types.TokenUsage, elapsed time.Duration) string {
	if err := validateFinalVerificationOutput(output); err != nil {
		log.Error("Final verifier output failed publication guard", "task_id", task.ID, "error", err)
		if output == nil {
			output = &FinalVerificationOutput{}
		}
		output.FinalAnswer = invalidAnswerFallback
		output.DraftConfidence = "low"
		output.Supported = false
		output.EvidenceSummary = "The generated final answer failed the publication quality guard."
		output.Issues = append(output.Issues, VerificationIssue{Kind: "evidence_gap", Detail: err.Error(), SourceID: "final_answer"})
	}
	if strings.EqualFold(strings.TrimSpace(config.Get().RAG.ContextMode), "jit") && planner.RequiresFactualEvidence(task) && !planner.HasSupportingEvidence(task.Trace) {
		output.FinalAnswer = "未检索到足够证据，暂时无法可靠回答该事实性问题。"
		output.DraftConfidence = "low"
		output.Supported = false
		output.EvidenceSummary = "No successful retrieval or tool evidence supports a factual answer."
		if len(output.Issues) == 0 {
			output.Issues = []VerificationIssue{{Kind: "evidence_gap", Detail: "No successful retrieval evidence", SourceID: "final_answer"}}
		}
	}
	confidence := output.resolvedDraftConfidence()
	task.Trace = append(task.Trace, types.StepTrace{
		Step:        task.StepCount,
		Goal:        task.Goal,
		Action:      "verify",
		Query:       "verifier",
		Observation: fmt.Sprintf("[verifier] supported=%t confidence=%s | Summary: %s", output.Supported, confidence, output.EvidenceSummary),
		Evidence:    verificationIssuesAsEvidence(output.Issues),
		AgentRole:   RoleVerifier,
		TokenUsage:  traceUsage,
	})
	task.StepCount++
	task.FinalAnswer = output.FinalAnswer
	log.Info("Phase 3 done — result verified", "task_id", task.ID, "supported", output.Supported, "draft_confidence", confidence, "elapsed", elapsed)
	return confidence
}

func (c *Coordinator) recordVerifierFailure(task *types.Task, err error, usage types.TokenUsage, retryable bool) {
	log.Error("VerifierAgent failed", "task_id", task.ID, "error", err, "retryable", retryable)
	task.Trace = append(task.Trace, types.StepTrace{
		Step:        task.StepCount,
		Goal:        task.Goal,
		Action:      "verify",
		Query:       "verifier",
		Observation: fmt.Sprintf("[verifier] final verification error: %v", err),
		Error:       err.Error(),
		AgentRole:   RoleVerifier,
		TokenUsage:  usage,
	})
	task.StepCount++
	if c.Metrics != nil && usage.TotalTokens > 0 {
		c.Metrics.ObserveTokens(usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens, "verifier")
	}
	if retryable {
		task.Status = types.StatusPartial
		appendUnresolvedReason(task, verifierRetryReason)
		return
	}
	task.Status = types.StatusFailed
}

func (c *Coordinator) resumeVerifierCheckpoint(ctx context.Context, task *types.Task, checkpoint verifierDraftCheckpoint) error {
	log := teamLogger(ctx)
	verifier, ok := c.FinalVerifier.(CheckpointFinalVerifier)
	if !ok {
		err := fmt.Errorf("pending verifier draft requires a checkpoint-capable FinalVerifier")
		if c.Metrics != nil {
			c.Metrics.ObserveMultiAgentVerifierResume("unsupported_verifier")
		}
		c.recordVerifierFailure(task, err, types.TokenUsage{}, false)
		return err
	}
	if err := validateVerificationDraft(&checkpoint.Draft); err != nil {
		checkpointErr := fmt.Errorf("invalid persisted verifier draft: %w", err)
		c.recordVerifierFailure(task, checkpointErr, checkpoint.Draft.TokenUsage, false)
		task.FinalAnswer = invalidAnswerFallback
		return checkpointErr
	}
	task.Status = types.StatusRunning
	verifyStart := time.Now()
	verification, err := verifier.Verify(ctx, task.Goal, checkpoint.Draft.FinalAnswer, checkpoint.Evidence)
	if c.Metrics != nil {
		outcome := "success"
		if err != nil {
			outcome = "error"
		}
		c.Metrics.ObserveMultiAgentPhase("verifier_resume", outcome, time.Since(verifyStart))
	}
	if err != nil {
		usage := types.TokenUsage{}
		if verification != nil {
			usage = verification.TokenUsage
		}
		if c.Metrics != nil {
			c.Metrics.ObserveMultiAgentVerifierResume("retryable_error")
		}
		c.recordVerifierFailure(task, err, usage, true)
		return nil
	}
	if c.Metrics != nil {
		c.Metrics.ObserveTokens(verification.TokenUsage.PromptTokens, verification.TokenUsage.CompletionTokens, verification.TokenUsage.TotalTokens, "verifier")
		c.Metrics.ObserveMultiAgentVerifierResume("success")
	}
	output := finalVerificationOutput(&checkpoint.Draft, verification)
	confidence := c.applyFinalVerificationOutput(task, output, verification.TokenUsage, 0)
	removeUnresolvedReason(task, verifierRetryReason)
	if checkpoint.ExecutionComplete && output.Supported {
		task.Status = types.StatusCompleted
	} else {
		task.Status = types.StatusPartial
		reason := checkpoint.ExecutionReason
		if reason == "" {
			reason = "final_answer_not_fully_supported"
		}
		appendUnresolvedReason(task, reason)
	}
	if c.EventCallback != nil {
		c.EventCallback(task.ID, task.Status)
	}
	log.Info("Verifier checkpoint resumed", "task_id", task.ID, "supported", output.Supported, "draft_confidence", confidence, "status", task.Status)
	return nil
}

func (c *Coordinator) runWritePhase(ctx context.Context, task *types.Task, evidence []StepEvidence) (string, error) {
	log := teamLogger(ctx)
	log.Info("Phase 3 — Writing final answer", "task_id", task.ID)

	start := time.Now()
	var answerChunks []string
	var output *WriterOutput
	var writerUsage types.TokenUsage
	var err error
	qualityRejected := false
	for attempt := 1; attempt <= 2; attempt++ {
		writeCtx := ctx
		var attemptChunks []string
		if attempt > 1 {
			writeCtx = withAnswerRegeneration(writeCtx)
		}
		if c.TokenCallback != nil {
			writeCtx = withAnswerTokenCallback(writeCtx, func(token string) { attemptChunks = append(attemptChunks, token) })
		}
		candidate, writeErr := c.Writer.Write(writeCtx, task.Goal, evidence, task.Memories)
		if candidate != nil {
			addMultiAgentUsage(&writerUsage, candidate.TokenUsage)
		}
		if writeErr != nil {
			err = writeErr
			break
		}
		if qualityErr := validateWriterOutput(candidate); qualityErr != nil {
			qualityRejected = true
			err = fmt.Errorf("invalid writer answer: %w", qualityErr)
			log.Warn("Writer answer rejected", "task_id", task.ID, "attempt", attempt, "error", qualityErr)
			continue
		}
		output = candidate
		output.TokenUsage = writerUsage
		answerChunks = attemptChunks
		err = nil
		break
	}
	elapsed := time.Since(start)

	if c.Metrics != nil {
		c.Metrics.ObserveWriter(elapsed, err)
		outcome := "success"
		if err != nil {
			outcome = "error"
		}
		c.Metrics.ObserveMultiAgentPhase("writer", outcome, elapsed)
	}

	if err != nil {
		log.Error("WriterAgent failed — marking task failed with best-effort summary", "task_id", task.ID, "error", err)
		// The synthesis step failed. Preserve the gathered evidence as a
		// best-effort answer for callers, but mark the task FAILED so the error
		// is not masked as a successful completion. Previously this set
		// StatusCompleted, which made writer errors indistinguishable from a
		// genuine success at the API/status layer.
		fallback := "Research complete but synthesis failed. See trace for gathered evidence."
		if qualityRejected {
			fallback = invalidAnswerFallback
		}
		task.Trace = append(task.Trace, types.StepTrace{
			Step:        task.StepCount,
			Goal:        task.Goal,
			Action:      "write",
			Query:       "writer",
			Observation: fmt.Sprintf("[writer] synthesis error: %v", err),
			Error:       err.Error(),
			AgentRole:   RoleWriter,
			TokenUsage:  writerUsage,
		})
		task.StepCount++
		task.Status = types.StatusFailed
		task.FinalAnswer = fallback
		return "low", err
	}

	if c.Metrics != nil {
		c.Metrics.ObserveTokens(output.TokenUsage.PromptTokens, output.TokenUsage.CompletionTokens, output.TokenUsage.TotalTokens, "writer")
	}
	var verificationEvidence []types.Evidence
	_, verificationEnabled := config.Get().LLM.Scenes[config.LLMSceneAnswerVerifier]
	if c.Verifier != nil && verificationEnabled && llmcore.AllowedForTask(config.LLMSceneAnswerVerifier, task) {
		verification, verifyErr := c.Verifier.Verify(ctx, task.Goal, output.FinalAnswer, evidence)
		if verifyErr == nil {
			verifyErr = validateVerificationResult(verification)
		}
		if verifyErr != nil {
			log.Warn("answer verifier failed; preserving writer result", "task_id", task.ID, "error", verifyErr)
		} else {
			if c.Metrics != nil {
				c.Metrics.ObserveTokens(verification.TokenUsage.PromptTokens, verification.TokenUsage.CompletionTokens, verification.TokenUsage.TotalTokens, "verifier")
			}
			if !verification.Supported {
				output.DraftConfidence = "low"
				output.EvidenceSummary = fmt.Sprintf("%s | Verifier reported %d structured issue(s).", output.EvidenceSummary, len(verification.Issues))
				verificationEvidence = verificationIssuesAsEvidence(verification.Issues)
			}
			output.TokenUsage.PromptTokens += verification.TokenUsage.PromptTokens
			output.TokenUsage.CompletionTokens += verification.TokenUsage.CompletionTokens
			output.TokenUsage.TotalTokens += verification.TokenUsage.TotalTokens
		}
	}
	if strings.EqualFold(strings.TrimSpace(config.Get().RAG.ContextMode), "jit") && planner.RequiresFactualEvidence(task) && !planner.HasSupportingEvidence(task.Trace) {
		output.FinalAnswer = "未检索到足够证据，暂时无法可靠回答该事实性问题。"
		output.DraftConfidence = "low"
		output.EvidenceSummary = "No successful retrieval or tool evidence supports a factual answer."
	}
	draftConfidence := output.resolvedDraftConfidence()

	task.Trace = append(task.Trace, types.StepTrace{
		Step:        task.StepCount,
		Goal:        task.Goal,
		Action:      "write",
		Query:       "writer",
		Observation: fmt.Sprintf("[writer] Draft confidence: %s | Summary: %s", draftConfidence, output.EvidenceSummary),
		Evidence:    verificationEvidence,
		AgentRole:   RoleWriter,
		TokenUsage:  output.TokenUsage,
	})
	task.StepCount++
	task.FinalAnswer = output.FinalAnswer
	if draftConfidence != "low" && c.TokenCallback != nil {
		for _, token := range answerChunks {
			c.TokenCallback(task.ID, token)
		}
	}

	log.Info("Phase 3 done — draft written", "task_id", task.ID, "draft_confidence", draftConfidence, "elapsed", elapsed)
	return draftConfidence, nil
}

func verificationIssuesAsEvidence(issues []VerificationIssue) []types.Evidence {
	result := make([]types.Evidence, 0, len(issues))
	for _, issue := range issues {
		sourceID := issue.SourceID
		if sourceID == "" {
			sourceID = "final_answer"
		}
		result = append(result, types.Evidence{
			Path:  types.AnswerVerifierEvidencePrefix + sourceID,
			Query: issue.Kind,
			Lines: []string{issue.Detail},
		})
	}
	return result
}

func totalTokensUsed(task *types.Task) int {
	total := 0
	for _, tr := range task.Trace {
		total += tr.TokenUsage.TotalTokens
	}
	return total
}

// recoverStepEvidence keeps successful evidence gathered before a suspended or
// interrupted Multi-Agent run available to the resumed Writer/Verifier.
func recoverStepEvidence(traces []types.StepTrace) []StepEvidence {
	result := make([]StepEvidence, 0)
	for _, trace := range traces {
		if trace.AgentRole != RoleResearcher && trace.AgentRole != RoleExecutor {
			continue
		}
		if _, registered := tools.Get(trace.Action); !registered {
			continue
		}
		result = append(result, StepEvidence{
			StepID:      fmt.Sprintf("trace-%d", trace.Step),
			StepDesc:    "Evidence recovered from a previous execution segment",
			Action:      trace.Action,
			Observation: trace.Observation,
			Evidence:    append([]types.Evidence(nil), trace.Evidence...),
			TokenUsage:  trace.TokenUsage,
			Failed:      trace.Error != "",
		})
	}
	return result
}

// tokenBudgetExhausted reports whether the task has reached or exceeded its
// token budget, summing TokenUsage across all recorded trace entries
// (planner, researcher, writer, replanner). TokenBudget <= 0 means "no token
// limit", in which case this always returns false.
//
// It is used as a gate in both the research phase and the adaptive-depth loop
// so that plan/replan/write iterations cannot keep burning tokens past the
// budget — previously only the research phase enforced it.
func tokenBudgetExhausted(task *types.Task) bool {
	if task.TokenBudget <= 0 {
		return false
	}
	return totalTokensUsed(task) >= task.TokenBudget
}

func estimateTokensPerStep(task *types.Task) int {
	totalTokens := 0
	stepCount := 0
	for _, tr := range task.Trace {
		if tr.Action != "" && tr.Action != "plan" && tr.Action != "stop" && tr.TokenUsage.TotalTokens > 0 {
			totalTokens += tr.TokenUsage.TotalTokens
			stepCount++
		}
	}
	if stepCount > 0 {
		return totalTokens / stepCount
	}
	return 2000 // default fallback estimate
}

// buildStepQuery constructs a structured, human-readable query string for
// StepTrace.Query. Only the fields relevant to the step's action are included,
// preventing the previous issue where all parameters were blindly concatenated
// into an unreadable string like "keyword*.go/path/file python3 args".
func buildStepQuery(step ResearchStep) string {
	switch step.Action {
	case "search_text":
		q := fmt.Sprintf("query=%q", step.SearchQuery)
		if step.FileGlob != "" {
			q += fmt.Sprintf(" glob=%q", step.FileGlob)
		}
		return q
	case "find_files":
		return fmt.Sprintf("glob=%q", step.FileGlob)
	case "read_file":
		return fmt.Sprintf("path=%q", step.FilePath)
	case "write_file":
		return fmt.Sprintf("path=%q", step.FilePath)
	case "execute_code":
		if step.Args != "" {
			return fmt.Sprintf("cmd=%q args=%q", step.Command, step.Args)
		}
		return fmt.Sprintf("cmd=%q", step.Command)
	case "git_diff":
		if step.FilePath != "" {
			return fmt.Sprintf("path=%q", step.FilePath)
		}
		return "workspace"
	case "http_fetch":
		return fmt.Sprintf("url=%q", step.URL)
	case "web_search":
		return fmt.Sprintf("query=%q", step.SearchQuery)
	case "wiki_search", "rag_search", "memory_search":
		return fmt.Sprintf("query=%q", step.SearchQuery)
	case "wiki_fetch", "rag_fetch", "memory_get":
		if ids, ok := step.RepairedParameters["ids"]; ok {
			return fmt.Sprintf("ids=%v", ids)
		}
		return "ids=[]"
	case "wiki_graph":
		return fmt.Sprintf("uri=%q depth=%d direction=%q", step.GraphURI, step.GraphDepth, step.GraphDirection)
	case "analyze_image":
		return fmt.Sprintf("path=%q prompt=%q", step.FilePath, step.Prompt)
	default:
		return fmt.Sprintf("action=%q", step.Action)
	}
}

func paramsToStep(params map[string]any, step *ResearchStep) {
	step.RepairedParameters = make(map[string]any, len(params))
	for name, value := range params {
		step.RepairedParameters[name] = value
	}
	if pattern, ok := params["pattern"].(string); ok {
		step.FileGlob = pattern
	}
	if glob, ok := params["glob"].(string); ok {
		step.FileGlob = glob
	}
	if query, ok := params["query"].(string); ok {
		step.SearchQuery = query
	}
	if path, ok := params["path"].(string); ok {
		step.FilePath = path
	}
	if prompt, ok := params["prompt"].(string); ok {
		step.Prompt = prompt
	}
	if content, ok := params["content"].(string); ok {
		step.Content = content
	}
	if command, ok := params["command"].(string); ok {
		step.Command = command
	}
	if args, ok := params["args"].(string); ok {
		step.Args = args
	}
	if url, ok := params["url"].(string); ok {
		step.URL = url
	}
	if uri, ok := params["uri"].(string); ok {
		step.GraphURI = uri
	}
	if depth, ok := params["depth"].(int); ok {
		step.GraphDepth = depth
	} else if depth, ok := params["depth"].(float64); ok {
		step.GraphDepth = int(depth)
	}
	if direction, ok := params["direction"].(string); ok {
		step.GraphDirection = direction
	}
}
