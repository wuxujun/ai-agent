package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/wiki-agent/pkg/models"
)

type fakePlanner struct {
	calls    []models.PlanRequest
	err      error
	numSteps int
}

func (f *fakePlanner) Plan(_ context.Context, req models.PlanRequest) (models.Plan, error) {
	f.calls = append(f.calls, req)
	if f.err != nil {
		return models.Plan{}, f.err
	}
	n := f.numSteps
	if n == 0 {
		n = 1
	}
	steps := make([]models.Step, n)
	for i := range steps {
		steps[i] = models.Step{ID: string(rune('a' + i)), Action: "do", References: []string{"k1"}}
	}
	return models.Plan{TaskID: req.Task.ID, Iteration: req.Iteration, Steps: steps}, nil
}

type fakeExecutor struct {
	err      error
	none     bool
	onExec   func()
	executed []string
}

func (f *fakeExecutor) Execute(_ context.Context, _ models.Task, step models.Step) ([]models.Finding, error) {
	f.executed = append(f.executed, step.ID)
	if f.onExec != nil {
		f.onExec()
	}
	if f.err != nil {
		return nil, f.err
	}
	if f.none {
		return nil, nil
	}
	return []models.Finding{{StepID: step.ID, Content: "result", Confidence: 0.9}}, nil
}

// fakeKB rejects findings for the first rejectRounds calls.
type fakeKB struct {
	rejectRounds int
	err          error
	calls        int
}

func (f *fakeKB) SubmitFindings(_ context.Context, findings []models.Finding) ([]models.ValidationResult, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	out := make([]models.ValidationResult, len(findings))
	for i := range findings {
		if f.calls <= f.rejectRounds {
			out[i] = models.ValidationResult{Status: models.StatusRejected, Violations: []models.Violation{{Rule: "confidence", Message: "too low"}}}
		} else {
			out[i] = models.ValidationResult{Status: models.StatusAccepted}
		}
	}
	return out, nil
}

func states(wf *models.Workflow) []models.State {
	out := []models.State{models.StatePending}
	for _, t := range wf.History {
		out = append(out, t.To)
	}
	return out
}

func assertStates(t *testing.T, wf *models.Workflow, want ...models.State) {
	t.Helper()
	got := states(wf)
	if len(got) != len(want) {
		t.Fatalf("states = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("states = %v, want %v", got, want)
		}
	}
	if wf.State != want[len(want)-1] {
		t.Fatalf("final state = %s, want %s", wf.State, want[len(want)-1])
	}
}

const (
	pending   = models.StatePending
	planning  = models.StatePlanning
	executing = models.StateExecuting
	verifying = models.StateVerifying
	completed = models.StateCompleted
	failed    = models.StateFailed
	cancelled = models.StateCancelled
)

var task = models.Task{ID: "t1", Goal: "add endpoint"}

func TestRunHappyPath(t *testing.T) {
	p, e, k := &fakePlanner{numSteps: 2}, &fakeExecutor{}, &fakeKB{}
	wf, err := New(p, e, k, Config{}, nil).Run(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	assertStates(t, wf, pending, planning, executing, verifying, completed)
	if len(e.executed) != 2 || len(wf.Findings) != 2 || len(wf.Validations) != 2 {
		t.Fatalf("executed=%v findings=%d validations=%d", e.executed, len(wf.Findings), len(wf.Validations))
	}
	if len(wf.Messages) == 0 || wf.Messages[0].To != models.AgentPlanner {
		t.Fatalf("expected inter-agent messages, got %+v", wf.Messages)
	}
}

func TestRunReplansWithFeedback(t *testing.T) {
	p, k := &fakePlanner{}, &fakeKB{rejectRounds: 1}
	wf, err := New(p, &fakeExecutor{}, k, Config{MaxIterations: 3}, nil).Run(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	assertStates(t, wf, pending, planning, executing, verifying, planning, executing, verifying, completed)
	if len(p.calls) != 2 || p.calls[1].Iteration != 1 || len(p.calls[1].Feedback) != 1 {
		t.Fatalf("planner calls = %+v", p.calls)
	}
}

func TestRunFailsAfterMaxIterations(t *testing.T) {
	wf, err := New(&fakePlanner{}, &fakeExecutor{}, &fakeKB{rejectRounds: 99}, Config{MaxIterations: 2}, nil).Run(context.Background(), task)
	if !errors.Is(err, ErrVerificationFailed) {
		t.Fatalf("err = %v", err)
	}
	assertStates(t, wf, pending, planning, executing, verifying, planning, executing, verifying, failed)
	if wf.Error == "" {
		t.Fatal("workflow error not recorded")
	}
}

func TestRunNoFindingsIsUnverified(t *testing.T) {
	_, err := New(&fakePlanner{}, &fakeExecutor{none: true}, &fakeKB{}, Config{MaxIterations: 1}, nil).Run(context.Background(), task)
	if !errors.Is(err, ErrVerificationFailed) {
		t.Fatalf("err = %v", err)
	}
}

func TestRunComponentFailures(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name string
		p    *fakePlanner
		e    *fakeExecutor
		k    *fakeKB
		want []models.State
	}{
		{"planner", &fakePlanner{err: boom}, &fakeExecutor{}, &fakeKB{}, []models.State{pending, planning, failed}},
		{"executor", &fakePlanner{}, &fakeExecutor{err: boom}, &fakeKB{}, []models.State{pending, planning, executing, failed}},
		{"knowledge", &fakePlanner{}, &fakeExecutor{}, &fakeKB{err: boom}, []models.State{pending, planning, executing, verifying, failed}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wf, err := New(tt.p, tt.e, tt.k, Config{}, nil).Run(context.Background(), task)
			if !errors.Is(err, boom) {
				t.Fatalf("err = %v", err)
			}
			assertStates(t, wf, tt.want...)
		})
	}
}

func TestRunCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	e := &fakeExecutor{onExec: cancel}
	wf, err := New(&fakePlanner{numSteps: 3}, e, &fakeKB{}, Config{}, nil).Run(ctx, task)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	assertStates(t, wf, pending, planning, executing, cancelled)
	if len(e.executed) != 1 {
		t.Fatalf("executed %d steps after cancellation, want 1", len(e.executed))
	}
}

func TestTransitionTable(t *testing.T) {
	valid := [][2]models.State{
		{pending, planning}, {planning, executing}, {executing, verifying}, {verifying, completed},
		{verifying, planning}, {planning, failed}, {executing, cancelled},
	}
	for _, tr := range valid {
		if !CanTransition(tr[0], tr[1]) {
			t.Errorf("%s -> %s should be allowed", tr[0], tr[1])
		}
	}
	invalid := [][2]models.State{
		{pending, executing}, {planning, verifying}, {executing, completed}, {completed, planning},
		{failed, planning}, {cancelled, planning}, {verifying, executing},
	}
	for _, tr := range invalid {
		if CanTransition(tr[0], tr[1]) {
			t.Errorf("%s -> %s should be rejected", tr[0], tr[1])
		}
	}
	for _, s := range []models.State{completed, failed, cancelled} {
		if !s.Terminal() || len(transitions[s]) != 0 {
			t.Errorf("%s must be terminal", s)
		}
	}

	m := newMachine(task, time.Now)
	if err := m.transition(executing, ""); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("err = %v", err)
	}
	if m.wf.State != pending || len(m.wf.History) != 0 {
		t.Fatal("invalid transition must not mutate state")
	}
}
