package orchestrator

import (
	"sync"

	"github.com/wuxujun/ai-agent/internal/types"
)

type approvalEntry struct {
	ch      chan bool
	request *types.ApprovalRequest
}

var (
	approvalMu sync.Mutex
	approvals  = make(map[string]approvalEntry)
)

func RegisterApproval(taskID string, request *types.ApprovalRequest) chan bool {
	approvalMu.Lock()
	defer approvalMu.Unlock()
	ch := make(chan bool, 1)
	approvals[taskID] = approvalEntry{ch: ch, request: request}
	return ch
}

func RemoveApproval(taskID string) {
	approvalMu.Lock()
	defer approvalMu.Unlock()
	if entry, ok := approvals[taskID]; ok {
		close(entry.ch)
		delete(approvals, taskID)
	}
}

func CurrentApproval(taskID string) (*types.ApprovalRequest, bool) {
	approvalMu.Lock()
	defer approvalMu.Unlock()
	entry, ok := approvals[taskID]
	return entry.request, ok
}

func ResolveApproval(taskID string, approved bool) bool {
	approvalMu.Lock()
	defer approvalMu.Unlock()
	if entry, ok := approvals[taskID]; ok {
		entry.ch <- approved
		delete(approvals, taskID)
		return true
	}
	return false
}
