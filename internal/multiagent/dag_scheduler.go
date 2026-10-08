package multiagent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/wuxujun/ai-agent/internal/tools"
	"github.com/wuxujun/ai-agent/internal/types"
)

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

const maxBatchConcurrency = 5

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
	jobs := make(chan int)
	for worker := 0; worker < min(maxBatchConcurrency, len(batch)); worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Each worker owns disjoint result indices; no task state is mutated
			// until all workers finish. Avoid a goroutine for every queued step.
			for idx := range jobs {
				s := batch[idx]
				start := time.Now()
				var ev *StepEvidence
				err := ctx.Err()
				executed := err == nil
				if executed {
					ev, err = c.executeWorkflowStep(ctx, task.Workspace, s)
				}
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
				if executed {
					tr.SetExecutionTiming(start, elapsed)
				}

				results[idx] = result{
					ev:      ev,
					tr:      tr,
					failed:  failed,
					elapsed: elapsed,
					action:  s.Action,
					err:     err,
				}
			}
		}()
	}
	for idx := range batch {
		jobs <- idx
	}
	close(jobs)
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

		trace := types.StepTrace{
			Step:        task.StepCount,
			Goal:        task.Goal,
			Action:      step.Action,
			Query:       buildStepQuery(step),
			Observation: obs,
			Evidence:    trEvidence,
			TokenUsage:  tokenUsage,
			AgentRole:   agentRole,
		}
		trace.SetExecutionTiming(start, elapsed)
		task.Trace = append(task.Trace, trace)
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
