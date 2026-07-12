package orchestrator

import (
	"sync"

	"github.com/google/uuid"
	"github.com/wuxujun/ai-agent/internal/types"
)

// approvalEntry holds a single pending approval and the channel that the
// suspended goroutine is waiting on. The map is keyed by ApprovalID so
// successive registrations for the same task never overwrite each other —
// the previous map keyed everything by task ID, which silently leaked the
// older goroutine's channel when a same-task approval landed a second time.
type approvalEntry struct {
	id      string
	taskID  string
	ch      chan types.ApprovalResult
	request *types.ApprovalRequest
}

// ApprovalStore is a thread-safe, purely in-memory registry of pending approvals.
// Each Engine owns one ApprovalStore, eliminating the package-level global state
// that leaked between tests and prevented safe multi-engine usage.
//
// The zero value is ready to use (maps are lazily initialised on first write).
//
// P1-1: Global-singleton elimination — the store is injected into Engine so
// tests can create independent instances. No persistence is added here; the
// ApprovalStore intentionally survives only for the lifetime of the process.
type ApprovalStore struct {
	mu sync.Mutex
	// approvals indexes pending approvals by approval ID.
	approvals map[string]*approvalEntry
	// taskIndex tracks the ordered list of pending approval IDs per task so
	// the HTTP layer can resolve the unique pending request without exposing
	// the approval ID in the common case, and can surface a 409 with the list
	// of pending IDs when more than one is outstanding.
	taskIndex map[string][]string
}

// NewApprovalStore allocates and returns a fresh ApprovalStore.
func NewApprovalStore() *ApprovalStore {
	return &ApprovalStore{
		approvals: make(map[string]*approvalEntry),
		taskIndex: make(map[string][]string),
	}
}

func (s *ApprovalStore) init() {
	if s.approvals == nil {
		s.approvals = make(map[string]*approvalEntry)
	}
	if s.taskIndex == nil {
		s.taskIndex = make(map[string][]string)
	}
}

// Register enqueues a pending approval for taskID. It returns the generated
// approval ID and the channel the caller should block on; the same ID is
// stamped onto request.ID so SSE consumers see it.
//
// Concurrent or overlapping calls for the same task are non-destructive: each
// call produces an independent entry and its own channel.
func (s *ApprovalStore) Register(taskID string, request *types.ApprovalRequest) (string, chan types.ApprovalResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.init()

	id := newApprovalID()
	ch := make(chan types.ApprovalResult, 1)
	entry := &approvalEntry{
		id:      id,
		taskID:  taskID,
		ch:      ch,
		request: request,
	}
	if request != nil {
		request.ID = id
	}
	s.approvals[id] = entry
	s.taskIndex[taskID] = append(s.taskIndex[taskID], id)
	return id, ch
}

// Remove deletes a pending entry by approval ID. If the entry is still in the
// map (i.e. no Resolve has happened) the channel is closed so a blocked reader
// observes a zero value (treated as rejection).
func (s *ApprovalStore) Remove(approvalID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.approvals[approvalID]
	if !ok {
		return
	}
	delete(s.approvals, approvalID)
	s.removeFromTaskIndex(entry.taskID, approvalID)
	close(entry.ch)
}

// Current returns the pending request for taskID when there is exactly one
// outstanding. When 0 or >1 are pending, returns (nil, false).
// Callers needing >1 details should use List.
func (s *ApprovalStore) Current(taskID string) (*types.ApprovalRequest, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := s.taskIndex[taskID]
	if len(ids) != 1 {
		return nil, false
	}
	entry, ok := s.approvals[ids[0]]
	if !ok {
		return nil, false
	}
	return entry.request, true
}

// GetByID returns the pending request for a specific approval ID, or
// (nil, false) if no such pending approval exists. Used by the HTTP layer to
// echo back the resolved approval payload when the client provided an explicit
// approval_id.
func (s *ApprovalStore) GetByID(approvalID string) (*types.ApprovalRequest, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.approvals[approvalID]
	if !ok {
		return nil, false
	}
	return entry.request, true
}

// List returns a snapshot of all pending approval requests for taskID, oldest
// first. Used by the HTTP layer to surface the ambiguous case (>1 outstanding)
// so the client can disambiguate by approval_id.
func (s *ApprovalStore) List(taskID string) []*types.ApprovalRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := s.taskIndex[taskID]
	out := make([]*types.ApprovalRequest, 0, len(ids))
	for _, id := range ids {
		if entry, ok := s.approvals[id]; ok {
			out = append(out, entry.request)
		}
	}
	return out
}

// PendingCount reports how many approval requests are outstanding for taskID
// without copying the slice.
func (s *ApprovalStore) PendingCount(taskID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.taskIndex[taskID])
}

// Resolve resolves the unique pending approval for taskID. Returns false if
// zero or more than one are pending — callers must then use ResolveByID, after
// listing the pending ones, to disambiguate.
func (s *ApprovalStore) Resolve(taskID string, result types.ApprovalResult) bool {
	s.mu.Lock()
	ids := s.taskIndex[taskID]
	if len(ids) != 1 {
		s.mu.Unlock()
		return false
	}
	id := ids[0]
	entry, ok := s.approvals[id]
	if !ok {
		s.mu.Unlock()
		return false
	}
	delete(s.approvals, id)
	s.removeFromTaskIndex(taskID, id)
	s.mu.Unlock()
	entry.ch <- result
	return true
}

// ResolveByID resolves a specific approval by its ID. Required when more than
// one approval is pending for the same task.
func (s *ApprovalStore) ResolveByID(approvalID string, result types.ApprovalResult) bool {
	s.mu.Lock()
	entry, ok := s.approvals[approvalID]
	if !ok {
		s.mu.Unlock()
		return false
	}
	delete(s.approvals, approvalID)
	s.removeFromTaskIndex(entry.taskID, approvalID)
	s.mu.Unlock()
	entry.ch <- result
	return true
}

// reset wipes all pending entries and closes their channels. Intended for test
// teardown only; acquiring the same mutex as all other operations keeps the
// reset itself race-free.
func (s *ApprovalStore) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, entry := range s.approvals {
		select {
		case <-entry.ch:
		default:
		}
		delete(s.approvals, id)
	}
	for k := range s.taskIndex {
		delete(s.taskIndex, k)
	}
}

// removeFromTaskIndex drops approvalID from taskIndex[taskID]. Callers must
// already hold s.mu.
func (s *ApprovalStore) removeFromTaskIndex(taskID, approvalID string) {
	list := s.taskIndex[taskID]
	for i, id := range list {
		if id == approvalID {
			list = append(list[:i], list[i+1:]...)
			break
		}
	}
	if len(list) == 0 {
		delete(s.taskIndex, taskID)
	} else {
		s.taskIndex[taskID] = list
	}
}

// ── Package-level singleton ───────────────────────────────────────────────────
//
// defaultApprovals is the process-wide ApprovalStore used by Engine instances
// that do not set their own Approvals field (i.e. production code wired
// through main.go). Tests that need isolation should use NewApprovalStore()
// and set Engine.Approvals explicitly.
//
// The package-level helper functions below delegate to defaultApprovals so
// existing call sites in handler.go and approval_bus.go remain unchanged.

var defaultApprovals = NewApprovalStore()

// newApprovalID returns a fresh UUID-based identifier. Exposed as a var so
// tests can substitute a deterministic generator.
var newApprovalID = func() string {
	return uuid.NewString()
}

// RegisterApproval is a package-level helper that delegates to the default
// ApprovalStore. Prefer using ApprovalStore methods directly when possible.
func RegisterApproval(taskID string, request *types.ApprovalRequest) (string, chan types.ApprovalResult) {
	return defaultApprovals.Register(taskID, request)
}

// RemoveApproval is a package-level helper that delegates to the default
// ApprovalStore.
func RemoveApproval(approvalID string) {
	defaultApprovals.Remove(approvalID)
}

// CurrentApproval is a package-level helper that delegates to the default
// ApprovalStore.
func CurrentApproval(taskID string) (*types.ApprovalRequest, bool) {
	return defaultApprovals.Current(taskID)
}

// GetApprovalByID is a package-level helper that delegates to the default
// ApprovalStore.
func GetApprovalByID(approvalID string) (*types.ApprovalRequest, bool) {
	return defaultApprovals.GetByID(approvalID)
}

// ListPendingApprovals is a package-level helper that delegates to the default
// ApprovalStore.
func ListPendingApprovals(taskID string) []*types.ApprovalRequest {
	return defaultApprovals.List(taskID)
}

// PendingApprovalCount is a package-level helper that delegates to the default
// ApprovalStore.
func PendingApprovalCount(taskID string) int {
	return defaultApprovals.PendingCount(taskID)
}

// ResolveApproval is a package-level helper that delegates to the default
// ApprovalStore.
func ResolveApproval(taskID string, result types.ApprovalResult) bool {
	return defaultApprovals.Resolve(taskID, result)
}

// ResolveApprovalByID is a package-level helper that delegates to the default
// ApprovalStore.
func ResolveApprovalByID(approvalID string, result types.ApprovalResult) bool {
	return defaultApprovals.ResolveByID(approvalID, result)
}
