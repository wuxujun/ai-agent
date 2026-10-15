package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wuxujun/ai-agent/internal/approvalcrypto"
	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/evidenceconflict"
	"github.com/wuxujun/ai-agent/internal/evidencefilter"
	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/promptguard"
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
		{"replacement", map[string]any{"path": "approved.txt", "content": "approved"}, map[string]any{"path": "approved.txt", "content": "approved"}},
		{"unchanged", nil, map[string]any{"path": "original.txt", "content": "original"}},
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
	task := &types.Task{ID: "parameters-task", TenantID: "tenant-a", Status: types.StatusAwaitingApproval, MaxSteps: 10, ToolBudget: 10}
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

func TestApprovalRecoveryRejectsInvalidArgumentsAndExhaustedBudget(t *testing.T) {
	for _, scenario := range []string{"empty_parameters", "missing_content", "traversal", "tool_budget", "steps", "tokens"} {
		t.Run(scenario, func(t *testing.T) {
			e, task, approval, executor := recoveryParametersFixture(t)
			var params map[string]any
			switch scenario {
			case "empty_parameters":
				params = map[string]any{}
			case "missing_content":
				params = map[string]any{"path": "x"}
			case "traversal":
				params = map[string]any{"path": "../outside", "content": "x"}
			case "tool_budget":
				task.ToolBudget = 0
			case "steps":
				task.StepCount = task.MaxSteps
			case "tokens":
				task.TokenBudget = 1
				task.Trace = []types.StepTrace{{TokenUsage: types.TokenUsage{TotalTokens: 1}}}
			}
			if err := e.Store.SaveFullTask(t.Context(), task); err != nil {
				t.Fatal(err)
			}
			if _, _, _, err := e.PersistApprovalResolution(t.Context(), task.ID, approval.ID, types.ApprovalResult{Approved: true, Parameters: params}); err != nil {
				t.Fatal(err)
			}
			st := e.Store.(store.DurableApprovalStore)
			approval, err := st.GetApproval(t.Context(), approval.ID, task.TenantID)
			if err != nil {
				t.Fatal(err)
			}
			got, err := e.RecoverApprovedApproval(t.Context(), task, approval, "owner")
			if got || err == nil || executor.calls != 0 {
				t.Fatalf("recovered=%v err=%v calls=%d", got, err, executor.calls)
			}
			after, err := st.GetApproval(t.Context(), approval.ID, task.TenantID)
			if err != nil || after.Status != types.ApprovalApproved {
				t.Fatalf("consumed invalid approval: %+v %v", after, err)
			}
		})
	}
}

type recoveryExternalExecutor struct{}

func (recoveryExternalExecutor) Execute(context.Context, *types.Task, *planner.PlanDecision) ([]types.StepTrace, error) {
	return []types.StepTrace{{Action: "http_fetch", Query: "version", Observation: "advertisement\nversion is 2"}}, nil
}

func TestApprovalRecoveryAuditsExternalOutputAndChargesBudget(t *testing.T) {
	for _, quarantine := range []bool{true, false} {
		t.Run(map[bool]string{true: "quarantine", false: "relevance_and_conflict"}[quarantine], func(t *testing.T) {
			e, task, approval, _ := recoveryParametersFixture(t)
			e.Executor = recoveryExternalExecutor{}
			if quarantine {
				e.PromptInjectionDetector = &fixedInjectionDetector{}
			} else {
				e.EvidenceRelevanceFilter = &fixedEvidenceFilter{}
				e.EvidenceConflictResolver = &fixedEvidenceConflictResolver{}
				e.LLMSceneEnabled = func(scene string) bool { return scene == config.LLMSceneEvidenceConflictResolver }
				task.Trace = []types.StepTrace{{Action: "http_fetch", Observation: "version is 1"}}
			}
			task.ToolBudget = 1
			if err := e.Store.SaveFullTask(t.Context(), task); err != nil {
				t.Fatal(err)
			}
			if _, _, _, err := e.PersistApprovalResolution(t.Context(), task.ID, approval.ID, types.ApprovalResult{Approved: true}); err != nil {
				t.Fatal(err)
			}
			approval, err := e.Store.(store.DurableApprovalStore).GetApproval(t.Context(), approval.ID, task.TenantID)
			if err != nil {
				t.Fatal(err)
			}
			recovered, err := e.RecoverApprovedApproval(t.Context(), task, approval, "owner")
			if err != nil || !recovered {
				t.Fatal(recovered, err)
			}
			saved, err := e.Store.GetTask(t.Context(), task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if saved.ToolBudget != 0 || saved.StepCount != 1 {
				t.Fatalf("budget=%d steps=%d", saved.ToolBudget, saved.StepCount)
			}
			seen := map[string]bool{}
			for _, tr := range saved.Trace {
				seen[tr.Action] = true
			}
			if quarantine {
				if !seen[promptguard.TraceAction] || !strings.Contains(saved.Trace[0].Observation, "quarantined") {
					t.Fatalf("unsafe trace persisted: %+v", saved.Trace)
				}
			} else if !seen[evidencefilter.TraceAction] || !seen[evidenceconflict.TraceAction] {
				t.Fatalf("missing audits: %+v", saved.Trace)
			}
		})
	}
}

func TestLiveApprovalRevalidatesReplacementBeforeExecutor(t *testing.T) {
	registry := NewApprovalStore()
	executor := &parameterRecoveryExecutor{}
	engine := &Engine{Approvals: registry, Executor: executor, Planner: &stubPlanner{decision: &planner.PlanDecision{Actions: []planner.ActionCall{{Action: "write_file", Parameters: map[string]any{"path": "out.txt", "content": "ok"}}}}}}
	engine.ApprovalCallback = func(taskID string, _ *types.ApprovalRequest) {
		if !registry.Resolve(taskID, types.ApprovalResult{Approved: true, Parameters: map[string]any{}}) {
			t.Error("approval not found")
		}
	}
	task := &types.Task{ID: "invalid-live-approval", Status: types.StatusCreated, MaxSteps: 10, ToolBudget: 10}
	err := engine.Next(t.Context(), task)
	if err == nil || executor.calls != 0 {
		t.Fatalf("invalid approved arguments executed: err=%v calls=%d", err, executor.calls)
	}
}

type failingRecoveryExecutor struct{}

func (failingRecoveryExecutor) Execute(context.Context, *types.Task, *planner.PlanDecision) ([]types.StepTrace, error) {
	return nil, errors.New("fixture failure")
}
func TestApprovalRecoveryFailedAttemptStillChargesBudget(t *testing.T) {
	e, task, approval, _ := recoveryParametersFixture(t)
	e.Executor = failingRecoveryExecutor{}
	if _, _, _, err := e.PersistApprovalResolution(t.Context(), task.ID, approval.ID, types.ApprovalResult{Approved: true}); err != nil {
		t.Fatal(err)
	}
	approval, err := e.Store.(store.DurableApprovalStore).GetApproval(t.Context(), approval.ID, task.TenantID)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := e.RecoverApprovedApproval(t.Context(), task, approval, "owner")
	if !recovered || err == nil || task.ToolBudget != 9 || task.StepCount != 1 || len(task.Trace) != 1 || task.Trace[0].Step != 1 {
		t.Fatalf("task=%+v recovered=%v err=%v", task, recovered, err)
	}
}
