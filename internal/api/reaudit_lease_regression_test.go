package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wuxujun/ai-agent/internal/orchestrator"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

type reauditRegressionPipeline struct {
	run func(context.Context, *types.Task)
}

func (p reauditRegressionPipeline) Process(ctx context.Context, task *types.Task, _ string) (*types.AnswerAuditReport, error) {
	p.run(ctx, task)
	report := &types.AnswerAuditReport{PipelineVersion: "lease-regression", Publishable: true}
	task.AnswerAudit = report
	return report, nil
}

type reauditAcquireHook struct {
	store.Store
	after func(context.Context, string, string)
}

func (s *reauditAcquireHook) AcquireTaskLease(ctx context.Context, id, owner string, ttl time.Duration) (bool, error) {
	ok, err := s.Store.AcquireTaskLease(ctx, id, owner, ttl)
	if err == nil && ok && s.after != nil {
		s.after(ctx, id, owner)
	}
	return ok, err
}
func (s *reauditAcquireHook) RenewTaskLease(ctx context.Context, id, owner string, ttl time.Duration) (bool, error) {
	return s.Store.(store.TaskLeaseStore).RenewTaskLease(ctx, id, owner, ttl)
}
func reauditRegressionStore(t *testing.T, kind string) store.Store {
	t.Helper()
	if kind == "memory" {
		return store.NewMemoryStore()
	}
	st, err := store.NewSQLiteStore(filepath.Join(t.TempDir(), "audit.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	return st
}
func reauditRegressionRequest(h *Handler) (*httptest.ResponseRecorder, *gin.Context) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/tasks/task/re-audit", nil)
	c.Params = gin.Params{{Key: "id", Value: "task"}}
	h.reauditTask(c)
	return w, c
}
func TestReauditReloadsCheckpointUnderLease(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, status := range []types.TaskStatus{types.StatusCompleted, types.StatusRunning} {
			t.Run(backend+"/"+string(status), func(t *testing.T) {
				base := reauditRegressionStore(t, backend)
				defer base.Close()
				if err := base.SaveFullTask(t.Context(), &types.Task{ID: "task", TenantID: "tenant", Status: types.StatusCompleted, FinalAnswer: "old"}); err != nil {
					t.Fatal(err)
				}
				st := &reauditAcquireHook{Store: base, after: func(ctx context.Context, id, owner string) {
					latest, err := base.GetTask(ctx, id)
					if err != nil {
						t.Fatal(err)
					}
					latest.Status = status
					latest.FinalAnswer = "latest"
					if err := base.SaveFullTask(store.WithTaskLease(ctx, id, owner), latest); err != nil {
						t.Fatal(err)
					}
				}}
				calls := 0
				pipeline := reauditRegressionPipeline{run: func(_ context.Context, task *types.Task) {
					calls++
					if task.FinalAnswer != "latest" {
						t.Error("pipeline received stale checkpoint")
					}
				}}
				h := &Handler{store: st, engine: &orchestrator.Engine{Store: st, AnswerPipeline: pipeline}, taskSem: newResizableSemaphore(1)}
				w, _ := reauditRegressionRequest(h)
				want := http.StatusOK
				wantCalls := 1
				if status == types.StatusRunning {
					want = http.StatusConflict
					wantCalls = 0
				}
				saved, err := base.GetTask(t.Context(), "task")
				if w.Code != want || calls != wantCalls || err != nil || saved.FinalAnswer != "latest" || saved.Status != status {
					t.Fatal("stale audit overwrite", w.Code, calls, err)
				}
			})
		}
	}
}
func TestReauditLostOwnerCannotOverwritePeer(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			base := reauditRegressionStore(t, backend)
			defer base.Close()
			if err := base.SaveFullTask(t.Context(), &types.Task{ID: "task", Status: types.StatusCompleted, FinalAnswer: "old"}); err != nil {
				t.Fatal(err)
			}
			var owner string
			st := &reauditAcquireHook{Store: base, after: func(_ context.Context, _ string, value string) { owner = value }}
			pipeline := reauditRegressionPipeline{run: func(ctx context.Context, _ *types.Task) {
				if err := base.ReleaseTaskLease(ctx, "task", owner); err != nil {
					t.Fatal(err)
				}
				ok, err := base.AcquireTaskLease(ctx, "task", "peer", time.Minute)
				if err != nil || !ok {
					t.Fatal("peer lease", err)
				}
				latest, err := base.GetTask(ctx, "task")
				if err != nil {
					t.Fatal(err)
				}
				latest.FinalAnswer = "peer answer"
				if err := base.SaveFullTask(store.WithTaskLease(ctx, "task", "peer"), latest); err != nil {
					t.Fatal(err)
				}
			}}
			h := &Handler{store: st, engine: &orchestrator.Engine{Store: st, AnswerPipeline: pipeline}, taskSem: newResizableSemaphore(1)}
			w, _ := reauditRegressionRequest(h)
			saved, err := base.GetTask(t.Context(), "task")
			if err != nil || saved.FinalAnswer != "peer answer" || saved.AnswerAudit != nil {
				t.Fatal("audit overwrote peer", err)
			}
			if w.Code != http.StatusConflict {
				t.Fatal("missing ownership conflict", w.Code, w.Body.String())
			}
		})
	}
}

func TestReauditRenewalLossCancelsPipeline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st := &controlledLeaseStore{MemoryStore: store.NewMemoryStore()}
		defer st.Close()
		st.reject.Store(true)
		if err := st.SaveFullTask(t.Context(), &types.Task{ID: "task", Status: types.StatusCompleted, FinalAnswer: "old"}); err != nil {
			t.Fatal(err)
		}
		pipeline := reauditRegressionPipeline{run: func(ctx context.Context, _ *types.Task) { <-ctx.Done() }}
		h := &Handler{store: st, engine: &orchestrator.Engine{Store: st, AnswerPipeline: pipeline}, taskSem: newResizableSemaphore(1)}
		w, _ := reauditRegressionRequest(h)
		saved, err := st.GetTask(t.Context(), "task")
		if w.Code != http.StatusConflict || st.renewals.Load() == 0 || err != nil || saved.AnswerAudit != nil || saved.FinalAnswer != "old" {
			t.Fatal("renewal failure did not stop re-audit", w.Code, err)
		}
	})
}
