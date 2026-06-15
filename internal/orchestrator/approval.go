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

var (
	approvalMu sync.Mutex
	// approvals indexes pending approvals by approval ID.
	approvals = make(map[string]*approvalEntry)
	// taskIndex tracks the order of pending approvals for a given task, so the
	// HTTP layer can resolve the unique pending request without exposing the
	// approval ID in the common case, and can surface a 409 with the list of
	// pending IDs when more than one is outstanding.
	taskIndex = make(map[string][]string)
)

// newApprovalID returns a fresh UUID-based identifier. Exposed as a var so
// tests can substitute a deterministic generator.
var newApprovalID = func() string {
	return uuid.NewString()
}

// RegisterApproval enqueues a pending approval for taskID. It returns the
// generated approval ID and the channel the caller should block on; the same
// ID is stamped onto request.ID so SSE consumers see it.
//
// Concurrent or overlapping calls for the same task are no longer destructive:
// each call produces an independent entry and its own channel.
func RegisterApproval(taskID string, request *types.ApprovalRequest) (string, chan types.ApprovalResult) {
	approvalMu.Lock()
	defer approvalMu.Unlock()

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
	approvals[id] = entry
	taskIndex[taskID] = append(taskIndex[taskID], id)
	return id, ch
}

// RemoveApproval deletes a pending entry by approval ID. If the entry is
// still in the map (i.e. no Resolve has happened) the channel is closed so a
// blocked reader observes a zero value (treated as rejection).
func RemoveApproval(approvalID string) {
	approvalMu.Lock()
	defer approvalMu.Unlock()
	entry, ok := approvals[approvalID]
	if !ok {
		return
	}
	delete(approvals, approvalID)
	removeFromTaskIndex(entry.taskID, approvalID)
	close(entry.ch)
}

// CurrentApproval returns the pending request for taskID when there is
// exactly one outstanding. When 0 or >1 are pending, returns (nil, false).
// Callers needing >1 details should use ListPendingApprovals.
func CurrentApproval(taskID string) (*types.ApprovalRequest, bool) {
	approvalMu.Lock()
	defer approvalMu.Unlock()
	ids := taskIndex[taskID]
	if len(ids) != 1 {
		return nil, false
	}
	entry, ok := approvals[ids[0]]
	if !ok {
		return nil, false
	}
	return entry.request, true
}

// GetApprovalByID returns the pending request for a specific approval ID, or
// (nil, false) if no such pending approval exists. Used by the HTTP layer to
// echo back the resolved approval payload when the client provided an
// explicit approval_id.
func GetApprovalByID(approvalID string) (*types.ApprovalRequest, bool) {
	approvalMu.Lock()
	defer approvalMu.Unlock()
	entry, ok := approvals[approvalID]
	if !ok {
		return nil, false
	}
	return entry.request, true
}

// ListPendingApprovals returns a snapshot of all pending approval requests
// for taskID, oldest first. Used by the HTTP layer to surface the ambiguous
// case (>1 outstanding) so the client can disambiguate by approval_id.
func ListPendingApprovals(taskID string) []*types.ApprovalRequest {
	approvalMu.Lock()
	defer approvalMu.Unlock()
	ids := taskIndex[taskID]
	out := make([]*types.ApprovalRequest, 0, len(ids))
	for _, id := range ids {
		if entry, ok := approvals[id]; ok {
			out = append(out, entry.request)
		}
	}
	return out
}

// PendingApprovalCount reports how many approval requests are outstanding
// for taskID without copying the slice.
func PendingApprovalCount(taskID string) int {
	approvalMu.Lock()
	defer approvalMu.Unlock()
	return len(taskIndex[taskID])
}

// ResolveApproval resolves the unique pending approval for taskID. Returns
// false if zero or more than one are pending — callers must then use
// ResolveApprovalByID, after listing the pending ones, to disambiguate.
func ResolveApproval(taskID string, result types.ApprovalResult) bool {
	approvalMu.Lock()
	ids := taskIndex[taskID]
	if len(ids) != 1 {
		approvalMu.Unlock()
		return false
	}
	id := ids[0]
	entry, ok := approvals[id]
	if !ok {
		approvalMu.Unlock()
		return false
	}
	delete(approvals, id)
	removeFromTaskIndex(taskID, id)
	approvalMu.Unlock()
	entry.ch <- result
	return true
}

// ResolveApprovalByID resolves a specific approval by its ID. Required when
// more than one approval is pending for the same task.
func ResolveApprovalByID(approvalID string, result types.ApprovalResult) bool {
	approvalMu.Lock()
	entry, ok := approvals[approvalID]
	if !ok {
		approvalMu.Unlock()
		return false
	}
	delete(approvals, approvalID)
	removeFromTaskIndex(entry.taskID, approvalID)
	approvalMu.Unlock()
	entry.ch <- result
	return true
}

// removeFromTaskIndex drops approvalID from taskIndex[taskID]. Callers must
// already hold approvalMu.
func removeFromTaskIndex(taskID, approvalID string) {
	list := taskIndex[taskID]
	for i, id := range list {
		if id == approvalID {
			list = append(list[:i], list[i+1:]...)
			break
		}
	}
	if len(list) == 0 {
		delete(taskIndex, taskID)
	} else {
		taskIndex[taskID] = list
	}
}
