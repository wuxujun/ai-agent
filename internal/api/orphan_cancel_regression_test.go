package api

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wuxujun/ai-agent/internal/approvalcrypto"
	"github.com/wuxujun/ai-agent/internal/orchestrator"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

func orphanCancelRequest(h *Handler) (*httptest.ResponseRecorder, *gin.Context) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodDelete, "/api/tasks/task/cancel", nil)
	c.Params = gin.Params{{Key: "id", Value: "task"}}
	h.cancelTask(c)
	return w, c
}
func TestOrphanCancelExpiresApprovalsAndAllowsDeletion(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, status := range []types.TaskStatus{types.StatusRunning, types.StatusAwaitingApproval, types.StatusPaused} {
			t.Run(backend+"/"+string(status), func(t *testing.T) {
				st := reauditRegressionStore(t, backend)
				defer st.Close()
				if err := st.SaveFullTask(t.Context(), &types.Task{ID: "task", TenantID: "tenant", Status: status}); err != nil {
					t.Fatal(err)
				}
				ds := st.(store.DurableApprovalStore)
				for _, state := range []types.DurableApprovalStatus{types.ApprovalPending, types.ApprovalApproved} {
					if err := ds.CreateApproval(t.Context(), &types.DurableApproval{ID: string(state), TaskID: "task", TenantID: "tenant", Status: state, Request: types.ApprovalRequest{ID: string(state), TaskID: "task", Action: "write_file", RiskLevel: types.RiskLevelHigh}, ActionPayload: []byte("fixture")}); err != nil {
						t.Fatal(err)
					}
				}
				h := &Handler{store: st}
				w, _ := orphanCancelRequest(h)
				saved, err := st.GetTask(t.Context(), "task")
				if w.Code != http.StatusOK || err != nil || saved.Status != types.StatusFailed || saved.TerminationKind != types.TerminationKind("user_cancelled") {
					t.Fatal("orphan not canceled", w.Code, err)
				}
				for _, id := range []string{"pending", "approved"} {
					a, err := ds.GetApproval(t.Context(), id, "tenant")
					if err != nil || a.Status != types.ApprovalExpired {
						t.Fatal("decision remains live", id, err)
					}
				}
				// Retrying an explicit cancellation is safe, including after partial cleanup.
				if w, _ := orphanCancelRequest(h); w.Code != http.StatusOK {
					t.Fatal("cancel retry failed", w.Code)
				}
				w = httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				c.Request = httptest.NewRequest(http.MethodDelete, "/api/tasks/task", nil)
				c.Params = gin.Params{{Key: "id", Value: "task"}}
				h.deleteTask(c)
				if w.Code != http.StatusOK {
					t.Fatal("task remains undeletable", w.Code, w.Body.String())
				}
			})
		}
	}
}
func TestOrphanCancelDoesNotOverwritePeerOrFreshTerminal(t *testing.T) {
	for _, scenario := range []string{"peer", "fresh terminal"} {
		t.Run(scenario, func(t *testing.T) {
			base := store.NewMemoryStore()
			defer base.Close()
			if err := base.SaveFullTask(t.Context(), &types.Task{ID: "task", Status: types.StatusRunning}); err != nil {
				t.Fatal(err)
			}
			var st store.Store = base
			if scenario == "peer" {
				ok, err := base.AcquireTaskLease(t.Context(), "task", "peer", time.Minute)
				if err != nil || !ok {
					t.Fatal(err)
				}
			} else {
				st = &reauditAcquireHook{Store: base, after: func(ctx context.Context, id, owner string) {
					task, err := base.GetTask(ctx, id)
					if err != nil {
						t.Fatal(err)
					}
					task.Status = types.StatusCompleted
					task.FinalAnswer = "new final answer"
					if err := base.SaveFullTask(store.WithTaskLease(ctx, id, owner), task); err != nil {
						t.Fatal(err)
					}
				}}
			}
			w, _ := orphanCancelRequest(&Handler{store: st})
			want := http.StatusConflict
			if scenario != "peer" {
				want = http.StatusBadRequest
			}
			saved, err := base.GetTask(t.Context(), "task")
			if w.Code != want || err != nil || saved.Status == types.StatusFailed {
				t.Fatal("cancel clobbered checkpoint", w.Code, err)
			}
		})
	}
}

type orphanCancelFailureStore struct {
	*store.MemoryStore
	fail bool
	race bool
}

func (s *orphanCancelFailureStore) TransitionApproval(ctx context.Context, id, tenant string, version int64, from, to types.DurableApprovalStatus, payload []byte) (bool, error) {
	if s.fail {
		return false, errors.New("fixture approval persistence failure")
	}
	if s.race && from == types.ApprovalPending && to == types.ApprovalExpired {
		s.race = false
		_, err := s.MemoryStore.TransitionApproval(ctx, id, tenant, version, from, types.ApprovalApproved, payload)
		return false, err
	}
	return s.MemoryStore.TransitionApproval(ctx, id, tenant, version, from, to, payload)
}
func TestOrphanCancelApprovalFailureAndResolutionRace(t *testing.T) {
	for _, scenario := range []string{"failure", "resolution race"} {
		t.Run(scenario, func(t *testing.T) {
			st := &orphanCancelFailureStore{MemoryStore: store.NewMemoryStore(), fail: scenario == "failure", race: scenario == "resolution race"}
			defer st.Close()
			if err := st.SaveFullTask(t.Context(), &types.Task{ID: "task", TenantID: "tenant", Status: types.StatusAwaitingApproval}); err != nil {
				t.Fatal(err)
			}
			if err := st.CreateApproval(t.Context(), &types.DurableApproval{ID: "decision", TaskID: "task", TenantID: "tenant", Status: types.ApprovalPending, Request: types.ApprovalRequest{ID: "decision", TaskID: "task", Action: "write_file", RiskLevel: types.RiskLevelHigh}, ActionPayload: []byte("fixture")}); err != nil {
				t.Fatal(err)
			}
			h := &Handler{store: st}
			w, c := orphanCancelRequest(h)
			saved, err := st.GetTask(t.Context(), "task")
			if err != nil || saved.TerminationKind != types.TerminationKind("user_cancelled") {
				t.Fatal("failure did not fail closed", err)
			}
			if scenario == "failure" {
				if len(c.Errors) == 0 {
					t.Fatal("approval failure reported success", w.Code)
				}
				// Even a previously approved checkpoint cannot execute after
				// durable cancellation, when expiring its record has failed.
				approval, loadErr := st.GetApproval(t.Context(), "decision", "tenant")
				if loadErr != nil {
					t.Fatal(loadErr)
				}
				approval.Status = types.ApprovalApproved
				codec, keyErr := approvalcrypto.New(bytes.Repeat([]byte{0x57}, 32))
				if keyErr != nil {
					t.Fatal(keyErr)
				}
				executor := &leaseCountingExecutor{}
				eng := &orchestrator.Engine{Store: st, ApprovalCodec: codec, Executor: executor}
				recovered, recoverErr := eng.RecoverApprovedApproval(t.Context(), saved, approval, "recovery-after-cancel")
				if recovered || recoverErr != nil || executor.calls.Load() != 0 {
					t.Fatal("explicit cancellation replayed approval", recoverErr)
				}
				st.fail = false
				w, _ = orphanCancelRequest(h)
			}
			a, err := st.GetApproval(t.Context(), "decision", "tenant")
			if w.Code != http.StatusOK || err != nil || a.Status != types.ApprovalExpired {
				t.Fatal("cancel cleanup retry/race failed", w.Code, err)
			}
		})
	}
}

func TestPausedTaskAdvertisesCancellation(t *testing.T) {
	h := &Handler{store: store.NewMemoryStore()}
	if !slices.Contains(h.allowedTaskActions(&types.Task{Status: types.StatusPaused}), "cancel") {
		t.Fatal("paused cancellation hidden from console")
	}
}
