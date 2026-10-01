package multiagent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/wuxujun/ai-agent/internal/config"
	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/plancritic"
	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/types"
)

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
