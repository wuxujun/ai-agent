package orchestrator

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/multiagent"
	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/tools"
	"github.com/wuxujun/ai-agent/internal/types"
)

func (e *Engine) preserveDurableApprovalOnCancellation(task *types.Task, err error) bool {
	if task == nil || task.Status != types.StatusAwaitingApproval ||
		(!errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)) {
		return false
	}
	durableStore, ok := e.Store.(store.DurableApprovalStore)
	if !ok || e.ApprovalCodec == nil {
		return false
	}
	tenantID := task.TenantID
	if tenantID == "" {
		tenantID = "default"
	}
	checkCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pending, checkErr := durableStore.ListTaskApprovals(checkCtx, task.ID, tenantID, types.ApprovalPending)
	return checkErr == nil && len(pending) > 0
}

func isRecoverableApprovalTask(task *types.Task) bool {
	if task == nil {
		return false
	}
	if task.Status == types.StatusAwaitingApproval {
		return true
	}
	if task.Status != types.StatusFailed {
		return false
	}
	switch task.TerminationKind {
	case types.TerminationClientCancelled, types.TerminationShutdownRollback, types.TerminationTimeout:
		return true
	case types.TerminationNone:
		// Older rows predate structured termination kinds.
	default:
		return false
	}
	switch task.ErrorCode {
	case "task_canceled", "client_disconnected", "execution_timeout":
		return true
	default:
		return strings.Contains(strings.ToLower(task.FinalAnswer), "context canceled")
	}
}

// enforceApprovals gates every RiskLevelHigh action in the decision behind the
// human approval flow before any tool runs. It returns rejected=true if the user
// rejected an action (the rejection trace is appended by SuspendForApproval, so
// the caller should skip execution and let the planner adapt next cycle); err is
// non-nil only on approval-flow failure (e.g. context cancellation).
//
// This is the single source of truth for high-risk gating across orchestrator
// modes. It MUST be called before Executor.Execute in every mode that runs an
// LLM-chosen PlanDecision — previously the gate lived only in runLegacyNext, so
// the default eino mode (and step/adk) executed write_file/execute_code with no
// approval at all (see BUG_REPORT.md #1).
func (e *Engine) enforceApprovals(ctx context.Context, task *types.Task, decision *planner.PlanDecision) (rejected bool, err error) {
	for i := range decision.Actions {
		ac := &decision.Actions[i]
		tool, ok := tools.Get(ac.Action)
		if !ok || tool.RiskLevel() != types.RiskLevelHigh {
			continue
		}
		approved, newParams, apErr := e.SuspendForApproval(ctx, task, ac.Action, ac.Parameters)
		if apErr != nil {
			engineLog.Error("action approval error", "task_id", task.ID, "action", ac.Action, "error", apErr)
			return false, apErr
		}
		if !approved {
			return true, nil
		}
		if newParams != nil {
			ac.Parameters = newParams
		}
	}
	return false, executionLeaseError(ctx)
}

// approvalStore returns the ApprovalStore this engine should use. When the
// Approvals field is set (e.g. in tests) that instance is used; otherwise the
// process-wide defaultApprovals singleton is returned.
func (e *Engine) approvalStore() *ApprovalStore {
	if e.Approvals != nil {
		return e.Approvals
	}
	return defaultApprovals
}

var ErrApprovalExpired = errors.New("approval has expired")

// ExpireDurableApproval atomically expires a stale pending approval according
// to the hot-reloadable TTL. It is safe for concurrent maintenance workers.
func (e *Engine) ExpireDurableApproval(ctx context.Context, durableStore store.DurableApprovalStore, record *types.DurableApproval) (bool, error) {
	ttlSeconds := config.Get().Approval.TTLSeconds
	if ttlSeconds <= 0 || record == nil || record.Status != types.ApprovalPending || record.CreatedAt.IsZero() || time.Now().Before(record.CreatedAt.Add(time.Duration(ttlSeconds)*time.Second)) {
		return false, nil
	}
	expired, err := durableStore.TransitionApproval(ctx, record.ID, record.TenantID, record.Version, types.ApprovalPending, types.ApprovalExpired, record.ResolutionPayload)
	if err == nil && expired && e.Metrics != nil {
		e.Metrics.ObserveDurableApproval(ctx, "expired")
	}
	return expired, err
}

// PersistApprovalResolution records an explicit approve/reject decision before
// any in-memory or Pub/Sub notification. found=false means durable approvals
// are disabled or the ID does not belong to this task/tenant.
func (e *Engine) PersistApprovalResolution(ctx context.Context, taskID, approvalID string, result types.ApprovalResult) (*types.ApprovalRequest, bool, bool, error) {
	durableStore, ok := e.Store.(store.DurableApprovalStore)
	if !ok || e.ApprovalCodec == nil {
		return nil, false, false, nil
	}
	task, err := e.Store.GetTask(ctx, taskID)
	if err != nil {
		return nil, false, false, err
	}
	tenantID := task.TenantID
	if tenantID == "" {
		tenantID = "default"
	}
	record, err := durableStore.GetApproval(ctx, approvalID, tenantID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, false, nil
	}
	if err != nil {
		return nil, false, false, err
	}
	if record.TaskID != taskID {
		return nil, false, false, nil
	}
	request := record.Request
	if expired, expireErr := e.ExpireDurableApproval(ctx, durableStore, record); expireErr != nil {
		return nil, true, false, expireErr
	} else if expired || record.Status == types.ApprovalExpired {
		return &request, true, false, ErrApprovalExpired
	}
	if record.Status != types.ApprovalPending {
		return &request, true, false, nil
	}
	// Empty replacement parameters and absent parameters have different
	// approval semantics. Override ApprovalResult's omitempty field so the
	// durable representation preserves {} instead of restoring old arguments.
	resolutionJSON, err := json.Marshal(struct {
		types.ApprovalResult
		Parameters map[string]any `json:"parameters"`
	}{ApprovalResult: result, Parameters: result.Parameters})
	if err != nil {
		return nil, true, false, fmt.Errorf("marshal durable approval resolution: %w", err)
	}
	ciphertext, err := e.ApprovalCodec.Encrypt(resolutionJSON)
	if err != nil {
		return nil, true, false, fmt.Errorf("encrypt durable approval resolution: %w", err)
	}
	target := types.ApprovalRejected
	if result.Approved {
		target = types.ApprovalApproved
	}
	matched, err := durableStore.TransitionApproval(ctx, approvalID, tenantID, record.Version, types.ApprovalPending, target, ciphertext)
	if err != nil {
		return nil, true, false, err
	}
	if !matched {
		return &request, true, false, nil
	}
	if e.Metrics != nil {
		event := "rejected"
		if result.Approved {
			event = "approved"
		}
		e.Metrics.ObserveDurableApproval(ctx, event)
	}
	return &request, true, true, nil
}

// PersistUniqueApprovalResolution resolves the only durable pending approval
// for a task. count lets the HTTP layer distinguish absent from ambiguous.
func (e *Engine) PersistUniqueApprovalResolution(ctx context.Context, taskID string, result types.ApprovalResult) (*types.ApprovalRequest, string, int, bool, error) {
	durableStore, ok := e.Store.(store.DurableApprovalStore)
	if !ok || e.ApprovalCodec == nil {
		return nil, "", 0, false, nil
	}
	task, err := e.Store.GetTask(ctx, taskID)
	if err != nil {
		return nil, "", 0, false, err
	}
	tenantID := task.TenantID
	if tenantID == "" {
		tenantID = "default"
	}
	pending, err := durableStore.ListTaskApprovals(ctx, taskID, tenantID, types.ApprovalPending)
	if err != nil {
		return nil, "", 0, false, err
	}
	active := pending[:0]
	for _, record := range pending {
		expired, expireErr := e.ExpireDurableApproval(ctx, durableStore, record)
		if expireErr != nil {
			return nil, "", 0, false, expireErr
		}
		if !expired {
			active = append(active, record)
		}
	}
	pending = active
	if len(pending) != 1 {
		return nil, "", len(pending), false, nil
	}
	request, exists, persisted, err := e.PersistApprovalResolution(ctx, taskID, pending[0].ID, result)
	if err != nil {
		return nil, pending[0].ID, 1, false, err
	}
	if !exists {
		return nil, pending[0].ID, 0, false, nil
	}
	return request, pending[0].ID, 1, persisted, nil
}

func (e *Engine) consumeDurableApproval(ctx context.Context, task *types.Task, approvalID string, result types.ApprovalResult) error {
	durableStore, ok := e.Store.(store.DurableApprovalStore)
	if !ok || e.ApprovalCodec == nil {
		return nil
	}
	tenantID := task.TenantID
	if tenantID == "" {
		tenantID = "default"
	}
	record, err := durableStore.GetApproval(ctx, approvalID, tenantID)
	if err != nil {
		return err
	}
	want := types.ApprovalRejected
	if result.Approved {
		want = types.ApprovalApproved
	}
	if record.Status == types.ApprovalPending {
		_, exists, persisted, persistErr := e.PersistApprovalResolution(ctx, task.ID, approvalID, result)
		if persistErr != nil {
			return persistErr
		}
		if !exists || !persisted {
			return errors.New("approval resolution lost a concurrent persistence race")
		}
		record, err = durableStore.GetApproval(ctx, approvalID, tenantID)
		if err != nil {
			return err
		}
	}
	if record.Status != want {
		return fmt.Errorf("approval has status %s, expected %s", record.Status, want)
	}
	consumed, err := durableStore.TransitionApproval(ctx, approvalID, tenantID, record.Version, want, types.ApprovalConsumed, record.ResolutionPayload)
	if err != nil {
		return err
	}
	if !consumed {
		if e.Metrics != nil {
			e.Metrics.ObserveDurableApproval(ctx, "conflict")
		}
		return errors.New("approval was already consumed")
	}
	if e.Metrics != nil {
		e.Metrics.ObserveDurableApproval(ctx, "consumed")
	}
	return nil
}

// RecoverApprovedApproval consumes and executes one approved action checkpoint.
// The approved->consumed CAS occurs before tool execution, providing at-most-once
// consumption across instances. A crash in the narrow post-CAS/pre-tool window
// intentionally favors avoiding duplicate high-risk side effects over retrying.
func (e *Engine) RecoverApprovedApproval(ctx context.Context, task *types.Task, approval *types.DurableApproval, owner string) (bool, error) {
	durableStore, ok := e.Store.(store.DurableApprovalStore)
	if !ok || e.ApprovalCodec == nil || e.Executor == nil || task == nil || approval == nil {
		return false, nil
	}
	if task.ID != approval.TaskID || approval.Status != types.ApprovalApproved || !isRecoverableApprovalTask(task) {
		return false, nil
	}
	ctx, releaseTaskLease, err := e.beginApprovalRecovery(ctx, task, owner)
	if err != nil {
		return false, err
	}
	defer releaseTaskLease()
	if !isRecoverableApprovalTask(task) {
		return false, nil
	}
	if err := recoveryExecutionError(ctx); err != nil {
		return false, err
	}
	acquired, err := durableStore.AcquireApprovalLease(ctx, approval.ID, owner, time.Minute)
	if err != nil || !acquired {
		return false, err
	}
	defer func() { _ = durableStore.ReleaseApprovalLease(context.Background(), approval.ID, owner) }()

	latest, err := durableStore.GetApproval(ctx, approval.ID, approval.TenantID)
	if err != nil || latest.Status != types.ApprovalApproved {
		return false, err
	}
	plaintext, err := e.ApprovalCodec.Decrypt(latest.ActionPayload)
	if err != nil {
		return false, fmt.Errorf("decrypt approved action checkpoint: %w", err)
	}
	var action planner.ActionCall
	if err := json.Unmarshal(plaintext, &action); err != nil {
		return false, fmt.Errorf("decode approved action checkpoint: %w", err)
	}
	if action.Action == "" || action.Action != latest.Request.Action {
		return false, errors.New("approved action checkpoint identity mismatch")
	}
	if len(latest.ResolutionPayload) == 0 {
		return false, errors.New("approved action checkpoint has no resolution")
	}
	resolutionJSON, err := e.ApprovalCodec.Decrypt(latest.ResolutionPayload)
	if err != nil {
		return false, fmt.Errorf("decrypt approved action resolution: %w", err)
	}
	var resolution types.ApprovalResult
	if err := json.Unmarshal(resolutionJSON, &resolution); err != nil {
		return false, errors.New("invalid approved action resolution")
	}
	if !resolution.Approved {
		return false, errors.New("approved action resolution is not an approval")
	}
	if resolution.Parameters != nil {
		action.Parameters = resolution.Parameters
	}
	if err := recoveryExecutionError(ctx); err != nil {
		return false, err
	}
	consumed, err := durableStore.TransitionApproval(ctx, latest.ID, latest.TenantID, latest.Version, types.ApprovalApproved, types.ApprovalConsumed, latest.ResolutionPayload)
	if err != nil {
		return false, err
	}
	if !consumed {
		if e.Metrics != nil {
			e.Metrics.ObserveDurableApproval(ctx, "conflict")
		}
		return false, nil
	}
	if e.Metrics != nil {
		e.Metrics.ObserveDurableApproval(ctx, "consumed")
	}

	if err := recoveryExecutionError(ctx); err != nil {
		return true, err
	}
	traces, executeErr := e.Executor.Execute(ctx, task, &planner.PlanDecision{Actions: []planner.ActionCall{action}})
	task.Trace = append(task.Trace, traces...)
	task.StepCount += len(traces)
	task.Status = types.StatusPaused
	if executeErr != nil {
		task.Trace = append(task.Trace, types.StepTrace{
			Step: task.StepCount + 1, Goal: task.Goal, Action: action.Action,
			Observation: "approved recovery action failed", Error: executeErr.Error(),
		})
		task.StepCount++
	}
	if err := executionLeaseError(ctx); err != nil {
		return true, err
	}
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	saveErr := e.Store.SaveFullTask(saveCtx, task)
	cancel()
	if saveErr != nil {
		return true, fmt.Errorf("save recovered approval action: %w", saveErr)
	}
	if err := executionLeaseError(ctx); err != nil {
		return true, err
	}
	if e.EventCallback != nil {
		e.EventCallback(task.ID, types.StatusPaused)
	}
	if e.Metrics != nil {
		e.Metrics.ObserveDurableApproval(ctx, "recovery_success")
	}
	return true, executeErr
}

// RecoverRejectedApproval consumes a persisted rejection and restores its
// feedback trace without executing the action checkpoint.
func (e *Engine) RecoverRejectedApproval(ctx context.Context, task *types.Task, approval *types.DurableApproval, owner string) (bool, error) {
	durableStore, ok := e.Store.(store.DurableApprovalStore)
	if !ok || e.ApprovalCodec == nil || task == nil || approval == nil {
		return false, nil
	}
	if task.ID != approval.TaskID || approval.Status != types.ApprovalRejected || !isRecoverableApprovalTask(task) {
		return false, nil
	}
	ctx, releaseTaskLease, err := e.beginApprovalRecovery(ctx, task, owner)
	if err != nil {
		return false, err
	}
	defer releaseTaskLease()
	if !isRecoverableApprovalTask(task) {
		return false, nil
	}
	if err := recoveryExecutionError(ctx); err != nil {
		return false, err
	}
	acquired, err := durableStore.AcquireApprovalLease(ctx, approval.ID, owner, time.Minute)
	if err != nil || !acquired {
		return false, err
	}
	defer func() { _ = durableStore.ReleaseApprovalLease(context.Background(), approval.ID, owner) }()
	latest, err := durableStore.GetApproval(ctx, approval.ID, approval.TenantID)
	if err != nil || latest.Status != types.ApprovalRejected {
		return false, err
	}
	plaintext, err := e.ApprovalCodec.Decrypt(latest.ResolutionPayload)
	if err != nil {
		return false, fmt.Errorf("decrypt rejected approval result: %w", err)
	}
	var result types.ApprovalResult
	if err := json.Unmarshal(plaintext, &result); err != nil {
		return false, fmt.Errorf("decode rejected approval result: %w", err)
	}
	if err := recoveryExecutionError(ctx); err != nil {
		return false, err
	}
	consumed, err := durableStore.TransitionApproval(ctx, latest.ID, latest.TenantID, latest.Version, types.ApprovalRejected, types.ApprovalConsumed, latest.ResolutionPayload)
	if err != nil {
		return false, err
	}
	if !consumed {
		if e.Metrics != nil {
			e.Metrics.ObserveDurableApproval(ctx, "conflict")
		}
		return false, nil
	}
	if e.Metrics != nil {
		e.Metrics.ObserveDurableApproval(ctx, "consumed")
	}
	message := result.Message
	if message == "" {
		message = "No reason provided"
	}
	task.Trace = append(task.Trace, types.StepTrace{
		Step: task.StepCount + 1, Goal: task.Goal, Action: latest.Request.Action,
		Observation: fmt.Sprintf("Action rejected by user. Reason: %s", message),
		Error:       fmt.Sprintf("Action %s rejected by user: %s", latest.Request.Action, message),
		Evidence:    []types.Evidence{{Path: "user_feedback", Lines: []string{message}, Query: "disapproval"}},
	})
	task.StepCount++
	task.Status = types.StatusPaused
	if err := executionLeaseError(ctx); err != nil {
		return true, err
	}
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	saveErr := e.Store.SaveFullTask(saveCtx, task)
	cancel()
	if saveErr != nil {
		return true, fmt.Errorf("save recovered rejection: %w", saveErr)
	}
	if err := executionLeaseError(ctx); err != nil {
		return true, err
	}
	if e.EventCallback != nil {
		e.EventCallback(task.ID, types.StatusPaused)
	}
	if e.Metrics != nil {
		e.Metrics.ObserveDurableApproval(ctx, "recovery_success")
	}
	return true, nil
}

func (e *Engine) SuspendForApproval(ctx context.Context, task *types.Task, action string, params map[string]any) (bool, map[string]any, error) {
	if err := executionLeaseError(ctx); err != nil {
		return false, nil, err
	}
	approval := e.BuildApprovalRequest(task, action, params)
	approvalRegistry := e.approvalStore()
	approvalID, ch := approvalRegistry.Register(task.ID, approval)
	defer approvalRegistry.Remove(approvalID)
	if durableStore, ok := e.Store.(store.DurableApprovalStore); ok && e.ApprovalCodec != nil {
		actionJSON, err := json.Marshal(struct {
			Action     string         `json:"action"`
			Parameters map[string]any `json:"parameters"`
		}{Action: action, Parameters: params})
		if err != nil {
			return false, nil, fmt.Errorf("marshal durable approval action: %w", err)
		}
		ciphertext, err := e.ApprovalCodec.Encrypt(actionJSON)
		if err != nil {
			return false, nil, fmt.Errorf("encrypt durable approval action: %w", err)
		}
		tenantID := task.TenantID
		if tenantID == "" {
			tenantID = "default"
		}
		persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		err = durableStore.CreateApproval(persistCtx, &types.DurableApproval{
			ID: approvalID, TaskID: task.ID, TenantID: tenantID, Request: *approval,
			ActionPayload: ciphertext, Status: types.ApprovalPending,
		})
		cancel()
		if err != nil {
			return false, nil, fmt.Errorf("persist durable approval: %w", err)
		}
		if e.Metrics != nil {
			e.Metrics.ObserveDurableApproval(context.Background(), "created")
		}
	}

	task.Status = types.StatusAwaitingApproval
	if e.Store != nil {
		// Persistence must survive caller ctx expiry: the caller's ctx carries
		// the run-all wall-clock budget and the user's approval-wait window,
		// neither of which should be allowed to abort the awaiting_approval
		// write. Without this, a task could observe ctx.Done() below and leave
		// the DB row in a pre-suspend state, looking lost across restarts.
		saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		err := e.Store.SaveFullTask(saveCtx, task)
		cancel()
		if err != nil {
			return false, nil, err
		}
	}
	if e.EventCallback != nil {
		e.EventCallback(task.ID, types.StatusAwaitingApproval)
	}
	if e.ApprovalCallback != nil {
		e.ApprovalCallback(task.ID, approval)
	}

	resumeExecutionTimeout := PauseExecutionTimeout(ctx)
	defer resumeExecutionTimeout()
	var res types.ApprovalResult
	select {
	case res = <-ch:
	case <-ctx.Done():
		select {
		case res = <-ch:
		default:
			return false, nil, ctx.Err()
		}
	}
	resumeExecutionTimeout()
	if err := executionLeaseError(ctx); err != nil {
		return false, nil, err
	}
	consumeCtx, consumeCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	consumeErr := e.consumeDurableApproval(consumeCtx, task, approvalID, res)
	consumeCancel()
	if consumeErr != nil {
		return false, nil, fmt.Errorf("consume durable approval: %w", consumeErr)
	}

	if err := executionLeaseError(ctx); err != nil {
		return false, nil, err
	}
	if !res.Approved {
		msg := res.Message
		if msg == "" {
			msg = "No reason provided"
		}
		role := types.AgentRoleSingle
		if e.effectiveMode(task) == ModeMultiAgent {
			role = types.AgentRoleResearcher
			if contextRole, ok := multiagent.ApprovalAgentRoleFromContext(ctx); ok {
				role = contextRole
			}
		}
		task.Trace = append(task.Trace, types.StepTrace{
			Step:        task.StepCount + 1,
			Goal:        task.Goal,
			Action:      action,
			Observation: fmt.Sprintf("Action rejected by user. Reason: %s", msg),
			Error:       fmt.Sprintf("Action %s rejected by user: %s", action, msg),
			Evidence: []types.Evidence{{
				Path:  "user_feedback",
				Lines: []string{msg},
				Query: "disapproval",
			}},
			AgentRole: role,
		})
		task.StepCount += 1

		task.Status = types.StatusRunning
		if e.Store != nil {
			saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			saveErr := e.Store.SaveFullTask(saveCtx, task)
			cancel()
			if saveErr != nil {
				return false, nil, saveErr
			}
		}
		if e.EventCallback != nil {
			e.EventCallback(task.ID, types.StatusRunning)
		}
		return false, nil, nil
	}

	task.Status = types.StatusRunning
	if e.Store != nil {
		saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		saveErr := e.Store.SaveFullTask(saveCtx, task)
		cancel()
		if saveErr != nil {
			return false, nil, saveErr
		}
	}
	if e.EventCallback != nil {
		e.EventCallback(task.ID, types.StatusRunning)
	}
	return true, res.Parameters, nil
}
