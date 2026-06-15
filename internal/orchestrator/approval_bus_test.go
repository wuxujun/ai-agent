package orchestrator

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/types"
)

// TestApprovalBusMessageMarshal verifies that busMessage round-trips through
// JSON without data loss — this is the wire format exchanged via Redis.
func TestApprovalBusMessageMarshal(t *testing.T) {
	result := types.ApprovalResult{
		Approved:   true,
		Parameters: map[string]any{"path": "/safe/file.go"},
	}
	msg := busMessage{
		Type:       "approve",
		ApprovalID: "abc-123",
		TaskID:     "task-99",
		Result:     &result,
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got busMessage
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ApprovalID != msg.ApprovalID {
		t.Errorf("ApprovalID: got %q want %q", got.ApprovalID, msg.ApprovalID)
	}
	if got.TaskID != msg.TaskID {
		t.Errorf("TaskID: got %q want %q", got.TaskID, msg.TaskID)
	}
	if got.Result == nil || !got.Result.Approved {
		t.Errorf("Result.Approved: got %v", got.Result)
	}
}

// TestApprovalBusDispatch verifies that the in-process approval channel
// receives the result when ResolveApprovalByID is called with a matching ID —
// simulating what dispatchApproval does after decoding a Redis message.
func TestApprovalBusDispatch(t *testing.T) {
	const taskID = "task-dispatch-test"
	req := &types.ApprovalRequest{TaskID: taskID, Action: "write_file"}
	approvalID, ch := RegisterApproval(taskID, req)
	defer RemoveApproval(approvalID)

	result := types.ApprovalResult{Approved: true, Message: "ok"}

	// Simulate what dispatchApproval does on the receiving instance.
	resolved := ResolveApprovalByID(approvalID, result)
	if !resolved {
		t.Fatal("expected ResolveApprovalByID to succeed")
	}

	select {
	case res := <-ch:
		if !res.Approved {
			t.Errorf("expected Approved=true, got %v", res.Approved)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("channel did not receive approval result in time")
	}
}

// TestApprovalBusCancelMessage verifies that a cancel busMessage marshals to
// the correct wire format (type="cancel", no result field).
func TestApprovalBusCancelMessage(t *testing.T) {
	msg := busMessage{Type: "cancel", TaskID: "task-abc"}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got busMessage
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Type != "cancel" {
		t.Errorf("Type: got %q want %q", got.Type, "cancel")
	}
	if got.TaskID != "task-abc" {
		t.Errorf("TaskID: got %q want %q", got.TaskID, "task-abc")
	}
	if got.Result != nil {
		t.Errorf("Result should be nil for cancel messages, got %v", got.Result)
	}
}

// TestApprovalBusFallbackResolve verifies that when no approvalID is specified
// the bus falls back to resolving the unique pending approval by taskID.
func TestApprovalBusFallbackResolve(t *testing.T) {
	const taskID = "task-fallback-test"
	req := &types.ApprovalRequest{TaskID: taskID, Action: "execute_code"}
	_, ch := RegisterApproval(taskID, req)

	result := types.ApprovalResult{Approved: false, Message: "too risky"}
	// Fallback path: resolve by task (no approvalID)
	resolved := ResolveApproval(taskID, result)
	if !resolved {
		t.Fatal("expected ResolveApproval to succeed")
	}

	select {
	case res := <-ch:
		if res.Approved {
			t.Error("expected Approved=false")
		}
		if res.Message != "too risky" {
			t.Errorf("Message: got %q want %q", res.Message, "too risky")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("channel did not receive result in time")
	}
}
