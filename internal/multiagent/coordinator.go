package multiagent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/wuxujun/ai-agent/internal/config"
	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/logger"
	"github.com/wuxujun/ai-agent/internal/metrics"
	"github.com/wuxujun/ai-agent/internal/planner"
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

type Writer interface {
	Write(ctx context.Context, goal string, evidence []StepEvidence, memories []types.Memory) (*WriterOutput, error)
}

// Coordinator orchestrates the three agents in sequence:
//
//	PlannerAgent → ResearcherAgent (×N) → WriterAgent
//
// It updates task.Trace and task.Status in-place.
type Coordinator struct {
	Planner            Planner
	Researcher         Researcher
	Writer             Writer
	Verifier           AnswerVerifier
	Metrics            *metrics.Collector
	SuspendForApproval func(ctx context.Context, task *types.Task, action string, params map[string]any) (bool, map[string]any, error)
	EventCallback      func(taskID string, status types.TaskStatus)
}

// NewCoordinator creates a Coordinator wired to the default LLM configuration
// derived from environment variables (same vars as the main planner).
func NewCoordinator(mc *metrics.Collector) *Coordinator {
	return &Coordinator{
		Planner:    &PlannerAgent{ArgumentRepairer: planner.NewLLMToolArgumentRepairer(config.LLMSceneToolArgumentRepair)},
		Researcher: &ResearcherAgent{},
		Writer:     &WriterAgent{},
		Metrics:    mc,
	}
}

// Run executes the full multi-agent workflow for the given task, updating
// task.Trace, task.StepCount, task.ToolBudget, and task.Status in-place.
//
// The workflow has three phases:
//  1. Plan   – PlannerAgent decomposes the goal into ResearchSteps
//  2. Research – ResearcherAgent executes each step (budget-gated)
//  3. Write  – WriterAgent synthesises all evidence into a final answer
func (c *Coordinator) Run(ctx context.Context, task *types.Task) error {
	ctx = llmcore.WithTaskBudget(ctx, task)
	ctx = llmcore.WithTaskRoutingHints(ctx, task)
	ctx, span := tracer.Start(ctx, "multiagent.coordinator.run")
	defer span.End()

	span.SetAttributes(
		attribute.String("agent.task.id", task.ID),
		attribute.String("agent.task.goal", task.Goal),
	)

	teamsCfg := GetTeamsConfig()
	log.Info("Starting multi-agent workflow", "task_id", task.ID, "goal", task.Goal, "active_team", teamsCfg.ActiveTeam)

	// ── Phase 1: Plan ──────────────────────────────────────────────────────────
	plan, err := c.runPlanPhase(ctx, task)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "plan phase failed")
		return err
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
		// ── Phase 2: Research ──────────────────────────────────────────────────────
		evidenceBatch := c.runResearchPhase(ctx, task, currentSteps)
		allEvidence = append(allEvidence, evidenceBatch...)
		span.SetAttributes(attribute.Int("multiagent.research.evidence_items", len(allEvidence)))

		select {
		case <-ctx.Done():
			log.Info("Context cancelled during execution flow")
			return ctx.Err()
		default:
		}

		// ── Phase 3: Write ─────────────────────────────────────────────────────────
		phaseCtx := llmcore.WithTaskRoutingHints(ctx, task)
		conf, writeErr := c.runWritePhase(phaseCtx, task, allEvidence)
		if writeErr != nil {
			break // fallback happened or writer failed
		}

		// Adaptive Step Depth expansion: if confidence is low, and we have budget/steps left, request Planner to generate more steps
		if conf == "low" && depthIterations < maxDepthIterations && task.ToolBudget > 0 && task.StepCount < task.MaxSteps && !tokenBudgetExhausted(task) {
			depthIterations++
			log.Info("Confidence is LOW (evidence is insufficient). Triggering adaptive step depth expansion", "iteration", depthIterations, "max_iterations", maxDepthIterations)

			// Record adaptive depth trace
			task.Trace = append(task.Trace, types.StepTrace{
				Step:        task.StepCount,
				Goal:        task.Goal,
				Action:      "plan",
				Query:       "adaptive_depth",
				Observation: "[coordinator] confidence was low; requesting additional steps for deeper investigation",
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
	plan, err := c.Planner.Plan(ctx, task.Goal, task.Workspace, task.Memories)
	elapsed := time.Since(start)

	if c.Metrics != nil {
		c.Metrics.ObservePlanner(elapsed, err)
	}
	if err != nil {
		return nil, fmt.Errorf("PlannerAgent: %w", err)
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

		// Trigger re-planning if any step in the batch failed
		if anyFailed && replansCount < maxReplans {
			replansCount++
			log.Info("Triggering collaborative replan/error-correction loop", "replan_count", replansCount)

			newPlan, replanErr := c.Planner.Replan(ctx, task.Goal, task.Workspace, task.Trace, task.Memories)
			if replanErr != nil {
				log.Error("Replanner failed — continuing with remaining steps", "error", replanErr)
			} else if len(newPlan.Steps) > 0 {
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
	case "find_files", "search_text", "read_file", "git_diff", "http_fetch", "web_search":
		return true
	}
	return false
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

	var wg sync.WaitGroup
	for i, step := range batch {
		wg.Add(1)
		go func(idx int, s ResearchStep) {
			defer wg.Done()
			start := time.Now()
			ev, err := c.Researcher.Research(ctx, task.Workspace, s)
			elapsed := time.Since(start)

			var obs string
			failed := (err != nil) || (ev != nil && ev.Failed)
			if err != nil {
				obs = fmt.Sprintf("[researcher] fatal error: %v", err)
			} else {
				obs = fmt.Sprintf("[researcher] %s", ev.Observation)
			}

			var trEvidence []types.Evidence
			if ev != nil {
				trEvidence = ev.Evidence
			}

			tr := types.StepTrace{
				Step:        baseStep + idx,
				Goal:        task.Goal,
				Action:      s.Action,
				Query:       buildStepQuery(s),
				Observation: obs,
				Evidence:    trEvidence,
				AgentRole:   RoleResearcher,
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
		task.Trace = append(task.Trace, r.tr)
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
		ev, err := c.Researcher.Research(ctx, task.Workspace, step)
		elapsed := time.Since(start)

		if c.Metrics != nil {
			c.Metrics.ObserveExecutor(elapsed, err, step.Action)
		}

		var obs string
		if err != nil {
			obs = fmt.Sprintf("[researcher] fatal error: %v", err)
		} else {
			obs = fmt.Sprintf("[researcher] %s", ev.Observation)
		}

		failed := (err != nil) || (ev != nil && ev.Failed)

		var trEvidence []types.Evidence
		if ev != nil {
			trEvidence = ev.Evidence
		}

		task.Trace = append(task.Trace, types.StepTrace{
			Step:        task.StepCount,
			Goal:        task.Goal,
			Action:      step.Action,
			Query:       buildStepQuery(step),
			Observation: obs,
			Evidence:    trEvidence,
			AgentRole:   RoleResearcher,
		})

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
	_, verificationEnabled := config.Get().LLM.Scenes[config.LLMSceneAnswerVerifier]
	if c.Verifier != nil && verificationEnabled && llmcore.AllowedForTask(config.LLMSceneAnswerVerifier, task) {
		verification, verifyErr := c.Verifier.Verify(ctx, task.Goal, output.FinalAnswer, evidence)
		if verifyErr != nil {
			log.Warn("answer verifier failed; preserving writer result", "task_id", task.ID, "error", verifyErr)
		} else {
			if c.Metrics != nil {
				c.Metrics.ObserveTokens(verification.TokenUsage.PromptTokens, verification.TokenUsage.CompletionTokens, verification.TokenUsage.TotalTokens, "verifier")
			}
			if !verification.Supported {
				output.Confidence = "low"
				output.EvidenceSummary = fmt.Sprintf("%s | Verification issues: %v", output.EvidenceSummary, verification.Issues)
			}
			output.TokenUsage.PromptTokens += verification.TokenUsage.PromptTokens
			output.TokenUsage.CompletionTokens += verification.TokenUsage.CompletionTokens
			output.TokenUsage.TotalTokens += verification.TokenUsage.TotalTokens
		}
	}

	task.Trace = append(task.Trace, types.StepTrace{
		Step:        task.StepCount,
		Goal:        task.Goal,
		Action:      "write",
		Query:       "writer",
		Observation: fmt.Sprintf("[writer] Confidence: %s | Summary: %s", output.Confidence, output.EvidenceSummary),
		AgentRole:   RoleWriter,
		TokenUsage:  output.TokenUsage,
	})
	task.StepCount++
	task.FinalAnswer = output.FinalAnswer

	log.Info("Phase 3 done — answer written", "confidence", output.Confidence, "elapsed", elapsed)
	return output.Confidence, nil
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
