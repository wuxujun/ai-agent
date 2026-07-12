package orchestrator

import (
	"sync"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/types"
)

// newTestStore returns a fresh ApprovalStore for isolated unit tests. Using a
// per-test instance instead of the package-level defaultApprovals means tests
// cannot pollute each other even when run in parallel.
func newTestStore(t *testing.T) *ApprovalStore {
	t.Helper()
	return NewApprovalStore()
}

// TestRegisterApprovalProducesUniqueEntries is the regression test for the bug
// where the approval map was keyed by task ID, so two overlapping approval
// requests for the same task silently leaked the older goroutine's channel
// (Register clobbered the previous entry, the first waiter blocked forever).
// Each Register call must yield a distinct ID and an independent channel.
func TestRegisterApprovalProducesUniqueEntries(t *testing.T) {
	s := newTestStore(t)
	taskID := "task-overlap"

	id1, ch1 := s.Register(taskID, &types.ApprovalRequest{TaskID: taskID, Action: "first"})
	id2, ch2 := s.Register(taskID, &types.ApprovalRequest{TaskID: taskID, Action: "second"})

	if id1 == "" || id2 == "" {
		t.Fatalf("expected non-empty IDs, got %q and %q", id1, id2)
	}
	if id1 == id2 {
		t.Fatalf("IDs must be unique, both got %q", id1)
	}
	if ch1 == ch2 {
		t.Fatalf("channels must be distinct instances")
	}

	if got := s.PendingCount(taskID); got != 2 {
		t.Fatalf("PendingCount = %d, want 2", got)
	}

	pending := s.List(taskID)
	if len(pending) != 2 {
		t.Fatalf("List returned %d, want 2", len(pending))
	}
	// Order is registration order so callers can disambiguate.
	if pending[0].Action != "first" || pending[1].Action != "second" {
		t.Errorf("pending order = [%q, %q], want [first, second]", pending[0].Action, pending[1].Action)
	}
	// The IDs surfaced through pending must match the ones the goroutines hold.
	if pending[0].ID != id1 || pending[1].ID != id2 {
		t.Errorf("pending IDs = [%q, %q], want [%q, %q]", pending[0].ID, pending[1].ID, id1, id2)
	}

	// Cleanup so channels are not leaked (Remove closes the channel).
	s.Remove(id1)
	s.Remove(id2)
}

// TestResolveApprovalRefusesAmbiguity guards the API contract: when more than
// one approval is pending for a task, the implicit single-target Resolve must
// refuse so the HTTP layer can return 409 with the list of pending IDs.
func TestResolveApprovalRefusesAmbiguity(t *testing.T) {
	s := newTestStore(t)
	taskID := "task-ambiguous"

	id1, ch1 := s.Register(taskID, &types.ApprovalRequest{TaskID: taskID, Action: "a"})
	id2, ch2 := s.Register(taskID, &types.ApprovalRequest{TaskID: taskID, Action: "b"})

	if s.Resolve(taskID, types.ApprovalResult{Approved: true}) {
		t.Fatal("Resolve must return false when >1 are pending")
	}

	// ResolveByID disambiguates explicitly.
	if !s.ResolveByID(id1, types.ApprovalResult{Approved: true}) {
		t.Fatalf("ResolveByID(%q) returned false", id1)
	}
	if got := <-ch1; got.Approved != true {
		t.Errorf("ch1 received %v, want true", got.Approved)
	}

	// After resolving one, exactly one remains — implicit Resolve now succeeds.
	if !s.Resolve(taskID, types.ApprovalResult{Approved: false}) {
		t.Fatal("Resolve must succeed when exactly one is pending")
	}
	if got := <-ch2; got.Approved != false {
		t.Errorf("ch2 received %v, want false", got.Approved)
	}

	if got := s.PendingCount(taskID); got != 0 {
		t.Errorf("after both resolved, PendingCount = %d, want 0", got)
	}

	_ = id2
}

// TestRemoveApprovalClosesChannel verifies the cleanup contract: if a pending
// approval is removed (e.g. context cancellation in SuspendForApproval's defer
// path), the waiting goroutine observes a zero-value receive (rejection)
// rather than blocking forever.
func TestRemoveApprovalClosesChannel(t *testing.T) {
	s := newTestStore(t)
	taskID := "task-remove"

	id, ch := s.Register(taskID, &types.ApprovalRequest{TaskID: taskID, Action: "x"})

	done := make(chan bool, 1)
	go func() {
		// Receive on the channel; closed channel yields zero value (false).
		res := <-ch
		done <- res.Approved
	}()

	s.Remove(id)

	select {
	case got := <-done:
		if got != false {
			t.Errorf("closed channel should yield false, got %v", got)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("waiter did not unblock after Remove")
	}

	if got := s.PendingCount(taskID); got != 0 {
		t.Errorf("PendingCount = %d after remove, want 0", got)
	}
}

// TestConcurrentRegisterApproval is a smoke test for races on the ApprovalStore.
// Run with -race to surface lock violations in Register / Resolve / Remove.
func TestConcurrentRegisterApproval(t *testing.T) {
	s := newTestStore(t)
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
			id, ch := s.Register(taskID, &types.ApprovalRequest{TaskID: taskID, Action: "a"})
			ids[i] = id
			chans[i] = ch
		}()
	}
	wg.Wait()

	if got := s.PendingCount(taskID); got != N {
		t.Fatalf("PendingCount = %d, want %d", got, N)
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
		s.Remove(id)
	}
}

// TestApprovalStoreIsolation verifies that two independent ApprovalStore
// instances do not share state — the core P1-1 requirement.
func TestApprovalStoreIsolation(t *testing.T) {
	s1 := NewApprovalStore()
	s2 := NewApprovalStore()

	taskID := "shared-task"
	id1, ch1 := s1.Register(taskID, &types.ApprovalRequest{TaskID: taskID, Action: "a"})
	defer s1.Remove(id1)
	_ = ch1

	// s2 must be unaffected by registrations in s1.
	if got := s2.PendingCount(taskID); got != 0 {
		t.Errorf("s2.PendingCount after s1.Register = %d, want 0; stores share state", got)
	}
	if list := s2.List(taskID); len(list) != 0 {
		t.Errorf("s2.List after s1.Register returned %d entries, want 0", len(list))
	}
	if s2.Resolve(taskID, types.ApprovalResult{Approved: true}) {
		t.Error("s2.Resolve resolved an entry that belongs to s1; stores share state")
	}
}

// resetApprovalState wipes the package-level defaultApprovals between tests
// that still use the package-level helper functions (e.g. approval_bus_test.go
// which exercises dispatchApproval directly). Tests that can use newTestStore
// should prefer that approach for proper isolation.
func resetApprovalState(t *testing.T) {
	t.Helper()
	defaultApprovals.reset()
}
