package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/wuxujun/ai-agent/wiki-agent/pkg/models"
)

// ErrVerificationFailed is returned when findings are still rejected after MaxIterations.
var ErrVerificationFailed = errors.New("orchestrator: findings failed verification")

// DefaultMaxIterations bounds plan → execute → verify cycles.
const DefaultMaxIterations = 3

// Config tunes the Orchestrator.
type Config struct {
	MaxIterations int
}

// Orchestrator drives a Task through the workflow state machine. It holds no
// per-task state, so a single instance may run many tasks concurrently.
type Orchestrator struct {
	planner   Planner
	executor  Executor
	knowledge KnowledgeBase
	cfg       Config
	log       *slog.Logger
	now       func() time.Time
}

// New creates an Orchestrator. A nil logger uses slog.Default().
func New(p Planner, e Executor, k KnowledgeBase, cfg Config, log *slog.Logger) *Orchestrator {
	if cfg.MaxIterations <= 0 {
		cfg.MaxIterations = DefaultMaxIterations
	}
	if log == nil {
		log = slog.Default()
	}
	return &Orchestrator{planner: p, executor: e, knowledge: k, cfg: cfg, log: log.With("component", "orchestrator"), now: time.Now}
}

// Run executes the workflow to a terminal state. The returned Workflow is
// always non-nil; err is non-nil when the final state is failed or cancelled.
func (o *Orchestrator) Run(ctx context.Context, task models.Task) (*models.Workflow, error) {
	m := newMachine(task, o.now)
	log := o.log.With("task_id", task.ID)

	if err := m.transition(models.StatePlanning, "task accepted"); err != nil {
		return m.wf, err
	}
	var feedback []models.Violation
	for {
		if err := ctx.Err(); err != nil {
			return o.abort(m, err)
		}
		log.InfoContext(ctx, "planning", "iteration", m.wf.Iteration)
		m.send(models.AgentOrchestrator, models.AgentPlanner, "plan_request", fmt.Sprintf("iteration %d, %d feedback item(s)", m.wf.Iteration, len(feedback)))
		plan, err := o.planner.Plan(ctx, models.PlanRequest{Task: task, Iteration: m.wf.Iteration, Feedback: feedback})
		if err != nil {
			return o.abort(m, fmt.Errorf("plan: %w", err))
		}
		m.wf.Plan = &plan
		m.send(models.AgentPlanner, models.AgentOrchestrator, "plan", fmt.Sprintf("%d step(s)", len(plan.Steps)))
		if err := m.transition(models.StateExecuting, fmt.Sprintf("plan with %d step(s)", len(plan.Steps))); err != nil {
			return m.wf, err
		}

		var findings []models.Finding
		for _, step := range plan.Steps {
			if err := ctx.Err(); err != nil {
				return o.abort(m, err)
			}
			m.send(models.AgentOrchestrator, models.AgentExecutor, "execute", step.ID)
			fs, err := o.executor.Execute(ctx, task, step)
			if err != nil {
				return o.abort(m, fmt.Errorf("execute step %s: %w", step.ID, err))
			}
			m.send(models.AgentExecutor, models.AgentOrchestrator, "findings", fmt.Sprintf("step %s: %d finding(s)", step.ID, len(fs)))
			findings = append(findings, fs...)
		}
		m.wf.Findings = findings
		if err := m.transition(models.StateVerifying, fmt.Sprintf("%d finding(s)", len(findings))); err != nil {
			return m.wf, err
		}

		m.send(models.AgentOrchestrator, models.AgentWiki, "verify", fmt.Sprintf("%d finding(s)", len(findings)))
		results, err := o.knowledge.SubmitFindings(ctx, findings)
		if err != nil {
			return o.abort(m, fmt.Errorf("verify: %w", err))
		}
		m.wf.Validations = results
		feedback = rejections(findings, results)
		m.send(models.AgentWiki, models.AgentOrchestrator, "verdict", fmt.Sprintf("%d accepted, %d issue(s)", len(results)-countRejected(results), len(feedback)))

		if len(feedback) == 0 {
			log.InfoContext(ctx, "workflow completed", "iteration", m.wf.Iteration)
			return m.wf, m.transition(models.StateCompleted, "all findings verified")
		}
		m.wf.Iteration++
		if m.wf.Iteration >= o.cfg.MaxIterations {
			return o.abort(m, fmt.Errorf("%w after %d iteration(s)", ErrVerificationFailed, m.wf.Iteration))
		}
		log.WarnContext(ctx, "re-planning after rejected findings", "issues", len(feedback))
		if err := m.transition(models.StatePlanning, fmt.Sprintf("%d verification issue(s)", len(feedback))); err != nil {
			return m.wf, err
		}
	}
}

func (o *Orchestrator) abort(m *machine, cause error) (*models.Workflow, error) {
	to := models.StateFailed
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		to = models.StateCancelled
	}
	m.wf.Error = cause.Error()
	if err := m.transition(to, cause.Error()); err != nil {
		return m.wf, errors.Join(cause, err)
	}
	o.log.Error("workflow aborted", "task_id", m.wf.Task.ID, "state", to, "error", cause)
	return m.wf, cause
}

// rejections returns feedback for every rejected finding. A plan that yields
// no findings is itself treated as unverified.
func rejections(findings []models.Finding, results []models.ValidationResult) []models.Violation {
	if len(findings) == 0 {
		return []models.Violation{{Rule: "orchestrator", Message: "executor produced no findings to verify"}}
	}
	if len(results) != len(findings) {
		return []models.Violation{{Rule: "orchestrator", Message: fmt.Sprintf("got %d verdicts for %d findings", len(results), len(findings))}}
	}
	var out []models.Violation
	for i, r := range results {
		if r.Accepted() {
			continue
		}
		for _, v := range r.Violations {
			v.Message = fmt.Sprintf("finding for step %s: %s", findings[i].StepID, v.Message)
			out = append(out, v)
		}
		if len(r.Violations) == 0 {
			out = append(out, models.Violation{Rule: "orchestrator", Message: fmt.Sprintf("finding for step %s rejected", findings[i].StepID)})
		}
	}
	return out
}

func countRejected(rs []models.ValidationResult) int {
	n := 0
	for _, r := range rs {
		if !r.Accepted() {
			n++
		}
	}
	return n
}
