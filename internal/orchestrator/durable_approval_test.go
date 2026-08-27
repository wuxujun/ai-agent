package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/approvalcrypto"
	"github.com/wuxujun/ai-agent/internal/config"
	metricspkg "github.com/wuxujun/ai-agent/internal/metrics"
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
	collector := metricspkg.NewCollector()
	engine := &Engine{Store: st, Approvals: registry, ApprovalCodec: codec, Metrics: collector}
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
	consumedRecord, err := st.GetApproval(context.Background(), approvalID, task.TenantID)
	if err != nil || consumedRecord.Status != types.ApprovalConsumed || consumedRecord.Version != 3 {
		t.Fatalf("online consumed approval = %#v, %v", consumedRecord, err)
	}
	metricsSnapshot := collector.Snapshot()
	if metricsSnapshot.DurableApprovalsCreated != 1 || metricsSnapshot.DurableApprovalsApproved != 1 || metricsSnapshot.DurableApprovalsConsumed != 1 {
		t.Fatalf("durable approval lifecycle metrics = %+v", metricsSnapshot)
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

func TestPersistApprovalResolutionExpiresStalePendingApproval(t *testing.T) {
	restore := config.OverrideForTesting(func(cfg *config.Config) { cfg.Approval.TTLSeconds = 1 })
	t.Cleanup(restore)
	st := storepkg.NewMemoryStore()
	codec, err := approvalcrypto.New(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	collector := metricspkg.NewCollector()
	engine := &Engine{Store: st, ApprovalCodec: codec, Metrics: collector}
	task := &types.Task{ID: "expired-task", TenantID: "tenant-a", Status: types.StatusAwaitingApproval}
	if err := st.SaveFullTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	approval := &types.DurableApproval{
		ID: "expired-approval", TaskID: task.ID, TenantID: task.TenantID,
		Request:       types.ApprovalRequest{ID: "expired-approval", TaskID: task.ID, Action: "write_file", RiskLevel: types.RiskLevelHigh},
		ActionPayload: []byte("ciphertext"), Status: types.ApprovalPending, CreatedAt: time.Now().Add(-2 * time.Second),
	}
	if err := st.CreateApproval(context.Background(), approval); err != nil {
		t.Fatal(err)
	}
	_, exists, persisted, err := engine.PersistApprovalResolution(context.Background(), task.ID, approval.ID, types.ApprovalResult{Approved: true})
	if !errors.Is(err, ErrApprovalExpired) || !exists || persisted {
		t.Fatalf("PersistApprovalResolution = exists %v persisted %v err %v", exists, persisted, err)
	}
	stored, err := st.GetApproval(context.Background(), approval.ID, approval.TenantID)
	if err != nil || stored.Status != types.ApprovalExpired || stored.Version != 2 {
		t.Fatalf("expired approval = %#v, %v", stored, err)
	}
	if collector.Snapshot().DurableApprovalsExpired != 1 {
		t.Fatalf("expired metrics = %+v", collector.Snapshot())
	}
}

func TestRecoverRejectedApprovalConsumesAndRestoresFeedback(t *testing.T) {
	t.Parallel()
	st := storepkg.NewMemoryStore()
	codec, err := approvalcrypto.New(bytes.Repeat([]byte{0x51}, 32))
	if err != nil {
		t.Fatal(err)
	}
	engine := &Engine{Store: st, ApprovalCodec: codec}
	task := &types.Task{ID: "rejected-task", TenantID: "tenant-a", Goal: "write", Status: types.StatusAwaitingApproval}
	if err := st.SaveFullTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	resolutionJSON, _ := json.Marshal(types.ApprovalResult{Approved: false, Message: "use another path"})
	resolutionPayload, err := codec.Encrypt(resolutionJSON)
	if err != nil {
		t.Fatal(err)
	}
	approval := &types.DurableApproval{
		ID: "rejected-approval", TaskID: task.ID, TenantID: task.TenantID,
		Request:           types.ApprovalRequest{ID: "rejected-approval", TaskID: task.ID, Action: "write_file", RiskLevel: types.RiskLevelHigh},
		ActionPayload:     []byte("ciphertext"),
		ResolutionPayload: resolutionPayload,
		Status:            types.ApprovalRejected,
	}
	if err := st.CreateApproval(context.Background(), approval); err != nil {
		t.Fatal(err)
	}
	recovered, err := engine.RecoverRejectedApproval(context.Background(), task, approval, "owner-a")
	if err != nil || !recovered {
		t.Fatalf("RecoverRejectedApproval = %v, %v", recovered, err)
	}
	if task.Status != types.StatusPaused || task.StepCount != 1 || len(task.Trace) != 1 || task.Trace[0].Step != 1 || task.Trace[0].Evidence[0].Lines[0] != "use another path" {
		t.Fatalf("recovered task = %#v", task)
	}
	stored, err := st.GetApproval(context.Background(), approval.ID, approval.TenantID)
	if err != nil || stored.Status != types.ApprovalConsumed {
		t.Fatalf("stored approval = %#v, %v", stored, err)
	}
}

func TestRunAllCancellationPreservesDurableAwaitingApproval(t *testing.T) {
	t.Parallel()
	st := storepkg.NewMemoryStore()
	codec, err := approvalcrypto.New(bytes.Repeat([]byte{0x63}, 32))
	if err != nil {
		t.Fatal(err)
	}
	engine := &Engine{Store: st, ApprovalCodec: codec}
	task := &types.Task{ID: "cancel-awaiting", TenantID: "tenant-a", Status: types.StatusAwaitingApproval}
	if err := st.SaveFullTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	approval := &types.DurableApproval{
		ID: "cancel-awaiting-approval", TaskID: task.ID, TenantID: task.TenantID,
		Request:       types.ApprovalRequest{ID: "cancel-awaiting-approval", TaskID: task.ID, Action: "write_file", RiskLevel: types.RiskLevelHigh},
		ActionPayload: []byte("ciphertext"), Status: types.ApprovalPending,
	}
	if err := st.CreateApproval(context.Background(), approval); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := engine.RunAll(ctx, task); err != context.Canceled {
		t.Fatalf("RunAll error = %v", err)
	}
	if task.Status != types.StatusAwaitingApproval || task.FinalAnswer != "" {
		t.Fatalf("task cancellation state = status %s answer %q", task.Status, task.FinalAnswer)
	}
}

func TestRecoverableApprovalTaskRecognizesStructuredCancellation(t *testing.T) {
	for _, code := range []string{"task_canceled", "client_disconnected", "execution_timeout"} {
		t.Run(code, func(t *testing.T) {
			task := &types.Task{Status: types.StatusFailed, ErrorCode: code}
			if !isRecoverableApprovalTask(task) {
				t.Fatalf("structured cancellation %q should remain recoverable", code)
			}
		})
	}
}
