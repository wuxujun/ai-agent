package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/approvalcrypto"
	"github.com/wuxujun/ai-agent/internal/planner"
	storepkg "github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

type recoveryExecutor struct{ calls int }

func (e *recoveryExecutor) Execute(_ context.Context, task *types.Task, decision *planner.PlanDecision) ([]types.StepTrace, error) {
	e.calls++
	return []types.StepTrace{{Step: task.StepCount + 1, Goal: task.Goal, Action: decision.Actions[0].Action, Observation: "recovered"}}, nil
}

func TestSuspendForApprovalPersistsEncryptedActionBeforeWaiting(t *testing.T) {
	t.Parallel()
	st := storepkg.NewMemoryStore()
	codec, err := approvalcrypto.New(bytes.Repeat([]byte{0x37}, 32))
	if err != nil {
		t.Fatal(err)
	}
	registry := NewApprovalStore()
	engine := &Engine{Store: st, Approvals: registry, ApprovalCodec: codec}
	task := &types.Task{ID: "durable-task", TenantID: "tenant-a", Goal: "write", Status: types.StatusRunning}
	if err := st.SaveFullTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resultCh := make(chan error, 1)
	go func() {
		_, _, suspendErr := engine.SuspendForApproval(ctx, task, "write_file", map[string]any{
			"path": "secret.txt", "content": "sensitive-value",
		})
		resultCh <- suspendErr
	}()

	var approvalID string
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		pending := registry.List(task.ID)
		if len(pending) == 1 {
			approvalID = pending[0].ID
			break
		}
		time.Sleep(time.Millisecond)
	}
	if approvalID == "" {
		t.Fatal("approval was not registered")
	}
	var record *types.DurableApproval
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		record, err = st.GetApproval(context.Background(), approvalID, task.TenantID)
		if err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err != nil {
		t.Fatalf("durable approval was not written before wait: %v", err)
	}
	if bytes.Contains(record.ActionPayload, []byte("sensitive-value")) {
		t.Fatal("durable action payload contains plaintext")
	}
	plaintext, err := codec.Decrypt(record.ActionPayload)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Action     string         `json:"action"`
		Parameters map[string]any `json:"parameters"`
	}
	if err := json.Unmarshal(plaintext, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Action != "write_file" || envelope.Parameters["content"] != "sensitive-value" {
		t.Fatalf("decrypted envelope = %#v", envelope)
	}
	request, exists, persisted, err := engine.PersistApprovalResolution(context.Background(), task.ID, approvalID, types.ApprovalResult{Approved: true, Message: "ok"})
	if err != nil || !exists || !persisted || request.ID != approvalID {
		t.Fatalf("PersistApprovalResolution = %#v, %v, %v, %v", request, exists, persisted, err)
	}
	resolvedRecord, err := st.GetApproval(context.Background(), approvalID, task.TenantID)
	if err != nil || resolvedRecord.Status != types.ApprovalApproved || resolvedRecord.Version != 2 {
		t.Fatalf("resolved durable approval = %#v, %v", resolvedRecord, err)
	}
	resolution, err := codec.Decrypt(resolvedRecord.ResolutionPayload)
	if err != nil || bytes.Contains(resolvedRecord.ResolutionPayload, []byte("ok")) {
		t.Fatalf("encrypted resolution = %q, decrypt error %v", resolvedRecord.ResolutionPayload, err)
	}
	var approvalResult types.ApprovalResult
	if err := json.Unmarshal(resolution, &approvalResult); err != nil || !approvalResult.Approved {
		t.Fatalf("resolution = %#v, %v", approvalResult, err)
	}
	if _, exists, persisted, err := engine.PersistApprovalResolution(context.Background(), task.ID, approvalID, types.ApprovalResult{Approved: false}); err != nil || !exists || persisted {
		t.Fatalf("replayed PersistApprovalResolution = exists %v persisted %v err %v", exists, persisted, err)
	}
	if !registry.ResolveByID(approvalID, types.ApprovalResult{Approved: true}) {
		t.Fatal("failed to resolve approval")
	}
	if err := <-resultCh; err != nil {
		t.Fatalf("SuspendForApproval: %v", err)
	}
}

func TestRecoverApprovedApprovalConsumesBeforeSingleExecution(t *testing.T) {
	t.Parallel()
	st := storepkg.NewMemoryStore()
	codec, err := approvalcrypto.New(bytes.Repeat([]byte{0x19}, 32))
	if err != nil {
		t.Fatal(err)
	}
	executor := &recoveryExecutor{}
	engine := &Engine{Store: st, ApprovalCodec: codec, Executor: executor}
	task := &types.Task{ID: "recover-task", TenantID: "tenant-a", Goal: "recover", Status: types.StatusAwaitingApproval}
	if err := st.SaveFullTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	actionJSON, _ := json.Marshal(planner.ActionCall{Action: "write_file", Parameters: map[string]any{"path": "x"}})
	ciphertext, err := codec.Encrypt(actionJSON)
	if err != nil {
		t.Fatal(err)
	}
	approval := &types.DurableApproval{
		ID: "recover-approval", TaskID: task.ID, TenantID: task.TenantID,
		Request:       types.ApprovalRequest{ID: "recover-approval", TaskID: task.ID, Action: "write_file", RiskLevel: types.RiskLevelHigh},
		ActionPayload: ciphertext, Status: types.ApprovalApproved,
	}
	if err := st.CreateApproval(context.Background(), approval); err != nil {
		t.Fatal(err)
	}
	recovered, err := engine.RecoverApprovedApproval(context.Background(), task, approval, "owner-a")
	if err != nil || !recovered {
		t.Fatalf("RecoverApprovedApproval = %v, %v", recovered, err)
	}
	if executor.calls != 1 || task.Status != types.StatusPaused || task.StepCount != 1 {
		t.Fatalf("calls=%d status=%s steps=%d", executor.calls, task.Status, task.StepCount)
	}
	stored, err := st.GetApproval(context.Background(), approval.ID, approval.TenantID)
	if err != nil || stored.Status != types.ApprovalConsumed {
		t.Fatalf("stored approval = %#v, %v", stored, err)
	}
	recovered, err = engine.RecoverApprovedApproval(context.Background(), task, approval, "owner-b")
	if err != nil || recovered || executor.calls != 1 {
		t.Fatalf("replayed recovery = %v, %v; calls=%d", recovered, err, executor.calls)
	}
}

func TestPersistUniqueApprovalResolution(t *testing.T) {
	t.Parallel()
	st := storepkg.NewMemoryStore()
	codec, err := approvalcrypto.New(bytes.Repeat([]byte{0x28}, 32))
	if err != nil {
		t.Fatal(err)
	}
	engine := &Engine{Store: st, ApprovalCodec: codec}
	task := &types.Task{ID: "unique-task", TenantID: "tenant-a", Status: types.StatusAwaitingApproval}
	if err := st.SaveFullTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	approval := &types.DurableApproval{
		ID: "unique-approval", TaskID: task.ID, TenantID: task.TenantID,
		Request:       types.ApprovalRequest{ID: "unique-approval", TaskID: task.ID, Action: "run_tests", RiskLevel: types.RiskLevelHigh},
		ActionPayload: []byte("ciphertext"), Status: types.ApprovalPending,
	}
	if err := st.CreateApproval(context.Background(), approval); err != nil {
		t.Fatal(err)
	}
	request, id, count, persisted, err := engine.PersistUniqueApprovalResolution(context.Background(), task.ID, types.ApprovalResult{Approved: false})
	if err != nil || count != 1 || !persisted || id != approval.ID || request.ID != approval.ID {
		t.Fatalf("PersistUniqueApprovalResolution = %#v, %q, %d, %v, %v", request, id, count, persisted, err)
	}
	stored, err := st.GetApproval(context.Background(), approval.ID, task.TenantID)
	if err != nil || stored.Status != types.ApprovalRejected {
		t.Fatalf("stored approval = %#v, %v", stored, err)
	}
}
