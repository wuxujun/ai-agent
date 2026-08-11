package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/types"
)

func TestMemoryStore_DurableApprovalLifecycle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := NewMemoryStore()
	approval := &types.DurableApproval{
		ID: "approval-1", TaskID: "task-1", TenantID: "tenant-a",
		Request: types.ApprovalRequest{
			ID: "approval-1", TaskID: "task-1", Action: "write_file",
			RiskLevel: types.RiskLevelHigh, Parameters: map[string]any{"path": "redacted"},
		},
		ActionPayload: []byte("ciphertext"), Status: types.ApprovalPending,
	}
	if err := st.CreateApproval(ctx, approval); err != nil {
		t.Fatalf("CreateApproval: %v", err)
	}
	stored, err := st.GetApproval(ctx, approval.ID, approval.TenantID)
	if err != nil {
		t.Fatalf("GetApproval: %v", err)
	}
	if stored.Version != 1 || stored.Status != types.ApprovalPending {
		t.Fatalf("stored approval = version %d, status %s", stored.Version, stored.Status)
	}
	stored.ActionPayload[0] = 'X'
	again, _ := st.GetApproval(ctx, approval.ID, approval.TenantID)
	if string(again.ActionPayload) != "ciphertext" {
		t.Fatal("GetApproval leaked mutable payload storage")
	}
	if _, err := st.GetApproval(ctx, approval.ID, "tenant-b"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-tenant GetApproval error = %v; want sql.ErrNoRows", err)
	}
	matched, err := st.TransitionApproval(ctx, approval.ID, approval.TenantID, 99, types.ApprovalPending, types.ApprovalApproved, []byte("resolution"))
	if err != nil || matched {
		t.Fatalf("stale TransitionApproval = %v, %v; want false, nil", matched, err)
	}
	matched, err = st.TransitionApproval(ctx, approval.ID, approval.TenantID, 1, types.ApprovalPending, types.ApprovalApproved, []byte("resolution"))
	if err != nil || !matched {
		t.Fatalf("TransitionApproval = %v, %v; want true, nil", matched, err)
	}
	matched, err = st.TransitionApproval(ctx, approval.ID, approval.TenantID, 1, types.ApprovalPending, types.ApprovalRejected, nil)
	if err != nil || matched {
		t.Fatalf("replayed TransitionApproval = %v, %v; want false, nil", matched, err)
	}
	listed, err := st.ListTaskApprovals(ctx, approval.TaskID, approval.TenantID, types.ApprovalApproved)
	if err != nil || len(listed) != 1 || listed[0].Version != 2 {
		t.Fatalf("ListTaskApprovals = %#v, %v", listed, err)
	}
}

func TestMemoryStore_DurableApprovalLeaseOwnership(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := NewMemoryStore()
	approval := &types.DurableApproval{
		ID: "approval-lease", TaskID: "task-lease", TenantID: "tenant-a",
		Request:       types.ApprovalRequest{ID: "approval-lease", TaskID: "task-lease", Action: "shell", RiskLevel: types.RiskLevelHigh},
		ActionPayload: []byte("ciphertext"), Status: types.ApprovalApproved,
	}
	if err := st.CreateApproval(ctx, approval); err != nil {
		t.Fatal(err)
	}
	acquired, err := st.AcquireApprovalLease(ctx, approval.ID, "owner-a", time.Minute)
	if err != nil || !acquired {
		t.Fatalf("owner-a acquire = %v, %v", acquired, err)
	}
	acquired, err = st.AcquireApprovalLease(ctx, approval.ID, "owner-b", time.Minute)
	if err != nil || acquired {
		t.Fatalf("owner-b acquire = %v, %v", acquired, err)
	}
	if err := st.ReleaseApprovalLease(ctx, approval.ID, "owner-b"); err != nil {
		t.Fatal(err)
	}
	acquired, _ = st.AcquireApprovalLease(ctx, approval.ID, "owner-b", time.Minute)
	if acquired {
		t.Fatal("wrong owner released approval lease")
	}
	if err := st.ReleaseApprovalLease(ctx, approval.ID, "owner-a"); err != nil {
		t.Fatal(err)
	}
	acquired, err = st.AcquireApprovalLease(ctx, approval.ID, "owner-b", time.Minute)
	if err != nil || !acquired {
		t.Fatalf("owner-b acquire after release = %v, %v", acquired, err)
	}
}
