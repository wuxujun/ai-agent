package multiagent

import (
	"context"
	"errors"
	"fmt"
	"time"

	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/types"
)

type reviewedDAGSynthesisPayload struct {
	DraftConfidence   string           `json:"draft_confidence"`
	Supported         bool             `json:"supported"`
	ExecutionComplete bool             `json:"execution_complete"`
	ExecutionReason   string           `json:"execution_reason,omitempty"`
	Status            types.TaskStatus `json:"status"`
}

// reviewedDAGStatusError marks failures whose legacy contract is represented
// by Task.Status and Task.FinalAnswer rather than a Coordinator error.
type reviewedDAGStatusError struct{ err error }

func (e *reviewedDAGStatusError) Error() string { return e.err.Error() }
func (e *reviewedDAGStatusError) Unwrap() error { return e.err }

func (c *Coordinator) runReviewedWorkflowDAG(ctx context.Context, task *types.Task, teamCfg TeamConfig) error {
	return c.runReviewedWorkflowDAGFromPlan(ctx, task, teamCfg, nil)
}

func (c *Coordinator) runReviewedWorkflowDAGFromPlan(ctx context.Context, task *types.Task, teamCfg TeamConfig, initialPlan *ResearchPlan) error {
	graph, err := BuildWorkflowGraph(WorkflowReviewed)
	if err != nil {
		return err
	}
	summary, err := graph.Summary()
	if err != nil {
		return err
	}
	teamSnapshot := teamConfigFromContext(ctx)
	var checkpoint *WorkflowRuntimeCheckpoint
	if persisted, ok := persistedWorkflowRuntimeCheckpoint(task, summary.Digest, WorkflowReviewed, teamSnapshot.Digest); ok {
		checkpoint = &persisted
	} else if initialPlan != nil {
		planResult, marshalErr := marshalWorkflowNodeResult(newResearchDAGPlanPayload(initialPlan))
		if marshalErr != nil {
			return marshalErr
		}
		checkpoint = &WorkflowRuntimeCheckpoint{
			Version: workflowRuntimeCheckpointVersion, Workflow: WorkflowReviewed, Route: WorkflowReviewed,
			GraphDigest: summary.Digest, ActiveTeam: teamSnapshot.ActiveTeam, TeamDigest: teamSnapshot.Digest,
			States:  map[string]WorkflowNodeState{"plan": WorkflowNodeSucceeded},
			Results: map[string]WorkflowNodeResult{"plan": planResult},
		}
	}
	runtime := WorkflowRuntime{
		MaxConcurrency: 1,
		SaveCheckpoint: func(current WorkflowRuntimeCheckpoint) error {
			current.ActiveTeam = teamSnapshot.ActiveTeam
			current.TeamDigest = teamSnapshot.Digest
			if err := upsertWorkflowRuntimeCheckpoint(task, current); err != nil {
				return err
			}
			return c.persistTaskDetached(task)
		},
	}
	_, err = runtime.Run(ctx, graph, WorkflowReviewed, checkpoint, func(nodeCtx context.Context, node WorkflowGraphNode, dependencies map[string]WorkflowNodeResult) (WorkflowNodeResult, error) {
		switch node.ID {
		case "plan":
			plan, planErr := c.runPlanPhase(nodeCtx, task)
			if planErr != nil {
				return WorkflowNodeResult{}, planErr
			}
			return marshalWorkflowNodeResult(newResearchDAGPlanPayload(plan))
		case "critique":
			var payload researchDAGPlanPayload
			if decodeErr := decodeWorkflowNodeResult(dependencies["plan"], &payload); decodeErr != nil {
				return WorkflowNodeResult{}, decodeErr
			}
			plan := payload.restorePlan()
			approvedPlan, criticErr := c.requireCriticApproval(nodeCtx, task, &plan)
			if criticErr != nil {
				task.Status = types.StatusFailed
				return WorkflowNodeResult{}, criticErr
			}
			if c.EventCallback != nil {
				c.EventCallback(task.ID, task.Status)
			}
			result, marshalErr := marshalWorkflowNodeResult(newResearchDAGPlanPayload(approvedPlan))
			if marshalErr != nil {
				return WorkflowNodeResult{}, marshalErr
			}
			approved := true
			result.Approved = &approved
			return result, nil
		case "execute":
			var payload researchDAGPlanPayload
			if decodeErr := decodeWorkflowNodeResult(dependencies["critique"], &payload); decodeErr != nil {
				return WorkflowNodeResult{}, decodeErr
			}
			plan := payload.restorePlan()
			result := c.runResearchPhase(nodeCtx, task, plan.Steps, WorkflowReviewed, teamCfg.Routing)
			if result.Err != nil {
				task.Status = types.StatusFailed
				return WorkflowNodeResult{}, result.Err
			}
			return marshalWorkflowNodeResult(researchDAGEvidencePayload{
				Evidence: result.Evidence, ExecutionComplete: result.Complete, ExecutionReason: result.Reason,
			})
		case "verify":
			var payload researchDAGEvidencePayload
			if decodeErr := decodeWorkflowNodeResult(dependencies["execute"], &payload); decodeErr != nil {
				return WorkflowNodeResult{}, decodeErr
			}
			result, verifyErr := c.runReviewedDAGVerification(nodeCtx, task, teamCfg.Routing, payload)
			if verifyErr != nil {
				return WorkflowNodeResult{}, verifyErr
			}
			return marshalWorkflowNodeResult(result)
		default:
			return WorkflowNodeResult{}, fmt.Errorf("unsupported reviewed DAG node %q", node.ID)
		}
	})
	if err != nil {
		var statusErr *reviewedDAGStatusError
		if errors.As(err, &statusErr) {
			return nil
		}
		return err
	}
	return nil
}

func (c *Coordinator) runReviewedDAGVerification(ctx context.Context, task *types.Task, routing WorkflowRoutingConfig, initial researchDAGEvidencePayload) (reviewedDAGSynthesisPayload, error) {
	allEvidence := append([]StepEvidence(nil), initial.Evidence...)
	executionComplete := initial.ExecutionComplete
	executionReason := initial.ExecutionReason
	draftConfidence := "low"
	supported := false
	depthIterations := 0
	const maxDepthIterations = 2

	for {
		phaseCtx := llmcore.WithTaskRoutingHints(ctx, task)
		verifierEvidence := append([]StepEvidence(nil), allEvidence...)
		if annotation := c.resolveEvidenceConflicts(phaseCtx, task, allEvidence); annotation != nil {
			verifierEvidence = append(verifierEvidence, *annotation)
		}
		if c.ResolveMemoryConflicts != nil {
			c.ResolveMemoryConflicts(phaseCtx, task)
			phaseCtx = llmcore.WithTaskRoutingHints(ctx, task)
		}
		var verifyErr error
		draftConfidence, supported, verifyErr = c.runVerifyPhase(phaseCtx, task, verifierEvidence, executionComplete, executionReason)
		if verifyErr != nil {
			if HasPendingVerifierDraft(task) {
				task.Status = types.StatusPartial
				appendUnresolvedReason(task, verifierRetryReason)
				if c.Metrics != nil {
					c.Metrics.IncCompleted()
				}
			}
			return reviewedDAGSynthesisPayload{
				DraftConfidence: draftConfidence, Supported: supported, ExecutionComplete: executionComplete,
				ExecutionReason: executionReason, Status: task.Status,
			}, &reviewedDAGStatusError{err: verifyErr}
		}
		if draftConfidence != "low" || depthIterations >= maxDepthIterations || task.ToolBudget <= 0 || multiAgentToolStepCount(task) >= task.MaxSteps || tokenBudgetExhausted(task) {
			break
		}

		depthIterations++
		log.Info("DAG verifier requested adaptive execution depth", "task_id", task.ID, "iteration", depthIterations, "max_iterations", maxDepthIterations)
		task.Trace = append(task.Trace, types.StepTrace{
			Step: task.StepCount, Goal: task.Goal, Action: "plan", Query: "adaptive_depth",
			Observation: "[coordinator] draft confidence was low; requesting additional steps for deeper investigation",
			AgentRole:   RolePlanner,
		})
		task.StepCount++

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
		if replanErr != nil || newPlan == nil || len(newPlan.Steps) == 0 {
			log.Error("DAG reviewed replan failed or returned empty steps", "error", replanErr)
			break
		}
		enforceJITResearchPlan(task, newPlan)
		if c.Metrics != nil {
			c.Metrics.ObserveTokens(newPlan.TokenUsage.PromptTokens, newPlan.TokenUsage.CompletionTokens, newPlan.TokenUsage.TotalTokens, "replanner")
		}
		task.Trace = append(task.Trace, types.StepTrace{
			Step: task.StepCount, Goal: task.Goal, Action: "plan", Query: "replanner",
			Observation: fmt.Sprintf("[replanner] %s — %d replacement step(s) planned", newPlan.ThoughtSummary, len(newPlan.Steps)),
			AgentRole:   RolePlanner, TokenUsage: newPlan.TokenUsage,
		})
		task.StepCount++
		newPlan, replanErr = c.requireCriticApproval(replanCtx, task, newPlan)
		if replanErr != nil {
			task.Status = types.StatusFailed
			return reviewedDAGSynthesisPayload{DraftConfidence: draftConfidence, Supported: false, ExecutionComplete: false, ExecutionReason: "critic_rejected_recovery_plan", Status: task.Status}, &reviewedDAGStatusError{err: replanErr}
		}
		task.Status = types.StatusRunning
		if c.EventCallback != nil {
			c.EventCallback(task.ID, task.Status)
		}
		execution := c.runResearchPhase(ctx, task, newPlan.Steps, WorkflowReviewed, routing)
		allEvidence = append(allEvidence, execution.Evidence...)
		if execution.Err != nil {
			task.Status = types.StatusFailed
			return reviewedDAGSynthesisPayload{DraftConfidence: draftConfidence, Supported: false, ExecutionComplete: false, ExecutionReason: execution.Reason, Status: task.Status}, execution.Err
		}
		if !execution.Complete {
			executionComplete = false
			if executionReason == "" {
				executionReason = execution.Reason
			}
		}
	}

	if task.Status != types.StatusFailed {
		if executionComplete && supported {
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
	return reviewedDAGSynthesisPayload{
		DraftConfidence: draftConfidence, Supported: supported, ExecutionComplete: executionComplete,
		ExecutionReason: executionReason, Status: task.Status,
	}, nil
}

func (c *Coordinator) completeReviewedDAGVerifierCheckpoint(ctx context.Context, task *types.Task) error {
	graph, err := BuildWorkflowGraph(WorkflowReviewed)
	if err != nil {
		return err
	}
	summary, err := graph.Summary()
	if err != nil {
		return err
	}
	teamSnapshot := teamConfigFromContext(ctx)
	checkpoint, ok := persistedWorkflowRuntimeCheckpoint(task, summary.Digest, WorkflowReviewed, teamSnapshot.Digest)
	if !ok {
		return nil
	}
	checkpoint.States["verify"] = WorkflowNodeSucceeded
	delete(checkpoint.Errors, "verify")
	payload := reviewedDAGSynthesisPayload{Supported: task.Status == types.StatusCompleted, Status: task.Status}
	result, err := marshalWorkflowNodeResult(payload)
	if err != nil {
		return err
	}
	if checkpoint.Results == nil {
		checkpoint.Results = make(map[string]WorkflowNodeResult)
	}
	checkpoint.Results["verify"] = result
	checkpoint.ActiveTeam = teamSnapshot.ActiveTeam
	checkpoint.TeamDigest = teamSnapshot.Digest
	if err := upsertWorkflowRuntimeCheckpoint(task, checkpoint); err != nil {
		return err
	}
	if c.PersistTask != nil {
		return c.PersistTask(ctx, task)
	}
	return nil
}
