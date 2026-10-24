package orchestrator

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/types"
)

func waitAssessmentSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatal("assessment worker did not make progress")
	}
}
func stopAssessmentRuntime(t *testing.T, r *assessmentRuntime) {
	t.Helper()
	r.stop()
	waitAssessmentSignal(t, r.done)
}
func TestAssessmentQueueBoundsAndStop(t *testing.T) {
	started := make(chan struct{}, 2)
	var active, peak atomic.Int32
	r := newAssessmentRuntime(2, 2, time.Second, func(ctx context.Context, _ assessmentJob) {
		current := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); current > old; old = peak.Load() {
			if peak.CompareAndSwap(old, current) {
				break
			}
		}
		started <- struct{}{}
		<-ctx.Done()
	})
	t.Cleanup(func() { stopAssessmentRuntime(t, r) })
	for range 2 {
		if !r.submit(assessmentJob{}) {
			t.Fatal("initial job rejected")
		}
		waitAssessmentSignal(t, started)
	}
	for range 2 {
		if !r.submit(assessmentJob{}) {
			t.Fatal("queue capacity not available")
		}
	}
	if r.submit(assessmentJob{}) {
		t.Fatal("unbounded queue")
	}
	r.stop()
	if r.submit(assessmentJob{}) {
		t.Fatal("accepted after stop")
	}
	waitAssessmentSignal(t, r.done)
	if peak.Load() != 2 || active.Load() != 0 {
		t.Fatalf("peak=%d active=%d", peak.Load(), active.Load())
	}
	if len(r.queue) != 0 {
		t.Fatal("queued snapshots retained after stop")
	}
}

func TestAssessmentDeadlineIncludesQueueTime(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	releaseWork := sync.OnceFunc(func() { close(release) })
	var runs atomic.Int32
	r := newAssessmentRuntime(1, 1, 30*time.Millisecond, func(ctx context.Context, _ assessmentJob) {
		runs.Add(1)
		started <- struct{}{}
		<-ctx.Done()
		<-release
	})
	t.Cleanup(func() { releaseWork(); stopAssessmentRuntime(t, r) })
	if !r.submit(assessmentJob{}) {
		t.Fatal("initial rejection")
	}
	waitAssessmentSignal(t, started)
	if !r.submit(assessmentJob{}) {
		t.Fatal("queue rejection")
	}
	// Wait for the queued job's own absolute deadline, without starting it.
	r.mu.Lock()
	queued := <-r.queue
	r.queue <- queued
	r.mu.Unlock()
	timer := time.NewTimer(time.Until(queued.deadline))
	defer timer.Stop()
	<-timer.C
	releaseWork()
	// Wait until the worker consumes the expired item without executing it.
	deadline := time.After(3 * time.Second)
	for {
		if runs.Load() > 1 {
			t.Fatal("expired queued job ran")
		}
		if len(r.queue) == 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("expired job not consumed")
		case <-time.After(time.Millisecond):
		}
	}
	r.stop()
	waitAssessmentSignal(t, r.done)
	if runs.Load() != 1 {
		t.Fatal("queue time was not included")
	}
}

func TestAssessmentPanicDoesNotLoseWorker(t *testing.T) {
	continued := make(chan struct{})
	var calls atomic.Int32
	r := newAssessmentRuntime(1, 2, time.Second, func(context.Context, assessmentJob) {
		if calls.Add(1) == 1 {
			panic("synthetic")
		}
		close(continued)
	})
	t.Cleanup(func() { stopAssessmentRuntime(t, r) })
	if !r.submit(assessmentJob{}) || !r.submit(assessmentJob{}) {
		t.Fatal("submission rejected")
	}
	waitAssessmentSignal(t, continued)
}

func TestEngineAssessmentStopBeforeStart(t *testing.T) {
	e := &Engine{}
	e.StopAssessments()
	e.StopAssessments()
	if e.enqueueAssessment(assessmentJob{}) {
		t.Fatal("engine restarted assessment workers after shutdown")
	}
	if err := e.WaitAssessments(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestAssessmentShutdownDeadlineAndRetry(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	releaseWork := sync.OnceFunc(func() { close(release) })
	r := newAssessmentRuntime(1, 1, time.Second, func(context.Context, assessmentJob) { close(started); <-release })
	e := &Engine{assessments: r}
	t.Cleanup(func() { releaseWork(); stopAssessmentRuntime(t, r) })
	if !e.enqueueAssessment(assessmentJob{}) {
		t.Fatal("rejected")
	}
	waitAssessmentSignal(t, started)
	e.StopAssessments()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	if err := e.WaitAssessments(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want bounded wait, got %v", err)
	}
	releaseWork()
	if err := e.WaitAssessments(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestAssessmentConcurrentSubmitAndStop(t *testing.T) {
	e := &Engine{assessments: newAssessmentRuntime(2, 4, time.Second, func(ctx context.Context, _ assessmentJob) { <-ctx.Done() })}
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			for range 10 {
				e.enqueueAssessment(assessmentJob{})
			}
		})
	}
	wg.Go(e.StopAssessments)
	wg.Wait()
	if err := e.WaitAssessments(t.Context()); err != nil {
		t.Fatal(err)
	}
	if e.enqueueAssessment(assessmentJob{}) {
		t.Fatal("accepted after concurrent stop")
	}
}

func TestAssessmentCancelledBeforeCaptureDoesNotWrite(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	runAssessment(ctx, assessSnapshot{taskID: "cancelled", team: "software", teamsCfg: testRefinePlan().teamsCfg}, config.HarnessRefineConfig{LessonsDir: dir}, dir)
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("cancelled assessment wrote files: %v", err)
	}
	// Also ensure no worker is created for nonterminal or nonselected tasks.
	e := &Engine{}
	e.assessAsync(refinePlan{assessable: true}, &types.Task{Status: types.StatusRunning}, config.HarnessRefineConfig{}, dir, "")
	if e.assessments != nil {
		t.Fatal("workers created for ineligible task")
	}
}

func TestQueuedAssessmentCapturesTaskAndOverlay(t *testing.T) {
	plan := testRefinePlan()
	plan.assessable = true
	task := &types.Task{ID: "captured", Team: "software", Status: types.StatusCompleted, StepCount: 7}
	cleanup, digest := plan.applyOverlay(t.Context(), task, t.TempDir())
	defer cleanup()
	if digest == "" {
		t.Fatal("overlay not applied")
	}
	wantSteps := task.StepCount // The selection audit itself advances the step counter.
	started := make(chan struct{})
	release := make(chan struct{})
	releaseWork := sync.OnceFunc(func() { close(release) })
	observed := make(chan assessmentJob, 1)
	r := newAssessmentRuntime(1, 1, time.Second, func(_ context.Context, job assessmentJob) { close(started); <-release; observed <- job })
	e := &Engine{assessments: r}
	t.Cleanup(func() { releaseWork(); stopAssessmentRuntime(t, r) })
	e.assessAsync(plan, task, config.HarnessRefineConfig{AssessmentEditor: "mock"}, t.TempDir(), digest)
	waitAssessmentSignal(t, started)
	cleanup()
	task.ID = "mutated"
	task.StepCount = 99
	releaseWork()
	select {
	case job := <-observed:
		if job.snapshot.taskID != "captured" || job.snapshot.steps != wantSteps || job.snapshot.harnessDigest != digest || job.snapshot.teamsCfg.Teams["software"].Planner.Model != "m2" {
			t.Fatal("queued assessment lost detached task/overlay snapshot")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("assessment did not finish")
	}
}
