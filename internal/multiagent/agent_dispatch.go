package multiagent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/evidenceconflict"
	"github.com/wuxujun/ai-agent/internal/evidencefilter"
	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/metrics"
	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/promptguard"
	"github.com/wuxujun/ai-agent/internal/sourcecredibility"
	"github.com/wuxujun/ai-agent/internal/types"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

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

func teamsConfigForTask(task *types.Task) (*TeamsConfig, error) {
	config := GetTeamsConfig()
	if task == nil || strings.TrimSpace(task.Team) == "" {
		return config, nil
	}
	selected := strings.TrimSpace(task.Team)
	if _, ok := config.Teams[selected]; !ok {
		return nil, fmt.Errorf("multi-agent team %q is not configured", selected)
	}
	config.ActiveTeam = selected
	return config, nil
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

func (c *Coordinator) runWritePhase(ctx context.Context, task *types.Task, evidence []StepEvidence) (string, error) {
	log := teamLogger(ctx)
	log.Info("Phase 3 — Writing final answer", "task_id", task.ID)
	evidence = writerEvidenceForTeam(ctx, evidence)

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

func writerEvidenceForTeam(ctx context.Context, evidence []StepEvidence) []StepEvidence {
	if teamConfigFromContext(ctx).ActiveTeam != "wiki_suggest" {
		return evidence
	}
	result := make([]StepEvidence, 0, len(evidence))
	for _, item := range evidence {
		if item.Action == "wiki_suggest" {
			item.Observation = fmt.Sprintf("retained %d read-only Wiki suggestion(s) after safety and relevance filtering", len(item.Evidence))
			result = append(result, item)
		}
	}
	return result
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
