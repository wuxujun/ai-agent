package orchestrator

import "sync"

var (
	approvalMu sync.Mutex
	approvals  = make(map[string]chan bool)
)

func RegisterApproval(taskID string) chan bool {
	approvalMu.Lock()
	defer approvalMu.Unlock()
	ch := make(chan bool)
	approvals[taskID] = ch
	return ch
}

func RemoveApproval(taskID string) {
	approvalMu.Lock()
	defer approvalMu.Unlock()
	if ch, ok := approvals[taskID]; ok {
		close(ch)
		delete(approvals, taskID)
	}
}

func ResolveApproval(taskID string, approved bool) bool {
	approvalMu.Lock()
	defer approvalMu.Unlock()
	if ch, ok := approvals[taskID]; ok {
		ch <- approved
		delete(approvals, taskID)
		return true
	}
	return false
}
