package multiagent

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/types"
)

type boundedResearcher struct {
	active, peak, calls atomic.Int32
	entered             chan struct{}
	release             chan struct{}
}

func (r *boundedResearcher) Research(ctx context.Context, _ string, s ResearchStep) (*StepEvidence, error) {
	r.calls.Add(1)
	n := r.active.Add(1)
	defer r.active.Add(-1)
	for old := r.peak.Load(); n > old; old = r.peak.Load() {
		if r.peak.CompareAndSwap(old, n) {
			break
		}
	}
	r.entered <- struct{}{}
	select {
	case <-r.release:
		return &StepEvidence{StepID: s.ID, Action: s.Action}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestRunBatchParallelConcurrencyBound(t *testing.T) {
	for _, cancelBatch := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelBatch), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			r := &boundedResearcher{entered: make(chan struct{}, 20), release: make(chan struct{})}
			c := &Coordinator{Researcher: r}
			task := &types.Task{ToolBudget: 20}
			batch := make([]ResearchStep, 20)
			for i := range batch {
				batch[i] = ResearchStep{ID: fmt.Sprint(i), Action: "read_file"}
			}
			done := make(chan struct{})
			var evidence []StepEvidence
			var failed bool
			go func() { evidence, failed = c.runBatchParallel(ctx, task, batch); close(done) }()
			for i := 0; i < 5; i++ {
				select {
				case <-r.entered:
				case <-ctx.Done():
					t.Fatal("workers did not start")
				}
			}
			select {
			case <-r.entered:
				t.Error("more than 5 steps started while workers blocked")
			case <-time.After(30 * time.Millisecond):
			}
			if cancelBatch {
				cancel()
			} else {
				close(r.release)
			}
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("batch did not finish")
			}
			if r.peak.Load() > 5 || r.active.Load() != 0 {
				t.Fatalf("peak=%d active=%d", r.peak.Load(), r.active.Load())
			}
			if cancelBatch {
				if r.calls.Load() != 5 || !failed {
					t.Fatalf("cancelled batch calls=%d failed=%v", r.calls.Load(), failed)
				}
			} else {
				if failed || len(evidence) != 20 {
					t.Fatalf("evidence=%d failed=%v", len(evidence), failed)
				}
				for i, ev := range evidence {
					if ev.StepID != batch[i].ID {
						t.Fatalf("out of order: %d", i)
					}
				}
			}
			if len(task.Trace) != 20 || task.ToolBudget != 0 {
				t.Fatalf("trace=%d budget=%d", len(task.Trace), task.ToolBudget)
			}
		})
	}
}
