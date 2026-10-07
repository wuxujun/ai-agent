package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wuxujun/ai-agent/internal/types"
)

// TestExternalRedisApprovalInbox exercises the durable inbox on a shared Redis
// service. It runs only with the repository's external integration opt-in.
func TestExternalRedisApprovalInbox(t *testing.T) {
	requireExternalIntegration(t)
	redisURL := os.Getenv("TEST_REDIS_URL")
	if redisURL == "" {
		t.Skip("TEST_REDIS_URL is not set")
	}
	first, err := NewRedisStoreFromURL(redisURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := NewRedisStoreFromURL(redisURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	tenantID := "approval-inbox-" + uuid.NewString()
	taskID := tenantID + "-task"
	ids := []string{tenantID + "-a", tenantID + "-b", tenantID + "-c"}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		pipe := first.client.TxPipeline()
		for _, id := range ids {
			pipe.Del(ctx, first.approvalKey(id))
		}
		pipe.Del(ctx, approvalTaskIndex(tenantID, taskID))
		pipe.Del(ctx, approvalTenantIndex(tenantID))
		pipe.Del(ctx, approvalTenantIndexMarker(tenantID))
		pipe.Del(ctx, approvalTenantStatusIndex(tenantID, types.ApprovalPending))
		pipe.Del(ctx, approvalTenantStatusIndex(tenantID, types.ApprovalApproved))
		_, _ = pipe.Exec(ctx)
		_, _ = first.DeleteTask(ctx, taskID)
	})
	if err := first.CreateTask(t.Context(), &types.Task{ID: taskID, TenantID: tenantID, Goal: "approval inbox", Status: types.StatusAwaitingApproval}); err != nil {
		t.Fatal(err)
	}
	createdAt := time.Now().UTC().Truncate(time.Microsecond)
	for i, id := range ids {
		status := types.ApprovalPending
		if i == 2 {
			status = types.ApprovalApproved
		}
		if err := first.CreateApproval(t.Context(), &types.DurableApproval{
			ID: id, TaskID: taskID, TenantID: tenantID, CreatedAt: createdAt,
			Request:       types.ApprovalRequest{ID: id, TaskID: taskID, Action: "write_file", RiskLevel: types.RiskLevelHigh},
			ActionPayload: []byte("ciphertext"), Status: status,
		}); err != nil {
			t.Fatal(err)
		}
	}
	stats, err := second.GetApprovalStats(t.Context(), tenantID)
	if err != nil || stats.Pending != 2 || stats.Approved != 1 || stats.OldestPendingAt == nil || !stats.OldestPendingAt.Equal(createdAt) {
		t.Fatalf("initial Redis stats = %+v, %v", stats, err)
	}
	firstPage, err := second.ListApprovals(t.Context(), ApprovalListFilter{TenantID: tenantID, Status: types.ApprovalPending, Limit: 1})
	if err != nil || len(firstPage) != 1 || firstPage[0].ID != ids[1] {
		t.Fatalf("first pending page = %+v, %v", firstPage, err)
	}
	secondPage, err := second.ListApprovals(t.Context(), ApprovalListFilter{
		TenantID: tenantID, Status: types.ApprovalPending, Limit: 1,
		BeforeCreatedAt: firstPage[0].CreatedAt, BeforeID: firstPage[0].ID,
	})
	if err != nil || len(secondPage) != 1 || secondPage[0].ID != ids[0] {
		t.Fatalf("second pending page = %+v, %v", secondPage, err)
	}
	matched, err := first.TransitionApproval(t.Context(), ids[1], tenantID, firstPage[0].Version, types.ApprovalPending, types.ApprovalApproved, []byte("decision"))
	if err != nil || !matched {
		t.Fatalf("transition = %v, %v", matched, err)
	}
	pending, err := second.ListApprovals(t.Context(), ApprovalListFilter{TenantID: tenantID, Status: types.ApprovalPending})
	if err != nil || len(pending) != 1 || pending[0].ID != ids[0] {
		t.Fatalf("pending after transition = %+v, %v", pending, err)
	}
	stats, err = second.GetApprovalStats(t.Context(), tenantID)
	if err != nil || stats.Pending != 1 || stats.Approved != 2 {
		t.Fatalf("Redis stats after transition = %+v, %v", stats, err)
	}
	// Simulate a pre-index deployment and verify lazy reconstruction.
	if err := first.client.Del(t.Context(), approvalTenantIndexMarker(tenantID), approvalTenantIndex(tenantID),
		approvalTenantStatusIndex(tenantID, types.ApprovalPending), approvalTenantStatusIndex(tenantID, types.ApprovalApproved)).Err(); err != nil {
		t.Fatal(err)
	}
	stats, err = second.GetApprovalStats(t.Context(), tenantID)
	if err != nil || stats.Pending != 1 || stats.Approved != 2 {
		t.Fatalf("Redis stats after index rebuild = %+v, %v", stats, err)
	}
	approved, err := second.ListApprovals(t.Context(), ApprovalListFilter{TenantID: tenantID, Status: types.ApprovalApproved})
	if err != nil || len(approved) != 2 {
		t.Fatalf("approved after index rebuild = %+v, %v", approved, err)
	}
}
