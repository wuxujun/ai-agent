package multiagent

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/wuxujun/ai-agent/internal/metrics"
	"github.com/wuxujun/ai-agent/internal/types"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

var tracer = otel.Tracer("ai-agent/multiagent")

// Coordinator orchestrates the three agents in sequence:
//
//	PlannerAgent → ResearcherAgent (×N) → WriterAgent
//
// It updates task.Trace and task.Status in-place.
type Coordinator struct {
	Planner    *PlannerAgent
	Researcher *ResearcherAgent
	Writer     *WriterAgent
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

	// ── Phase 2: Research ──────────────────────────────────────────────────────
	allEvidence := c.runResearchPhase(ctx, task, plan.Steps)
	span.SetAttributes(attribute.Int("multiagent.research.evidence_items", len(allEvidence)))

	// ── Phase 3: Write ─────────────────────────────────────────────────────────
	c.runWritePhase(ctx, task, allEvidence)

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
	plan, err := c.Planner.Plan(ctx, task.Goal, task.Workspace)
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
	log.Printf("[Coordinator] Phase 2 — Researching %d step(s) for task %s", len(steps), task.ID)

	var allEvidence []StepEvidence

	for i, step := range steps {
		// Budget and step-count gate
		if task.ToolBudget <= 0 {
			log.Printf("[Coordinator] Tool budget exhausted at research step %d — stopping early", i+1)
			break
		}
		if task.StepCount >= task.MaxSteps {
			log.Printf("[Coordinator] Max steps reached at research step %d — stopping early", i+1)
			break
		}

		// Context cancellation check
		select {
		case <-ctx.Done():
			log.Printf("[Coordinator] Context cancelled during research phase: %v", ctx.Err())
			return allEvidence
		default:
		}

		start := time.Now()
		ev, err := c.Researcher.Research(ctx, task.Workspace, step)
		elapsed := time.Since(start)

		if c.Metrics != nil {
			c.Metrics.ObserveExecutor(elapsed, err, step.Action)
		}

		if err != nil {
			// Fatal researcher error (e.g. policy violation propagated up)
			log.Printf("[Coordinator] ResearcherAgent fatal error at step %s: %v", step.ID, err)
			task.Trace = append(task.Trace, types.StepTrace{
				Step:        task.StepCount,
				Goal:        task.Goal,
				Action:      step.Action,
				Query:       step.SearchQuery + step.FileGlob + step.FilePath,
				Observation: fmt.Sprintf("[researcher] fatal error: %v", err),
				AgentRole:   RoleResearcher,
			})
		} else {
			task.Trace = append(task.Trace, types.StepTrace{
				Step:        task.StepCount,
				Goal:        task.Goal,
				Action:      step.Action,
				Query:       step.SearchQuery + step.FileGlob + step.FilePath,
				Observation: fmt.Sprintf("[researcher] %s", ev.Observation),
				Evidence:    ev.Evidence,
				AgentRole:   RoleResearcher,
			})
			allEvidence = append(allEvidence, *ev)
		}

		task.StepCount++
		task.ToolBudget--
		task.Status = types.StatusRunning
	}

	log.Printf("[Coordinator] Phase 2 done — %d evidence item(s) gathered", len(allEvidence))
	return allEvidence
}

func (c *Coordinator) runWritePhase(ctx context.Context, task *types.Task, evidence []StepEvidence) {
	log.Printf("[Coordinator] Phase 3 — Writing final answer for task %s", task.ID)

	start := time.Now()
	output, err := c.Writer.Write(ctx, task.Goal, evidence)
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
		return
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
}
