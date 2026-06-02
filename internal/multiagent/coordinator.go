package multiagent

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/wuxujun/ai-agent/internal/metrics"
	"github.com/wuxujun/ai-agent/internal/types"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

var tracer = otel.Tracer("ai-agent/multiagent")

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
	Planner    Planner
	Researcher Researcher
	Writer     Writer
	Metrics    *metrics.Collector
}

// NewCoordinator creates a Coordinator wired to the default LLM configuration
// derived from environment variables (same vars as the main planner).
func NewCoordinator(mc *metrics.Collector) *Coordinator {
	cfg := DefaultLLMConfig()
	return &Coordinator{
		Planner:    &PlannerAgent{Config: cfg},
		Researcher: &ResearcherAgent{},
		Writer:     &WriterAgent{Config: cfg},
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
	ctx, span := tracer.Start(ctx, "multiagent.coordinator.run")
	defer span.End()

	span.SetAttributes(
		attribute.String("agent.task.id", task.ID),
		attribute.String("agent.task.goal", task.Goal),
	)

	log.Printf("[Coordinator] Starting multi-agent workflow for task %s — goal: %q", task.ID, task.Goal)

	// ── Phase 1: Plan ──────────────────────────────────────────────────────────
	plan, err := c.runPlanPhase(ctx, task)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "plan phase failed")
		return err
	}
	span.SetAttributes(attribute.Int("multiagent.plan.step_count", len(plan.Steps)))

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
			log.Printf("[Coordinator] Context cancelled during execution flow")
			return ctx.Err()
		default:
		}

		// ── Phase 3: Write ─────────────────────────────────────────────────────────
		conf, writeErr := c.runWritePhase(ctx, task, allEvidence)
		if writeErr != nil {
			break // fallback happened or writer failed
		}

		// Adaptive Step Depth expansion: if confidence is low, and we have budget/steps left, request Planner to generate more steps
		if conf == "low" && depthIterations < maxDepthIterations && task.ToolBudget > 0 && task.StepCount < task.MaxSteps {
			depthIterations++
			log.Printf("[Coordinator] Confidence is LOW (evidence is insufficient). Triggering adaptive step depth expansion (iteration %d/%d)",
				depthIterations, maxDepthIterations)

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
			newPlan, replanErr := c.Planner.Replan(ctx, task.Goal, task.Workspace, task.Trace, task.Memories)
			if replanErr != nil || len(newPlan.Steps) == 0 {
				log.Printf("[Coordinator Error] Adaptive replan failed or returned empty steps — stopping loop")
				break
			}

			// Record planner trace
			task.Trace = append(task.Trace, types.StepTrace{
				Step:        task.StepCount,
				Goal:        task.Goal,
				Action:      "plan",
				Query:       "replanner",
				Observation: fmt.Sprintf("[replanner] %s — %d additional step(s) planned", newPlan.ThoughtSummary, len(newPlan.Steps)),
				AgentRole:   RolePlanner,
			})
			task.StepCount++

			// Prepare new steps for the next iteration
			currentSteps = newPlan.Steps
			task.Status = types.StatusRunning
			continue
		}

		break
	}

	log.Printf("[Coordinator] Workflow complete for task %s — status=%s", task.ID, task.Status)
	span.SetAttributes(
		attribute.String("agent.task.final_status", string(task.Status)),
		attribute.String("agent.task.final_answer", task.FinalAnswer),
	)
	return nil
}

// ── phase helpers ─────────────────────────────────────────────────────────────

func (c *Coordinator) runPlanPhase(ctx context.Context, task *types.Task) (*ResearchPlan, error) {
	log.Printf("[Coordinator] Phase 1 — Planning for task %s", task.ID)

	start := time.Now()
	plan, err := c.Planner.Plan(ctx, task.Goal, task.Workspace, task.Memories)
	elapsed := time.Since(start)

	if c.Metrics != nil {
		c.Metrics.ObservePlanner(elapsed, err)
	}

	if err != nil {
		return nil, fmt.Errorf("PlannerAgent: %w", err)
	}

	task.Hypothesis = plan.ThoughtSummary
	task.Trace = append(task.Trace, types.StepTrace{
		Step:      task.StepCount,
		Goal:      task.Goal,
		Action:    "plan",
		Query:     "planner",
		Observation: fmt.Sprintf("[planner] %s — %d step(s) planned",
			plan.ThoughtSummary, len(plan.Steps)),
		AgentRole: RolePlanner,
	})
	task.StepCount++
	task.Status = types.StatusRunning

	log.Printf("[Coordinator] Phase 1 done — %d steps planned in %s", len(plan.Steps), elapsed)
	return plan, nil
}

func (c *Coordinator) runResearchPhase(ctx context.Context, task *types.Task, steps []ResearchStep) []StepEvidence {
	log.Printf("[Coordinator] Phase 2 — Researching step(s) for task %s", task.ID)

	var allEvidence []StepEvidence

	currentSteps := make([]ResearchStep, len(steps))
	copy(currentSteps, steps)

	replansCount := 0
	maxReplans := 3

	for len(currentSteps) > 0 {
		// Budget and step-count gate
		if task.ToolBudget <= 0 {
			log.Printf("[Coordinator] Tool budget exhausted — stopping research early")
			break
		}
		if task.StepCount >= task.MaxSteps {
			log.Printf("[Coordinator] Max steps reached — stopping research early")
			break
		}

		// Context cancellation check
		select {
		case <-ctx.Done():
			log.Printf("[Coordinator] Context cancelled during research phase")
			return allEvidence
		default:
		}

		// Partition: collect a batch of parallelisable (read-only) steps at the front,
		// or fall back to a single serial step.
		batch, remainder, isParallel := partitionBatch(currentSteps, task.ToolBudget, task.MaxSteps-task.StepCount)
		currentSteps = remainder

		var batchEvidence []StepEvidence
		var anyFailed bool

		if isParallel && len(batch) > 1 {
			log.Printf("[Coordinator] Executing %d read-only steps in parallel", len(batch))
			batchEvidence, anyFailed = c.runBatchParallel(ctx, task, batch)
		} else {
			log.Printf("[Coordinator] Executing %d step(s) serially", len(batch))
			batchEvidence, anyFailed = c.runBatchSerial(ctx, task, batch)
		}

		allEvidence = append(allEvidence, batchEvidence...)

		// Trigger re-planning if any step in the batch failed
		if anyFailed && replansCount < maxReplans {
			replansCount++
			log.Printf("[Coordinator] Triggering collaborative replan/error-correction loop (replan count: %d)", replansCount)

			newPlan, replanErr := c.Planner.Replan(ctx, task.Goal, task.Workspace, task.Trace, task.Memories)
			if replanErr != nil {
				log.Printf("[Coordinator Error] Replanner failed: %v — continuing with remaining steps", replanErr)
			} else if len(newPlan.Steps) > 0 {
				log.Printf("[Coordinator] Replanner generated %d revised steps.", len(newPlan.Steps))
				task.Trace = append(task.Trace, types.StepTrace{
					Step:        task.StepCount,
					Goal:        task.Goal,
					Action:      "plan",
					Query:       "replanner",
					Observation: fmt.Sprintf("[replanner] %s — %d step(s) revised due to failure", newPlan.ThoughtSummary, len(newPlan.Steps)),
					AgentRole:   RolePlanner,
				})
				task.StepCount++
				currentSteps = newPlan.Steps
				continue
			}
		}
	}

	log.Printf("[Coordinator] Phase 2 done — %d evidence item(s) gathered", len(allEvidence))
	return allEvidence
}

// isReadOnlyAction returns true for actions that do not mutate the workspace
// and are therefore safe to run concurrently in a parallel batch.
func isReadOnlyAction(action string) bool {
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

		log.Printf("[Coordinator] Executing research step %d: ID=%s, Action=%s, Desc=%q",
			task.StepCount+1, step.ID, step.Action, step.Description)

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
	log.Printf("[Coordinator] Phase 3 — Writing final answer for task %s", task.ID)

	start := time.Now()
	output, err := c.Writer.Write(ctx, task.Goal, evidence, task.Memories)
	elapsed := time.Since(start)

	if c.Metrics != nil {
		c.Metrics.ObserveWriter(elapsed, err)
	}

	if err != nil {
		log.Printf("[Coordinator] WriterAgent failed for task %s: %v — using fallback answer", task.ID, err)
		// Graceful fallback: task completes with a best-effort summary
		fallback := "Research complete but synthesis failed. See trace for gathered evidence."
		task.Trace = append(task.Trace, types.StepTrace{
			Step:        task.StepCount,
			Goal:        task.Goal,
			Action:      "write",
			Query:       "writer",
			Observation: fmt.Sprintf("[writer] synthesis error: %v", err),
			AgentRole:   RoleWriter,
		})
		task.StepCount++
		task.Status = types.StatusCompleted
		task.FinalAnswer = fallback
		return "low", err
	}

	task.Trace = append(task.Trace, types.StepTrace{
		Step:  task.StepCount,
		Goal:  task.Goal,
		Action: "write",
		Query:  "writer",
		Observation: fmt.Sprintf("[writer] confidence=%s | %s",
			output.Confidence, output.EvidenceSummary),
		AgentRole: RoleWriter,
	})
	task.StepCount++
	task.Status = types.StatusCompleted
	task.FinalAnswer = output.FinalAnswer

	if c.Metrics != nil {
		c.Metrics.IncCompleted()
	}

	log.Printf("[Coordinator] Phase 3 done — answer written (confidence=%s) in %s", output.Confidence, elapsed)
	return output.Confidence, nil
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
