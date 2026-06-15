package orchestrator

import (
	"sync"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/types"
)

// TestRegisterApprovalProducesUniqueEntries is the regression test for the bug
// where the approval map was keyed by task ID, so two overlapping approval
// requests for the same task silently leaked the older goroutine's channel
// (Register clobbered the previous entry, the first waiter blocked forever).
// Each Register call must yield a distinct ID and an independent channel.
func TestRegisterApprovalProducesUniqueEntries(t *testing.T) {
	resetApprovalState(t)
	taskID := "task-overlap"

	id1, ch1 := RegisterApproval(taskID, &types.ApprovalRequest{TaskID: taskID, Action: "first"})
	id2, ch2 := RegisterApproval(taskID, &types.ApprovalRequest{TaskID: taskID, Action: "second"})

	if id1 == "" || id2 == "" {
		t.Fatalf("expected non-empty IDs, got %q and %q", id1, id2)
	}
	if id1 == id2 {
		t.Fatalf("IDs must be unique, both got %q", id1)
	}
	if ch1 == ch2 {
		t.Fatalf("channels must be distinct instances")
	}

	if got := PendingApprovalCount(taskID); got != 2 {
		t.Fatalf("PendingApprovalCount = %d, want 2", got)
	}

	pending := ListPendingApprovals(taskID)
	if len(pending) != 2 {
		t.Fatalf("ListPendingApprovals returned %d, want 2", len(pending))
	}
	// Order is registration order so callers can disambiguate.
	if pending[0].Action != "first" || pending[1].Action != "second" {
		t.Errorf("pending order = [%q, %q], want [first, second]", pending[0].Action, pending[1].Action)
	}
	// The IDs surfaced through pending must match the ones the goroutines hold.
	if pending[0].ID != id1 || pending[1].ID != id2 {
		t.Errorf("pending IDs = [%q, %q], want [%q, %q]", pending[0].ID, pending[1].ID, id1, id2)
	}

	// Cleanup so we don't leak channels into other tests.
	RemoveApproval(id1)
	RemoveApproval(id2)
}

// TestResolveApprovalRefusesAmbiguity guards the API contract: when more than
// one approval is pending for a task, the implicit single-target Resolve must
// refuse so the HTTP layer can return 409 with the list of pending IDs.
func TestResolveApprovalRefusesAmbiguity(t *testing.T) {
	resetApprovalState(t)
	taskID := "task-ambiguous"

	id1, ch1 := RegisterApproval(taskID, &types.ApprovalRequest{TaskID: taskID, Action: "a"})
	id2, ch2 := RegisterApproval(taskID, &types.ApprovalRequest{TaskID: taskID, Action: "b"})

	if ResolveApproval(taskID, types.ApprovalResult{Approved: true}) {
		t.Fatal("ResolveApproval must return false when >1 are pending")
	}

	// ResolveApprovalByID disambiguates explicitly.
	if !ResolveApprovalByID(id1, types.ApprovalResult{Approved: true}) {
		t.Fatalf("ResolveApprovalByID(%q) returned false", id1)
	}
	if got := <-ch1; got.Approved != true {
		t.Errorf("ch1 received %v, want true", got.Approved)
	}

	// After resolving one, exactly one remains — implicit Resolve now succeeds.
	if !ResolveApproval(taskID, types.ApprovalResult{Approved: false}) {
		t.Fatal("ResolveApproval must succeed when exactly one is pending")
	}
	if got := <-ch2; got.Approved != false {
		t.Errorf("ch2 received %v, want false", got.Approved)
	}

	if got := PendingApprovalCount(taskID); got != 0 {
		t.Errorf("after both resolved, PendingApprovalCount = %d, want 0", got)
	}

	_ = id2
}

// TestRemoveApprovalClosesChannel verifies the cleanup contract: if a pending
// approval is removed (e.g. context cancellation in SuspendForApproval's defer
// path), the waiting goroutine observes a zero-value receive (rejection)
// rather than blocking forever.
func TestRemoveApprovalClosesChannel(t *testing.T) {
	resetApprovalState(t)
	taskID := "task-remove"

	id, ch := RegisterApproval(taskID, &types.ApprovalRequest{TaskID: taskID, Action: "x"})

	done := make(chan bool, 1)
	go func() {
		// Receive on the channel; closed channel yields zero value (false).
		res := <-ch
		done <- res.Approved
	}()

	RemoveApproval(id)

	select {
	case got := <-done:
		if got != false {
			t.Errorf("closed channel should yield false, got %v", got)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("waiter did not unblock after RemoveApproval")
	}

	if got := PendingApprovalCount(taskID); got != 0 {
		t.Errorf("PendingApprovalCount = %d after remove, want 0", got)
	}
}

// TestConcurrentRegisterApproval is a smoke test for races on the approval map.
// Run with -race to surface lock violations in Register / Resolve / Remove.
func TestConcurrentRegisterApproval(t *testing.T) {
	resetApprovalState(t)
	taskID := "task-concurrent"

	const N = 50
	ids := make([]string, N)
	chans := make([]chan types.ApprovalResult, N)

	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			id, ch := RegisterApproval(taskID, &types.ApprovalRequest{TaskID: taskID, Action: "a"})
			ids[i] = id
			chans[i] = ch
		}()
	}
	wg.Wait()

	if got := PendingApprovalCount(taskID); got != N {
		t.Fatalf("PendingApprovalCount = %d, want %d", got, N)
	}

	seen := make(map[string]struct{}, N)
	for _, id := range ids {
		if id == "" {
			t.Fatal("empty ID returned from concurrent Register")
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate ID %q from concurrent Register", id)
		}
		seen[id] = struct{}{}
	}

	for _, id := range ids {
		RemoveApproval(id)
	}
}

// resetApprovalState wipes the package-level approval map between tests so a
// failure in one test cannot leak pending entries into another. Acquiring the
// same mutex the package uses keeps the reset itself race-free.
func resetApprovalState(t *testing.T) {
	t.Helper()
	approvalMu.Lock()
	for id, entry := range approvals {
		// Drain so any leaked waiter unblocks before we drop the channel.
		select {
		case <-entry.ch:
		default:
		}
		delete(approvals, id)
	}
	for k := range taskIndex {
		delete(taskIndex, k)
	}
	approvalMu.Unlock()
}
