package multiagent

import (
	"context"
	"fmt"
	"log"
	"strings"
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

	// Convert slice to a dynamic list we can modify during execution
	currentSteps := make([]ResearchStep, len(steps))
	copy(currentSteps, steps)

	stepIndex := 0
	replansCount := 0
	maxReplans := 3 // Limit re-planning count to prevent infinite loop

	for stepIndex < len(currentSteps) {
		// Budget and step-count gate
		if task.ToolBudget <= 0 {
			log.Printf("[Coordinator] Tool budget exhausted at research step index %d — stopping early", stepIndex)
			break
		}
		if task.StepCount >= task.MaxSteps {
			log.Printf("[Coordinator] Max steps reached at research step index %d — stopping early", stepIndex)
			break
		}

		// Context cancellation check
		select {
		case <-ctx.Done():
			log.Printf("[Coordinator] Context cancelled during research phase")
			return allEvidence
		default:
		}

		step := currentSteps[stepIndex]
		log.Printf("[Coordinator] Executing research step %d: ID=%s, Action=%s, Desc=%q",
			task.StepCount+1, step.ID, step.Action, step.Description)

		start := time.Now()
		ev, err := c.Researcher.Research(ctx, task.Workspace, step)
		elapsed := time.Since(start)

		if c.Metrics != nil {
			c.Metrics.ObserveExecutor(elapsed, err, step.Action)
		}

		// Detect if step failed
		failed := false
		var obs string
		if err != nil {
			failed = true
			obs = fmt.Sprintf("[researcher] fatal error: %v", err)
			log.Printf("[Coordinator] ResearcherAgent fatal error at step %s: %v", step.ID, err)
		} else {
			obs = fmt.Sprintf("[researcher] %s", ev.Observation)
			// Check if the observation indicates failure or error (e.g., RCE denied, command failed, not found, policy violation)
			lowerObs := strings.ToLower(ev.Observation)
			if strings.Contains(lowerObs, "error:") ||
				strings.Contains(lowerObs, "failed:") ||
				strings.Contains(lowerObs, "not found") ||
				strings.Contains(lowerObs, "policy violation") {
				failed = true
				log.Printf("[Coordinator] Observation indicates failure at step %s: %q", step.ID, ev.Observation)
			}
		}

		// Record the trace
		var traceEvidence []types.Evidence
		if ev != nil {
			traceEvidence = ev.Evidence
		}

		task.Trace = append(task.Trace, types.StepTrace{
			Step:        task.StepCount,
			Goal:        task.Goal,
			Action:      step.Action,
			Query:       step.SearchQuery + step.FileGlob + step.FilePath + step.Command + " " + step.Args,
			Observation: obs,
			Evidence:    traceEvidence,
			AgentRole:   RoleResearcher,
		})

		if ev != nil && !failed {
			allEvidence = append(allEvidence, *ev)
		}

		task.StepCount++
		task.ToolBudget--
		task.Status = types.StatusRunning

		// Trigger re-planning if the step failed and we haven't reached replans limit
		if failed && replansCount < maxReplans {
			replansCount++
			log.Printf("[Coordinator] Triggering collaborative replan/error-correction loop (replan count: %d)", replansCount)

			// Call Replanner to adjust plan based on trace history
			newPlan, replanErr := c.Planner.Replan(ctx, task.Goal, task.Workspace, task.Trace, task.Memories)
			if replanErr != nil {
				log.Printf("[Coordinator Error] Replanner failed: %v — continuing with remaining steps", replanErr)
			} else if len(newPlan.Steps) > 0 {
				log.Printf("[Coordinator] Replanner generated %d revised steps. Replacing remaining plan.", len(newPlan.Steps))

				// Record planner trace
				task.Trace = append(task.Trace, types.StepTrace{
					Step:        task.StepCount,
					Goal:        task.Goal,
					Action:      "plan",
					Query:       "replanner",
					Observation: fmt.Sprintf("[replanner] %s — %d step(s) revised due to failure", newPlan.ThoughtSummary, len(newPlan.Steps)),
					AgentRole:   RolePlanner,
				})
				task.StepCount++

				// Replace remaining steps with the new steps, reset execution pointer
				currentSteps = newPlan.Steps
				stepIndex = 0
				continue
			}
		}

		stepIndex++
	}

	log.Printf("[Coordinator] Phase 2 done — %d evidence item(s) gathered", len(allEvidence))
	return allEvidence
}

func (c *Coordinator) runWritePhase(ctx context.Context, task *types.Task, evidence []StepEvidence) (string, error) {
	log.Printf("[Coordinator] Phase 3 — Writing final answer for task %s", task.ID)

	start := time.Now()
	output, err := c.Writer.Write(ctx, task.Goal, evidence, task.Memories)
	elapsed := time.Since(start)

	if c.Metrics != nil {
		// Reuse planner metric for writer LLM call (both are LLM latency)
		c.Metrics.ObservePlanner(elapsed, err)
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
