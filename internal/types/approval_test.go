package types

import "testing"

func TestDurableApprovalValidationAndTransitions(t *testing.T) {
	record := &DurableApproval{
		ID: "approval-1", TaskID: "task-1", TenantID: "tenant-1",
		Request:       ApprovalRequest{ID: "approval-1", TaskID: "task-1", Action: "write_file", RiskLevel: RiskLevelHigh},
		ActionPayload: []byte("ciphertext"),
	}
	if err := record.Validate(); err != nil {
		t.Fatal(err)
	}
	if record.Status != ApprovalPending {
		t.Fatalf("default status = %q", record.Status)
	}
	for _, transition := range []struct {
		from, to DurableApprovalStatus
		want     bool
	}{
		{ApprovalPending, ApprovalApproved, true},
		{ApprovalPending, ApprovalRejected, true},
		{ApprovalApproved, ApprovalConsumed, true},
		{ApprovalRejected, ApprovalConsumed, true},
		{ApprovalConsumed, ApprovalApproved, false},
	} {
		if got := CanTransitionApproval(transition.from, transition.to); got != transition.want {
			t.Errorf("transition %s -> %s = %t, want %t", transition.from, transition.to, got, transition.want)
		}
	}
}

func TestDurableApprovalRejectsUnsafeIdentity(t *testing.T) {
	record := &DurableApproval{
		ID: "approval-1", TaskID: "task-1", TenantID: "tenant-1",
		Request:       ApprovalRequest{ID: "other", TaskID: "task-1", Action: "write_file", RiskLevel: RiskLevelHigh},
		ActionPayload: []byte("ciphertext"),
	}
	if err := record.Validate(); err == nil {
		t.Fatal("expected mismatched request identity to fail")
	}
}
