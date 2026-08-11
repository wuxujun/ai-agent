package api

import (
	"bytes"
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/approvalcrypto"
	"github.com/wuxujun/ai-agent/internal/orchestrator"
	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

type competingRecoveryExecutor struct{ calls atomic.Int32 }

func (e *competingRecoveryExecutor) Execute(_ context.Context, task *types.Task, decision *planner.PlanDecision) ([]types.StepTrace, error) {
	e.calls.Add(1)
	return []types.StepTrace{{Step: task.StepCount + 1, Action: decision.Actions[0].Action, Observation: "executed once"}}, nil
}

func TestDurableApprovalRecoveryCompetingInstancesExecuteOnce(t *testing.T) {
	t.Parallel()
	st := store.NewMemoryStore()
	codec, err := approvalcrypto.New(bytes.Repeat([]byte{0x71}, 32))
	if err != nil {
		t.Fatal(err)
	}
	executor := &competingRecoveryExecutor{}
	engineA := &orchestrator.Engine{Store: st, ApprovalCodec: codec, Executor: executor}
	engineB := &orchestrator.Engine{Store: st, ApprovalCodec: codec, Executor: executor}
	handlerA := &Handler{store: st, engine: engineA}
	handlerB := &Handler{store: st, engine: engineB}
	task := &types.Task{ID: "competing-recovery", TenantID: "tenant-a", Status: types.StatusAwaitingApproval}
	if err := st.SaveFullTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	actionJSON, _ := json.Marshal(planner.ActionCall{Action: "write_file", Parameters: map[string]any{"path": "x.txt"}})
	actionPayload, err := codec.Encrypt(actionJSON)
	if err != nil {
		t.Fatal(err)
	}
	resolutionJSON, _ := json.Marshal(types.ApprovalResult{Approved: true})
	resolutionPayload, err := codec.Encrypt(resolutionJSON)
	if err != nil {
		t.Fatal(err)
	}
	approval := &types.DurableApproval{
		ID: "competing-approval", TaskID: task.ID, TenantID: task.TenantID,
		Request:           types.ApprovalRequest{ID: "competing-approval", TaskID: task.ID, Action: "write_file", RiskLevel: types.RiskLevelHigh},
		ActionPayload:     actionPayload,
		ResolutionPayload: resolutionPayload,
		Status:            types.ApprovalApproved,
	}
	if err := st.CreateApproval(context.Background(), approval); err != nil {
		t.Fatal(err)
	}

	handlerA.startDurableApprovalRecovery(task.ID, approval.ID)
	handlerB.startDurableApprovalRecovery(task.ID, approval.ID)
	done := make(chan struct{})
	go func() {
		handlerA.wg.Wait()
		handlerB.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("competing recovery handlers did not finish")
	}
	if got := executor.calls.Load(); got != 1 {
		t.Fatalf("executor calls = %d; want 1", got)
	}
	storedApproval, err := st.GetApproval(context.Background(), approval.ID, approval.TenantID)
	if err != nil || storedApproval.Status != types.ApprovalConsumed {
		t.Fatalf("stored approval = %#v, %v", storedApproval, err)
	}
	storedTask, err := st.GetTask(context.Background(), task.ID)
	if err != nil || storedTask.Status != types.StatusPaused || storedTask.StepCount != 1 || len(storedTask.Trace) != 1 {
		t.Fatalf("stored task = %#v, %v", storedTask, err)
	}
}
