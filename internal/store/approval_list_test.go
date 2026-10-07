package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/types"
)

func TestListApprovals_TenantCursorAndStatus(t *testing.T) {
	for _, backend := range []struct {
		name string
		open func(*testing.T) Store
	}{
		{"memory", func(*testing.T) Store { return NewMemoryStore() }},
		{"sqlite", func(t *testing.T) Store {
			st, err := NewSQLiteStore(filepath.Join(t.TempDir(), "approvals.db"))
			if err != nil {
				t.Fatal(err)
			}
			return st
		}},
	} {
		t.Run(backend.name, func(t *testing.T) {
			st := backend.open(t)
			t.Cleanup(func() { _ = st.Close() })
			creator := st.(TaskCreationStore)
			approvalStore := st.(DurableApprovalStore)
			lister := st.(ApprovalListStore)
			for _, tenant := range []string{"tenant-a", "tenant-b"} {
				if err := creator.CreateTask(t.Context(), &types.Task{ID: tenant + "-task", TenantID: tenant, Goal: "test", Status: types.StatusAwaitingApproval}); err != nil {
					t.Fatal(err)
				}
			}
			created := time.Now().UTC().Truncate(time.Microsecond)
			for _, fixture := range []struct {
				id, tenant string
				status     types.DurableApprovalStatus
			}{
				{"a-1", "tenant-a", types.ApprovalPending},
				{"a-2", "tenant-a", types.ApprovalPending},
				{"a-3", "tenant-a", types.ApprovalApproved},
				{"b-1", "tenant-b", types.ApprovalPending},
			} {
				if err := approvalStore.CreateApproval(t.Context(), &types.DurableApproval{
					ID: fixture.id, TaskID: fixture.tenant + "-task", TenantID: fixture.tenant,
					Request:       types.ApprovalRequest{ID: fixture.id, TaskID: fixture.tenant + "-task", Action: "write_file", RiskLevel: types.RiskLevelHigh},
					ActionPayload: []byte("ciphertext"), Status: fixture.status, CreatedAt: created,
				}); err != nil {
					t.Fatal(err)
				}
			}
			first, err := lister.ListApprovals(context.Background(), ApprovalListFilter{TenantID: "tenant-a", Status: types.ApprovalPending, Limit: 1})
			if err != nil || len(first) != 1 || first[0].ID != "a-2" {
				t.Fatalf("first page = %+v, %v", first, err)
			}
			second, err := lister.ListApprovals(context.Background(), ApprovalListFilter{
				TenantID: "tenant-a", Status: types.ApprovalPending, Limit: 2,
				BeforeCreatedAt: first[0].CreatedAt, BeforeID: first[0].ID,
			})
			if err != nil || len(second) != 1 || second[0].ID != "a-1" {
				t.Fatalf("second page = %+v, %v", second, err)
			}
			if rows, err := lister.ListApprovals(context.Background(), ApprovalListFilter{Status: types.ApprovalPending}); err == nil || rows != nil {
				t.Fatalf("missing tenant = %+v, %v", rows, err)
			}
			statsStore := st.(ApprovalStatsStore)
			stats, err := statsStore.GetApprovalStats(t.Context(), "tenant-a")
			if err != nil || stats.Pending != 2 || stats.Approved != 1 || stats.OldestPendingAt == nil || !stats.OldestPendingAt.Equal(created) {
				t.Fatalf("tenant-a stats = %+v, %v", stats, err)
			}
			foreign, err := statsStore.GetApprovalStats(t.Context(), "tenant-b")
			if err != nil || foreign.Pending != 1 || foreign.Approved != 0 {
				t.Fatalf("tenant-b stats = %+v, %v", foreign, err)
			}
			if _, err := statsStore.GetApprovalStats(t.Context(), ""); err == nil {
				t.Fatal("missing tenant accepted by approval statistics")
			}
			changed, err := approvalStore.TransitionApproval(t.Context(), "a-1", "tenant-a", 1, types.ApprovalPending, types.ApprovalRejected, nil)
			if err != nil || !changed {
				t.Fatalf("approval transition = %v, %v", changed, err)
			}
			stats, err = statsStore.GetApprovalStats(t.Context(), "tenant-a")
			if err != nil || stats.Pending != 1 || stats.Rejected != 1 || stats.Approved != 1 || stats.OldestPendingAt == nil {
				t.Fatalf("stats after transition = %+v, %v", stats, err)
			}
		})
	}
}
