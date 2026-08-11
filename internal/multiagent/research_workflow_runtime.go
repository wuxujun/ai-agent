package multiagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/types"
)

type researchDAGPlanPayload struct {
	Plan               ResearchPlan     `json:"plan"`
	RepairedParameters []map[string]any `json:"repaired_parameters,omitempty"`
}

type researchDAGEvidencePayload struct {
	Evidence          []StepEvidence `json:"evidence"`
	ExecutionComplete bool           `json:"execution_complete"`
	ExecutionReason   string         `json:"execution_reason,omitempty"`
}

type researchDAGSynthesisPayload struct {
	DraftConfidence   string           `json:"draft_confidence"`
	ExecutionComplete bool             `json:"execution_complete"`
	ExecutionReason   string           `json:"execution_reason,omitempty"`
	Status            types.TaskStatus `json:"status"`
}

func (c *Coordinator) runResearchWorkflowDAG(ctx context.Context, task *types.Task, teamCfg TeamConfig) error {
	return c.runResearchWorkflowDAGFromPlan(ctx, task, teamCfg, nil, WorkflowResearch)
}

func (c *Coordinator) runResearchWorkflowDAGFromPlan(ctx context.Context, task *types.Task, teamCfg TeamConfig, initialPlan *ResearchPlan, configuredWorkflow Workflow) error {
	graph, err := BuildWorkflowGraph(WorkflowResearch)
	if err != nil {
		return err
	}
	summary, err := graph.Summary()
	if err != nil {
		return err
	}
	var checkpoint *WorkflowRuntimeCheckpoint
	teamSnapshot := teamConfigFromContext(ctx)
	if persisted, ok := persistedWorkflowRuntimeCheckpoint(task, summary.Digest, WorkflowResearch, teamSnapshot.Digest); ok {
		checkpoint = &persisted
	} else if initialPlan != nil {
		planResult, marshalErr := marshalWorkflowNodeResult(newResearchDAGPlanPayload(initialPlan))
		if marshalErr != nil {
			return marshalErr
		}
		checkpoint = &WorkflowRuntimeCheckpoint{
			Version: workflowRuntimeCheckpointVersion, Workflow: WorkflowResearch, Route: WorkflowResearch,
			GraphDigest: summary.Digest, ActiveTeam: teamSnapshot.ActiveTeam, TeamDigest: teamSnapshot.Digest,
			States:  map[string]WorkflowNodeState{"plan": WorkflowNodeSucceeded},
			Results: map[string]WorkflowNodeResult{"plan": planResult},
		}
	}
	runtime := WorkflowRuntime{
		MaxConcurrency: 1,
		SaveCheckpoint: func(current WorkflowRuntimeCheckpoint) error {
			teamSnapshot := teamConfigFromContext(ctx)
			current.ActiveTeam = teamSnapshot.ActiveTeam
			current.TeamDigest = teamSnapshot.Digest
			if err := upsertWorkflowRuntimeCheckpoint(task, current); err != nil {
				return err
			}
			return c.persistTaskDetached(task)
		},
	}
	_, err = runtime.Run(ctx, graph, WorkflowResearch, checkpoint, func(nodeCtx context.Context, node WorkflowGraphNode, dependencies map[string]WorkflowNodeResult) (WorkflowNodeResult, error) {
		switch node.ID {
		case "plan":
			plan, planErr := c.runPlanPhase(nodeCtx, task)
			if planErr != nil {
				return WorkflowNodeResult{}, planErr
			}
			c.critiqueResearchPlan(nodeCtx, task, plan)
			if c.EventCallback != nil {
				c.EventCallback(task.ID, task.Status)
			}
			return marshalWorkflowNodeResult(newResearchDAGPlanPayload(plan))
		case "research":
			var payload researchDAGPlanPayload
			if decodeErr := decodeWorkflowNodeResult(dependencies["plan"], &payload); decodeErr != nil {
				return WorkflowNodeResult{}, decodeErr
			}
			plan := payload.restorePlan()
			result := c.runResearchPhase(nodeCtx, task, plan.Steps, configuredWorkflow, teamCfg.Routing)
			if result.Err != nil {
				task.Status = types.StatusFailed
				return WorkflowNodeResult{}, result.Err
			}
			if result.Workflow == WorkflowReviewed {
				return WorkflowNodeResult{}, &adaptiveDAGEscalationError{reason: "research_replan_escalated_to_reviewed"}
			}
			return marshalWorkflowNodeResult(researchDAGEvidencePayload{
				Evidence: result.Evidence, ExecutionComplete: result.Complete, ExecutionReason: result.Reason,
			})
		case "write":
			var payload researchDAGEvidencePayload
			if decodeErr := decodeWorkflowNodeResult(dependencies["research"], &payload); decodeErr != nil {
				return WorkflowNodeResult{}, decodeErr
			}
			result, writeErr := c.runResearchDAGSynthesis(nodeCtx, task, teamCfg.Routing, configuredWorkflow, payload)
			if writeErr != nil {
				return WorkflowNodeResult{}, writeErr
			}
			return marshalWorkflowNodeResult(result)
		default:
			return WorkflowNodeResult{}, fmt.Errorf("unsupported research DAG node %q", node.ID)
		}
	})
	if err != nil {
		var nodeErr *WorkflowNodeError
		if errors.As(err, &nodeErr) && nodeErr.NodeID == "write" && task.Status == types.StatusFailed {
			// Preserve the legacy contract: Writer failure is represented by task
			// status and fallback answer rather than a Coordinator error.
			return nil
		}
		return err
	}
	return nil
}

func (c *Coordinator) runResearchDAGSynthesis(ctx context.Context, task *types.Task, routing WorkflowRoutingConfig, configuredWorkflow Workflow, initial researchDAGEvidencePayload) (researchDAGSynthesisPayload, error) {
	allEvidence := append([]StepEvidence(nil), initial.Evidence...)
	executionComplete := initial.ExecutionComplete
	executionReason := initial.ExecutionReason
	finalSufficient := false
	draftConfidence := "low"
	depthIterations := 0
	const maxDepthIterations = 2

	for {
		phaseCtx := llm.WithTaskRoutingHints(ctx, task)
		writerEvidence := append([]StepEvidence(nil), allEvidence...)
		if annotation := c.resolveEvidenceConflicts(phaseCtx, task, allEvidence); annotation != nil {
			writerEvidence = append(writerEvidence, *annotation)
		}
		if c.ResolveMemoryConflicts != nil {
			c.ResolveMemoryConflicts(phaseCtx, task)
			phaseCtx = llm.WithTaskRoutingHints(ctx, task)
		}
		var writeErr error
		draftConfidence, writeErr = c.runWritePhase(phaseCtx, task, writerEvidence)
		finalSufficient = draftConfidence != "low"
		if writeErr != nil {
			return researchDAGSynthesisPayload{DraftConfidence: draftConfidence, ExecutionComplete: executionComplete, ExecutionReason: executionReason, Status: task.Status}, writeErr
		}
		if draftConfidence != "low" || depthIterations >= maxDepthIterations || task.ToolBudget <= 0 || multiAgentToolStepCount(task) >= task.MaxSteps || tokenBudgetExhausted(task) {
			break
		}

		depthIterations++
		log.Info("DAG writer requested adaptive research depth", "task_id", task.ID, "iteration", depthIterations, "max_iterations", maxDepthIterations)
		task.Trace = append(task.Trace, types.StepTrace{
			Step: task.StepCount, Goal: task.Goal, Action: "plan", Query: "adaptive_depth",
			Observation: "[coordinator] draft confidence was low; requesting additional steps for deeper investigation",
			AgentRole:   RolePlanner,
		})
		task.StepCount++

		replanCtx := llm.WithTaskRoutingHints(ctx, task)
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
			log.Error("DAG adaptive replan failed or returned empty steps", "error", replanErr)
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
		c.critiqueResearchPlan(replanCtx, task, newPlan)
		task.Status = types.StatusRunning
		if c.EventCallback != nil {
			c.EventCallback(task.ID, task.Status)
		}

		researchResult := c.runResearchPhase(ctx, task, newPlan.Steps, configuredWorkflow, routing)
		allEvidence = append(allEvidence, researchResult.Evidence...)
		if researchResult.Err != nil {
			task.Status = types.StatusFailed
			return researchDAGSynthesisPayload{DraftConfidence: draftConfidence, ExecutionComplete: false, ExecutionReason: researchResult.Reason, Status: task.Status}, researchResult.Err
		}
		if researchResult.Workflow == WorkflowReviewed {
			return researchDAGSynthesisPayload{DraftConfidence: draftConfidence, ExecutionComplete: false, ExecutionReason: "research_replan_escalated_to_reviewed", Status: task.Status}, &adaptiveDAGEscalationError{reason: "research_replan_escalated_to_reviewed"}
		}
		if !researchResult.Complete {
			executionComplete = false
			if executionReason == "" {
				executionReason = researchResult.Reason
			}
		}
	}

	if task.Status != types.StatusFailed {
		if executionComplete && finalSufficient {
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
	return researchDAGSynthesisPayload{
		DraftConfidence: draftConfidence, ExecutionComplete: executionComplete,
		ExecutionReason: executionReason, Status: task.Status,
	}, nil
}

func newResearchDAGPlanPayload(plan *ResearchPlan) researchDAGPlanPayload {
	payload := researchDAGPlanPayload{Plan: *plan, RepairedParameters: make([]map[string]any, len(plan.Steps))}
	for i, step := range plan.Steps {
		if step.RepairedParameters != nil {
			payload.RepairedParameters[i] = step.RepairedParameters
		}
	}
	return payload
}

func (p researchDAGPlanPayload) restorePlan() ResearchPlan {
	plan := p.Plan
	for i := range plan.Steps {
		if i < len(p.RepairedParameters) {
			plan.Steps[i].RepairedParameters = p.RepairedParameters[i]
		}
	}
	return plan
}

func marshalWorkflowNodeResult(value any) (WorkflowNodeResult, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return WorkflowNodeResult{}, fmt.Errorf("marshal workflow node result: %w", err)
	}
	return WorkflowNodeResult{Data: encoded}, nil
}

func decodeWorkflowNodeResult(result WorkflowNodeResult, target any) error {
	if len(result.Data) == 0 {
		return fmt.Errorf("workflow dependency result is empty")
	}
	if err := json.Unmarshal(result.Data, target); err != nil {
		return fmt.Errorf("decode workflow dependency result: %w", err)
	}
	return nil
}
