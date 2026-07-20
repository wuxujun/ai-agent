package multiagent

import (
	"context"
	"encoding/json"
	"fmt"
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

type workflowContextKey struct{}

func withWorkflow(ctx context.Context, workflow Workflow) context.Context {
	return context.WithValue(ctx, workflowContextKey{}, workflow)
}

func workflowFromContext(ctx context.Context) Workflow {
	if workflow, ok := ctx.Value(workflowContextKey{}).(Workflow); ok {
		return workflow
	}
	return WorkflowResearch
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
func (c *Coordinator) Run(ctx context.Context, task *types.Task) error {
	ctx = tools.WithRetrievalExecutionContext(ctx, task.ID, task.TenantID)
	ctx = llmcore.WithTaskBudget(ctx, task)
	ctx = llmcore.WithTaskRoutingHints(ctx, task)
	ctx, span := tracer.Start(ctx, "multiagent.coordinator.run")
	defer span.End()

	span.SetAttributes(
		attribute.String("agent.task.id", task.ID),
		attribute.String("agent.task.goal", task.Goal),
	)

	teamsCfg := GetTeamsConfig()
	workflow := teamsCfg.ActiveWorkflow()
	ctx = withWorkflow(ctx, workflow)
	log.Info("Starting multi-agent workflow", "task_id", task.ID, "goal", task.Goal, "active_team", teamsCfg.ActiveTeam, "workflow", workflow)

	// ── Phase 1: Plan ──────────────────────────────────────────────────────────
	plan, err := c.runPlanPhase(ctx, task)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "plan phase failed")
		return err
	}
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

	var allEvidence []StepEvidence
	currentSteps := plan.Steps
	depthIterations := 0
	maxDepthIterations := 2

	for {
		// ── Phase 2: Research / Execute ────────────────────────────────────────────
		evidenceBatch := c.runResearchPhase(ctx, task, currentSteps)
		allEvidence = append(allEvidence, evidenceBatch...)
		span.SetAttributes(attribute.Int("multiagent.research.evidence_items", len(allEvidence)))

		select {
		case <-ctx.Done():
			log.Info("Context cancelled during execution flow")
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
			draftConfidence, writeErr = c.runVerifyPhase(phaseCtx, task, writerEvidence)
		} else {
			draftConfidence, writeErr = c.runWritePhase(phaseCtx, task, writerEvidence)
		}
		if writeErr != nil {
			break // fallback happened or writer failed
		}

		// Adaptive Step Depth expansion: draft confidence is a generation-only
		// signal and never becomes the published answer confidence.
		if draftConfidence == "low" && depthIterations < maxDepthIterations && task.ToolBudget > 0 && task.StepCount < task.MaxSteps && !tokenBudgetExhausted(task) {
			depthIterations++
			log.Info("Draft confidence is LOW (evidence is insufficient). Triggering adaptive step depth expansion", "iteration", depthIterations, "max_iterations", maxDepthIterations)

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
			newPlan, replanErr := c.Planner.Replan(replanCtx, task.Goal, task.Workspace, task.Trace, task.Memories)
			if replanErr != nil || len(newPlan.Steps) == 0 {
				log.Error("Adaptive replan failed or returned empty steps — stopping loop")
				break
			}
			enforceJITResearchPlan(task, newPlan)

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
					log.Error("Adaptive plan rejected by critic", "error", replanErr)
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
		task.Status = types.StatusCompleted
		if c.Metrics != nil {
			c.Metrics.IncCompleted()
		}
	}

	log.Info("Workflow complete", "task_id", task.ID, "status", task.Status)
	span.SetAttributes(
		attribute.String("agent.task.final_status", string(task.Status)),
		attribute.String("agent.task.final_answer", task.FinalAnswer),
	)
	return nil
}

// ── phase helpers ─────────────────────────────────────────────────────────────

func (c *Coordinator) runPlanPhase(ctx context.Context, task *types.Task) (*ResearchPlan, error) {
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
	}
	if err != nil {
		return nil, fmt.Errorf("PlannerAgent: %w", err)
	}
	if enforceJITResearchPlan(task, plan) {
		log.Info("Adjusted research plan to JIT retrieval route", "task_id", task.ID, "action", plan.Steps[0].Action)
	}

	if c.Metrics != nil {
		c.Metrics.ObserveTokens(plan.TokenUsage.PromptTokens, plan.TokenUsage.CompletionTokens, plan.TokenUsage.TotalTokens, "planner")
	}
	task.Hypothesis = plan.ThoughtSummary
	task.Trace = append(task.Trace, types.StepTrace{
		Step:   task.StepCount,
		Goal:   task.Goal,
		Action: "plan",
		Query:  "planner",
		Observation: fmt.Sprintf("[planner] %s — %d step(s) planned",
			plan.ThoughtSummary, len(plan.Steps)),
		AgentRole:  RolePlanner,
		TokenUsage: plan.TokenUsage,
	})
	task.StepCount++
	task.Status = types.StatusRunning

	log.Info("Phase 1 done", "steps_planned", len(plan.Steps), "elapsed", elapsed)
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
	neutral := plancritic.Plan{Summary: plan.ThoughtSummary, Steps: make([]plancritic.Step, 0, len(plan.Steps))}
	for _, step := range plan.Steps {
		neutral.Steps = append(neutral.Steps, plancritic.Step{Action: step.Action, Description: step.Description, Parameters: stepToParams(step)})
	}
	if !required && !plancritic.ShouldCritique(task, neutral) {
		return nil, nil
	}
	fingerprint := plancritic.Fingerprint(neutral)
	if !required && plancritic.AlreadyCritiqued(task, fingerprint) {
		return &plancritic.Result{Approved: true, Summary: "plan already reviewed"}, nil
	}
	result, usage, err := c.PlanCritic.Critique(ctx, task, neutral)
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
	}
	return result, err
}

func (c *Coordinator) requireCriticApproval(ctx context.Context, task *types.Task, plan *ResearchPlan) (*ResearchPlan, error) {
	result, err := c.reviewResearchPlan(ctx, task, plan, true, true)
	if err != nil {
		return nil, fmt.Errorf("CriticAgent: %w", err)
	}
	if result != nil && result.Approved {
		return plan, nil
	}

	revised, err := c.Planner.Replan(ctx, task.Goal, task.Workspace, task.Trace, task.Memories)
	if err != nil {
		return nil, fmt.Errorf("CriticAgent rejected plan and replanning failed: %w", err)
	}
	if revised == nil || len(revised.Steps) == 0 {
		return nil, fmt.Errorf("CriticAgent rejected plan and replanning returned no executable steps")
	}
	enforceJITResearchPlan(task, revised)
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

	result, err = c.reviewResearchPlan(ctx, task, revised, true, true)
	if err != nil {
		return nil, fmt.Errorf("CriticAgent: %w", err)
	}
	if result == nil || !result.Approved {
		return nil, fmt.Errorf("CriticAgent rejected the revised plan")
	}
	return revised, nil
}

func (c *Coordinator) runResearchPhase(ctx context.Context, task *types.Task, steps []ResearchStep) []StepEvidence {
	log.Info("Phase 2 — Researching", "task_id", task.ID)

	var allEvidence []StepEvidence

	currentSteps := make([]ResearchStep, len(steps))
	copy(currentSteps, steps)

	replansCount := 0
	maxReplans := 3

	for len(currentSteps) > 0 {
		// Budget and step-count gate
		if task.ToolBudget <= 0 || tokenBudgetExhausted(task) {
			log.Info("Budget exhausted (tools or tokens) — stopping research early")
			break
		}
		if task.StepCount >= task.MaxSteps {
			log.Info("Max steps reached — stopping research early")
			break
		}

		// Context cancellation check
		select {
		case <-ctx.Done():
			log.Info("Context cancelled during research phase")
			return allEvidence
		default:
		}

		// Partition: collect a batch of parallelisable (read-only) steps at the front,
		// or fall back to a single serial step.
		batch, remainder, isParallel := partitionBatch(currentSteps, task.ToolBudget, task.MaxSteps-task.StepCount)
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

		if isParallel && len(batch) > 1 {
			log.Info("Executing read-only steps in parallel", "count", len(batch))
			batchEvidence, anyFailed = c.runBatchParallel(ctx, task, batch)
		} else {
			log.Info("Executing steps serially", "count", len(batch))
			batchEvidence, anyFailed = c.runBatchSerial(ctx, task, batch)
		}

		allEvidence = append(allEvidence, batchEvidence...)
		if followups := retrievalFetchSteps(batchEvidence); len(followups) > 0 {
			// Search candidates are intentionally compact. Fetch selected details
			// before unrelated remaining work so the Writer receives real evidence,
			// not just candidate snippets.
			currentSteps = append(followups, currentSteps...)
		}

		// Trigger re-planning if any step in the batch failed
		if anyFailed && replansCount < maxReplans {
			replansCount++
			log.Info("Triggering collaborative replan/error-correction loop", "replan_count", replansCount)

			newPlan, replanErr := c.Planner.Replan(ctx, task.Goal, task.Workspace, task.Trace, task.Memories)
			if replanErr != nil {
				log.Error("Replanner failed — continuing with remaining steps", "error", replanErr)
			} else if len(newPlan.Steps) > 0 {
				enforceJITResearchPlan(task, newPlan)
				log.Info("Replanner generated revised steps", "count", len(newPlan.Steps))
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
						log.Error("Revised plan rejected by critic", "error", replanErr)
						task.Status = types.StatusFailed
						return allEvidence
					}
				} else {
					c.critiqueResearchPlan(ctx, task, newPlan)
				}
				currentSteps = newPlan.Steps
				continue
			}
		}
	}

	log.Info("Phase 2 done", "evidence_items", len(allEvidence))
	return allEvidence
}

// isReadOnlyAction returns true for actions that are safe to run concurrently
// in a parallel batch: they must not mutate the workspace AND must not be
// high-risk in the tool registry.
//
// The registry RiskLevel check is the authoritative guard. The parallel batch
// path (runBatchParallel) does NOT perform approval gating — only the serial
// path (runBatchSerial) calls SuspendForApproval. Previously this function
// relied solely on a hardcoded read-only name list, so if a high-risk tool were
// ever (mis)classified as read-only, it would be executed in the parallel batch
// and silently bypass approval. By rejecting any RiskLevelHigh tool here, such a
// tool is forced onto the serial path where approval is enforced — closing the
// bypass regardless of how the name list evolves.
func isReadOnlyAction(action string) bool {
	if tool, ok := tools.Get(action); ok && tool.RiskLevel() == types.RiskLevelHigh {
		return false
	}
	switch action {
	case "find_files", "search_text", "read_file", "git_diff", "http_fetch", "web_search", "rag_search", "rag_fetch", "memory_search", "memory_get":
		return true
	}
	return false
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

func retrievalFetchSteps(evidence []StepEvidence) []ResearchStep {
	limit := config.Get().RAG.JITFetchMaxItems
	if limit <= 0 {
		limit = 3
	}
	steps := make([]ResearchStep, 0, len(evidence))
	for _, item := range evidence {
		if item.Failed || (item.Action != "rag_search" && item.Action != "memory_search") {
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
	if !isReadOnlyAction(steps[0].Action) {
		return steps[:1], steps[1:], false
	}
	// Collect consecutive read-only steps up to budget/step limits
	end := 0
	for end < len(steps) && isReadOnlyAction(steps[end].Action) && end < budgetLeft && end < stepsLeft {
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
	baseStep := task.StepCount
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
				Step:        baseStep + idx,
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
		task.Trace = append(task.Trace, r.tr)
		if audit != nil {
			task.Trace = append(task.Trace, *audit)
		}
		if relevanceAudit != nil {
			task.Trace = append(task.Trace, *relevanceAudit)
		}
		if r.ev != nil && !r.failed {
			evidence = append(evidence, *r.ev)
		}
		if r.failed {
			anyFailed = true
		}
	}
	task.StepCount += len(batch)
	task.ToolBudget -= len(batch)
	if c.EventCallback != nil {
		c.EventCallback(task.ID, task.Status)
	}
	task.Status = types.StatusRunning
	return
}

// runBatchSerial executes steps one at a time (used for write/execute steps).
func (c *Coordinator) runBatchSerial(ctx context.Context, task *types.Task, batch []ResearchStep) (evidence []StepEvidence, anyFailed bool) {
	agentRole, agentLabel := executionTraceIdentity(ctx)
	for _, step := range batch {
		if task.ToolBudget <= 0 || task.StepCount >= task.MaxSteps {
			break
		}
		select {
		case <-ctx.Done():
			return evidence, anyFailed
		default:
		}

		log.Info("Executing research step", "step_num", task.StepCount+1, "step_id", step.ID, "action", step.Action, "desc", step.Description)

		tool, ok := tools.Get(step.Action)
		if ok && tool.RiskLevel() == types.RiskLevelHigh && c.SuspendForApproval != nil {
			approved, newParams, err := c.SuspendForApproval(ctx, task, step.Action, stepToParams(step))
			if err != nil {
				log.Error("Action approval error", "action", step.Action, "error", err)
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
		if audit != nil {
			task.Trace = append(task.Trace, *audit)
		}
		if relevanceAudit != nil {
			task.Trace = append(task.Trace, *relevanceAudit)
		}

		if ev != nil && !failed {
			evidence = append(evidence, *ev)
		}
		if failed {
			anyFailed = true
		}

		task.StepCount++
		task.ToolBudget--
		task.Status = types.StatusRunning
	}
	return
}

func (c *Coordinator) runVerifyPhase(ctx context.Context, task *types.Task, evidence []StepEvidence) (string, error) {
	log.Info("Phase 3 — Verifying execution result", "task_id", task.ID)
	if c.FinalVerifier == nil {
		err := fmt.Errorf("reviewed workflow requires a final Verifier")
		task.Status = types.StatusFailed
		return "low", err
	}

	start := time.Now()
	output, err := c.FinalVerifier.Finalize(ctx, task.Goal, evidence, task.Memories)
	elapsed := time.Since(start)
	if err != nil {
		log.Error("VerifierAgent failed", "task_id", task.ID, "error", err)
		task.Trace = append(task.Trace, types.StepTrace{
			Step:        task.StepCount,
			Goal:        task.Goal,
			Action:      "verify",
			Query:       "verifier",
			Observation: fmt.Sprintf("[verifier] final verification error: %v", err),
			Error:       err.Error(),
			AgentRole:   RoleVerifier,
		})
		task.StepCount++
		task.Status = types.StatusFailed
		task.FinalAnswer = "Execution completed but final verification failed. See trace for gathered evidence."
		return "low", err
	}
	if c.Metrics != nil {
		c.Metrics.ObserveTokens(output.TokenUsage.PromptTokens, output.TokenUsage.CompletionTokens, output.TokenUsage.TotalTokens, "verifier")
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
		TokenUsage:  output.TokenUsage,
	})
	task.StepCount++
	task.FinalAnswer = output.FinalAnswer
	log.Info("Phase 3 done — result verified", "supported", output.Supported, "draft_confidence", confidence, "elapsed", elapsed)
	return confidence, nil
}

func (c *Coordinator) runWritePhase(ctx context.Context, task *types.Task, evidence []StepEvidence) (string, error) {
	log.Info("Phase 3 — Writing final answer", "task_id", task.ID)

	start := time.Now()
	output, err := c.Writer.Write(ctx, task.Goal, evidence, task.Memories)
	elapsed := time.Since(start)

	if c.Metrics != nil {
		c.Metrics.ObserveWriter(elapsed, err)
	}

	if err != nil {
		log.Error("WriterAgent failed — marking task failed with best-effort summary", "task_id", task.ID, "error", err)
		// The synthesis step failed. Preserve the gathered evidence as a
		// best-effort answer for callers, but mark the task FAILED so the error
		// is not masked as a successful completion. Previously this set
		// StatusCompleted, which made writer errors indistinguishable from a
		// genuine success at the API/status layer.
		fallback := "Research complete but synthesis failed. See trace for gathered evidence."
		task.Trace = append(task.Trace, types.StepTrace{
			Step:        task.StepCount,
			Goal:        task.Goal,
			Action:      "write",
			Query:       "writer",
			Observation: fmt.Sprintf("[writer] synthesis error: %v", err),
			Error:       err.Error(),
			AgentRole:   RoleWriter,
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

	log.Info("Phase 3 done — draft written", "draft_confidence", draftConfidence, "elapsed", elapsed)
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
	case "rag_search", "memory_search":
		return fmt.Sprintf("query=%q", step.SearchQuery)
	case "rag_fetch", "memory_get":
		if ids, ok := step.RepairedParameters["ids"]; ok {
			return fmt.Sprintf("ids=%v", ids)
		}
		return "ids=[]"
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
}
