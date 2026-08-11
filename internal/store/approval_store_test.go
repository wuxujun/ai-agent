package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/types"
)

func TestSQLiteStore_CleanupTerminalApprovals(t *testing.T) {
	st, err := NewSQLiteStore(filepath.Join(t.TempDir(), "approvals.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	cutoff := time.Now().Add(-time.Hour)
	record := &types.DurableApproval{
		ID: "sqlite-cleanup", TaskID: "sqlite-cleanup-task", TenantID: "tenant-a",
		Request:       types.ApprovalRequest{ID: "sqlite-cleanup", TaskID: "sqlite-cleanup-task", Action: "write_file", RiskLevel: types.RiskLevelHigh},
		ActionPayload: []byte("ciphertext"), Status: types.ApprovalExpired, ResolvedAt: cutoff.Add(-time.Minute),
	}
	if err := st.CreateApproval(ctx, record); err != nil {
		t.Fatal(err)
	}
	deleted, err := st.DeleteTerminalApprovalsBefore(ctx, cutoff)
	if err != nil || deleted != 1 {
		t.Fatalf("DeleteTerminalApprovalsBefore = %d, %v", deleted, err)
	}
	if _, err := st.GetApproval(ctx, record.ID, record.TenantID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetApproval after cleanup = %v", err)
	}
}

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

func TestMemoryStore_CleanupOnlyOldTerminalApprovals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := NewMemoryStore()
	cutoff := time.Now().Add(-24 * time.Hour)
	for _, tc := range []struct {
		id       string
		status   types.DurableApprovalStatus
		resolved time.Time
	}{
		{"old-consumed", types.ApprovalConsumed, cutoff.Add(-time.Hour)},
		{"old-expired", types.ApprovalExpired, cutoff.Add(-time.Hour)},
		{"old-rejected", types.ApprovalRejected, cutoff.Add(-time.Hour)},
		{"new-consumed", types.ApprovalConsumed, cutoff.Add(time.Hour)},
	} {
		record := &types.DurableApproval{
			ID: tc.id, TaskID: "cleanup-task", TenantID: "tenant-a",
			Request:       types.ApprovalRequest{ID: tc.id, TaskID: "cleanup-task", Action: "write_file", RiskLevel: types.RiskLevelHigh},
			ActionPayload: []byte("ciphertext"), Status: tc.status, ResolvedAt: tc.resolved,
		}
		if err := st.CreateApproval(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	deleted, err := st.DeleteTerminalApprovalsBefore(ctx, cutoff)
	if err != nil || deleted != 2 {
		t.Fatalf("DeleteTerminalApprovalsBefore = %d, %v", deleted, err)
	}
	remaining, err := st.ListTaskApprovals(ctx, "cleanup-task", "tenant-a", "")
	if err != nil || len(remaining) != 2 || remaining[0].ID == "old-consumed" || remaining[0].ID == "old-expired" || remaining[1].ID == "old-consumed" || remaining[1].ID == "old-expired" {
		t.Fatalf("remaining = %#v, %v", remaining, err)
	}
}
