package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/wuxujun/ai-agent/internal/approvalcrypto"
	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

type parameterRecoveryExecutor struct {
	calls      int
	parameters map[string]any
}

func (e *parameterRecoveryExecutor) Execute(_ context.Context, _ *types.Task, decision *planner.PlanDecision) ([]types.StepTrace, error) {
	e.calls++
	e.parameters = decision.Actions[0].Parameters
	return nil, nil
}

func TestRecoverApprovedApprovalUsesFinalParameters(t *testing.T) {
	for _, tc := range []struct {
		name       string
		parameters map[string]any
		want       map[string]any
	}{
		{"replacement", map[string]any{"path": "approved.txt"}, map[string]any{"path": "approved.txt"}},
		{"unchanged", nil, map[string]any{"path": "original.txt", "content": "original"}},
		{"empty_replacement", map[string]any{}, map[string]any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			engine, task, approval, executor := recoveryParametersFixture(t)
			_, exists, persisted, err := engine.PersistApprovalResolution(ctx, task.ID, approval.ID, types.ApprovalResult{Approved: true, Parameters: tc.parameters})
			if err != nil || !exists || !persisted {
				t.Fatalf("persist: exists=%v persisted=%v err=%v", exists, persisted, err)
			}
			st := engine.Store.(store.DurableApprovalStore)
			approval, err = st.GetApproval(ctx, approval.ID, task.TenantID)
			if err != nil {
				t.Fatal(err)
			}
			recovered, err := engine.RecoverApprovedApproval(ctx, task, approval, "owner")
			if err != nil || !recovered {
				t.Fatalf("recover=%v err=%v", recovered, err)
			}
			if !reflect.DeepEqual(executor.parameters, tc.want) {
				t.Errorf("executed parameters=%#v, want %#v", executor.parameters, tc.want)
			}
			staleTask := *task
			staleTask.Status = types.StatusAwaitingApproval
			again, err := engine.RecoverApprovedApproval(ctx, &staleTask, approval, "peer")
			if err != nil || again || executor.calls != 1 {
				t.Fatalf("repeat=%v err=%v calls=%d", again, err, executor.calls)
			}
		})
	}
}

func TestRecoverApprovedApprovalRejectsInvalidResolutionBeforeConsumption(t *testing.T) {
	for _, tc := range []struct {
		name, plaintext string
		unsealed        bool
	}{
		{"missing", "", true},
		{"corrupt_ciphertext", "not ciphertext", true},
		{"invalid_json", "not json", false},
		{"null", "null", false},
		{"missing_decision", `{}`, false},
		{"rejected", `{"approved":false}`, false},
		{"invalid_parameters", `{"approved":true,"parameters":[]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine, task, approval, executor := recoveryParametersFixture(t)
			payload := []byte(tc.plaintext)
			var err error
			if !tc.unsealed {
				payload, err = engine.ApprovalCodec.Encrypt(payload)
				if err != nil {
					t.Fatal(err)
				}
			}
			st := engine.Store.(store.DurableApprovalStore)
			ok, err := st.TransitionApproval(context.Background(), approval.ID, task.TenantID, approval.Version, types.ApprovalPending, types.ApprovalApproved, payload)
			if err != nil || !ok {
				t.Fatalf("transition=%v err=%v", ok, err)
			}
			approval, err = st.GetApproval(context.Background(), approval.ID, task.TenantID)
			if err != nil {
				t.Fatal(err)
			}
			recovered, err := engine.RecoverApprovedApproval(context.Background(), task, approval, "owner")
			if err == nil || recovered || executor.calls != 0 {
				t.Fatalf("invalid resolution executed: recovered=%v calls=%d err=%v", recovered, executor.calls, err)
			}
			stored, err := st.GetApproval(context.Background(), approval.ID, task.TenantID)
			if err != nil || stored.Status != types.ApprovalApproved {
				t.Fatalf("invalid resolution consumed: approval=%+v err=%v", stored, err)
			}
		})
	}
}

func recoveryParametersFixture(t *testing.T) (*Engine, *types.Task, *types.DurableApproval, *parameterRecoveryExecutor) {
	t.Helper()
	st := store.NewMemoryStore()
	codec, err := approvalcrypto.New(bytes.Repeat([]byte{0x47}, 32))
	if err != nil {
		t.Fatal(err)
	}
	executor := &parameterRecoveryExecutor{}
	engine := &Engine{Store: st, ApprovalCodec: codec, Executor: executor}
	task := &types.Task{ID: "parameters-task", TenantID: "tenant-a", Status: types.StatusAwaitingApproval}
	if err := st.SaveFullTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(planner.ActionCall{Action: "write_file", Parameters: map[string]any{"path": "original.txt", "content": "original"}})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := codec.Encrypt(raw)
	if err != nil {
		t.Fatal(err)
	}
	approval := &types.DurableApproval{ID: "parameters-approval", TaskID: task.ID, TenantID: task.TenantID, ActionPayload: payload, Status: types.ApprovalPending, Request: types.ApprovalRequest{ID: "parameters-approval", TaskID: task.ID, Action: "write_file", RiskLevel: types.RiskLevelHigh}}
	if err := st.CreateApproval(context.Background(), approval); err != nil {
		t.Fatal(err)
	}
	approval, err = st.GetApproval(context.Background(), approval.ID, task.TenantID)
	if err != nil {
		t.Fatal(err)
	}
	return engine, task, approval, executor
}
