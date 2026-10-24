package orchestrator

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/wuxujun/ai-agent/internal/config"
)

const (
	assessmentWorkers       = 2
	assessmentQueueCapacity = 16
	assessmentTimeout       = 30 * time.Second
)

type assessmentJob struct {
	snapshot   assessSnapshot
	config     config.HarnessRefineConfig
	skillsRoot string
	deadline   time.Time
}

// assessmentRuntime owns a fixed number of workers. Stop closes admission and
// cancels running work; Wait uses an existing completion channel, so repeated
// shutdown attempts do not leave waiter goroutines behind.
type assessmentRuntime struct {
	mu      sync.Mutex
	stopped bool
	ctx     context.Context
	cancel  context.CancelFunc
	queue   chan assessmentJob
	done    chan struct{}
	timeout time.Duration
	run     func(context.Context, assessmentJob)
}

func newAssessmentRuntime(workers, capacity int, timeout time.Duration, run func(context.Context, assessmentJob)) *assessmentRuntime {
	ctx, cancel := context.WithCancel(context.Background())
	r := &assessmentRuntime{ctx: ctx, cancel: cancel, queue: make(chan assessmentJob, capacity), done: make(chan struct{}), timeout: timeout, run: run}
	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			r.work()
		}()
	}
	go func() { wg.Wait(); close(r.done) }()
	return r
}

func (r *assessmentRuntime) submit(job assessmentJob) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return false
	}
	job.deadline = time.Now().Add(r.timeout)
	select {
	case r.queue <- job:
		return true
	default:
		return false
	}
}

func (r *assessmentRuntime) stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return
	}
	r.stopped = true
	r.cancel()
	// Release queued snapshots without starting them or waiting for running jobs.
	for {
		select {
		case <-r.queue:
		default:
			return
		}
	}
}

func (r *assessmentRuntime) work() {
	for {
		select {
		case <-r.ctx.Done():
			return
		case job := <-r.queue:
			if r.ctx.Err() != nil {
				return
			}
			r.execute(job)
		}
	}
}

func (r *assessmentRuntime) execute(job assessmentJob) {
	ctx, cancel := context.WithDeadline(r.ctx, job.deadline)
	defer cancel()
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Error("refinement: assessment panicked", "task_id", job.snapshot.taskID, "panic_type", fmt.Sprintf("%T", recovered))
		}
	}()
	if ctx.Err() == nil {
		r.run(ctx, job)
	}
}

func (e *Engine) enqueueAssessment(job assessmentJob) bool {
	e.assessmentMu.Lock()
	defer e.assessmentMu.Unlock()
	if e.assessmentsStopped {
		return false
	}
	if e.assessments == nil {
		e.assessments = newAssessmentRuntime(assessmentWorkers, assessmentQueueCapacity, assessmentTimeout, func(ctx context.Context, job assessmentJob) {
			runAssessment(ctx, job.snapshot, job.config, job.skillsRoot)
		})
	}
	return e.assessments.submit(job)
}

// StopAssessments is idempotent and permanently closes assessment admission for
// this Engine, including when called before its lazy workers have started.
func (e *Engine) StopAssessments() {
	e.assessmentMu.Lock()
	defer e.assessmentMu.Unlock()
	e.assessmentsStopped = true
	if e.assessments != nil {
		e.assessments.stop()
	}
}

// WaitAssessments joins the workers after StopAssessments. A timeout reports
// incomplete shutdown; it does not forcibly terminate a callback ignoring context.
func (e *Engine) WaitAssessments(ctx context.Context) error {
	e.assessmentMu.Lock()
	r := e.assessments
	e.assessmentMu.Unlock()
	if r == nil {
		return nil
	}
	select {
	case <-r.done:
		return nil
	default:
	}
	select {
	case <-r.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
