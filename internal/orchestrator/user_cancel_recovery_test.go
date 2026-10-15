package orchestrator

import (
	"github.com/wuxujun/ai-agent/internal/types"
	"testing"
)

func TestExplicitUserCancellationCannotRecoverApproval(t *testing.T) {
	for _, code := range []string{"task_canceled", "client_disconnected", "execution_timeout"} {
		task := &types.Task{Status: types.StatusFailed, TerminationKind: types.TerminationUserCancelled, ErrorCode: code, FinalAnswer: "context canceled"}
		if isRecoverableApprovalTask(task) {
			t.Fatal("user cancellation became recoverable", code)
		}
	}
}
