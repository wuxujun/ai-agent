package orchestrator

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

type blockingRecoveryStore struct {
	*store.MemoryStore
	entered, resume chan struct{}
	rejectRenew     bool
}

func (s *blockingRecoveryStore) GetApproval(ctx context.Context, id, tenant string) (*types.DurableApproval, error) {
	close(s.entered)
	<-s.resume // Model an upstream call returning after ownership changes.
	return s.MemoryStore.GetApproval(context.WithoutCancel(ctx), id, tenant)
}
func (s *blockingRecoveryStore) RenewTaskLease(ctx context.Context, id, owner string, ttl time.Duration) (bool, error) {
	if s.rejectRenew {
		return false, nil
	}
	return s.MemoryStore.RenewTaskLease(ctx, id, owner, ttl)
}

func TestApprovalRecoveryRetainsTaskLeaseAndRejectsLostOwner(t *testing.T) {
	for _, lose := range []bool{false, true} {
		name := "long_recovery"
		if lose {
			name = "lost_owner"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				engine, task, approval, executor := recoveryParametersFixture(t)
				_, _, _, err := engine.PersistApprovalResolution(t.Context(), task.ID, approval.ID, types.ApprovalResult{Approved: true})
				if err != nil {
					t.Fatal(err)
				}
				base := engine.Store.(*store.MemoryStore)
				approval, err = base.GetApproval(t.Context(), approval.ID, task.TenantID)
				if err != nil {
					t.Fatal(err)
				}
				st := &blockingRecoveryStore{MemoryStore: base, entered: make(chan struct{}), resume: make(chan struct{}), rejectRenew: lose}
				engine.Store = st
				done := make(chan error, 1)
				go func() {
					_, err := engine.RecoverApprovedApproval(t.Context(), task, approval, "old-owner")
					done <- err
				}()
				<-st.entered
				synctest.Wait()
				if lose {
					if err := base.ReleaseTaskLease(t.Context(), task.ID, "old-owner"); err != nil {
						t.Fatal(err)
					}
					if ok, err := base.AcquireTaskLease(t.Context(), task.ID, "peer", time.Hour); err != nil || !ok {
						t.Fatal(ok, err)
					}
					fresh := types.CloneTask(task)
					fresh.Goal = "peer result"
					fresh.Status = types.StatusRunning
					if err := base.SaveFullTask(t.Context(), fresh); err != nil {
						t.Fatal(err)
					}
				}
				time.Sleep(40 * time.Second)
				synctest.Wait()
				peer, err := base.AcquireTaskLease(t.Context(), task.ID, "intruder", time.Hour)
				if err != nil {
					t.Fatal(err)
				}
				close(st.resume)
				recoverErr := <-done
				if peer {
					t.Error("recovery allowed competing task ownership")
				}
				if lose {
					if !errors.Is(recoverErr, store.ErrTaskLeaseLost) || executor.calls != 0 {
						t.Fatalf("old recovery err=%v calls=%d", recoverErr, executor.calls)
					}
					got, err := base.GetTask(t.Context(), task.ID)
					if err != nil || got.Goal != "peer result" || got.Status != types.StatusRunning {
						t.Fatalf("peer changed: %+v %v", got, err)
					}
					record, err := base.GetApproval(t.Context(), approval.ID, task.TenantID)
					if err != nil || record.Status != types.ApprovalApproved {
						t.Fatalf("old recovery consumed approval: %+v %v", record, err)
					}
				} else if recoverErr != nil || executor.calls != 1 {
					t.Fatalf("recovery err=%v calls=%d", recoverErr, executor.calls)
				}
			})
		})
	}
}
