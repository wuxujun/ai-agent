package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync/atomic"
	"time"

	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
	"go.opentelemetry.io/otel/trace"
)

// SQLiteOwnedExecutionIdentity is a copy, not a mutable business Task or a
// credential. Cleanup must wait for actual scope-owned tool cleanup to finish.
type SQLiteOwnedExecutionIdentity struct {
	Origin, NamespaceSHA256                     string
	TaskID, TenantID, AttemptID, InstanceSHA256 string
	TaskIncarnation, GateEpoch                  int64
}

type SQLiteOwnedExecutionOptions struct {
	// OnAdmitted is a trusted notification after durable registration and monitor start.
	OnAdmitted                func(SQLiteOwnedExecutionIdentity)
	AttemptID, InstanceSHA256 string
	ExpectedGateEpoch         int64
	TTL                       time.Duration
	RunAll                    bool
	Cleanup                   func(context.Context, SQLiteOwnedExecutionIdentity) (string, error)
}

var ErrSQLiteOwnedCleanup = errors.New("sqlite owned execution cleanup unconfirmed")
var ErrSQLiteOwnedFlush = errors.New("sqlite owned execution final flush unconfirmed")

// RunOwnedSQLiteExecution is an explicit fixture integration, never selected by
// configuration or the HTTP API. It retains ownership through Engine return,
// final flush and cleanup, then stops renewal BEFORE atomically finalizing and
// releasing the lease. Failed stages leave exited/unresolved durable records.
// It does not authenticate an instance, reconcile a host or prove stopped.
func (e *Engine) RunOwnedSQLiteExecution(ctx context.Context, st *store.SQLiteStore, task *types.Task, options SQLiteOwnedExecutionOptions) error {
	return e.runOwnedSQLiteExecution(ctx, st, task, options, nil)
}

type sqliteOwnedScopeKey struct{}
type sqliteOwnedScope struct {
	store    *store.SQLiteStore
	identity SQLiteOwnedExecutionIdentity
}

// PreparedSQLiteOwnedExecution retains the sole durable owner while a trusted
// adapter performs pre-response CAS and queues execution. Call exactly one of
// Execute or Abort after a successful Prepare; both own final flush and cleanup.
// The task and Context must not be used concurrently with that final operation.
type PreparedSQLiteOwnedExecution struct {
	engine   *Engine
	store    *store.SQLiteStore
	task     *types.Task
	options  SQLiteOwnedExecutionOptions
	identity SQLiteOwnedExecutionIdentity
	ctx      context.Context
	cancel   context.CancelCauseFunc
	lease    *store.ExecutionLease
	loaded   bool
	consumed atomic.Bool
}

func (s *PreparedSQLiteOwnedExecution) Context() context.Context { return s.ctx }

func (e *Engine) PrepareOwnedSQLiteExecution(ctx context.Context, st *store.SQLiteStore, task *types.Task, options SQLiteOwnedExecutionOptions) (*PreparedSQLiteOwnedExecution, error) {
	if st == nil || task == nil || e.Store != st || e.ExecutionAdmission != st || options.Cleanup == nil || options.TTL < 300*time.Millisecond || options.TTL > time.Hour {
		return nil, types.ErrExecutionAdmission
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	taskID, tenantID := task.ID, task.TenantID
	acquiredAt := time.Now()
	attempt, err := st.StartSQLiteExecutionAttempt(ctx, taskID, tenantID, options.AttemptID, options.InstanceSHA256, options.ExpectedGateEpoch, options.TTL)
	if err != nil {
		return nil, err
	}
	identity := SQLiteOwnedExecutionIdentity{Origin: attempt.Origin, NamespaceSHA256: attempt.NamespaceSHA256, TaskID: taskID, TenantID: tenantID, AttemptID: attempt.ID, InstanceSHA256: attempt.InstanceSHA256, TaskIncarnation: attempt.TaskIncarnation, GateEpoch: attempt.GateEpoch}
	ownedCtx, cancel := context.WithCancelCause(store.WithSQLiteExecutionAttempt(ctx, attempt))
	lease := store.NewExecutionLease(func() { cancel(store.ErrTaskLeaseLost) })
	ownedCtx = context.WithValue(ownedCtx, sqliteOwnedScopeKey{}, sqliteOwnedScope{st, identity})
	ownedCtx = WithExecutionLeaseCheck(ownedCtx, lease.Err)
	lease.Start(ownedCtx, st, taskID, "execution-attempt:"+attempt.ID, options.TTL, acquiredAt)
	scope := &PreparedSQLiteOwnedExecution{engine: e, store: st, task: task, options: options, identity: identity, ctx: ownedCtx, cancel: cancel, lease: lease}
	// Reload before exposing the scope; stale callers cannot overwrite a checkpoint.
	latest, loadErr := st.GetTask(ownedCtx, taskID)
	if loadErr == nil && (latest.ID != taskID || latest.TenantID != tenantID) {
		loadErr = types.ErrExecutionAdmission
	}
	if loadErr != nil {
		return nil, errors.Join(loadErr, scope.Abort(loadErr))
	}
	*task = *latest
	task.ExecutionTraceID = ""
	if span := trace.SpanContextFromContext(ownedCtx); span.IsValid() {
		task.ExecutionTraceID = span.TraceID().String()
	}
	scope.loaded = true
	if options.OnAdmitted != nil {
		func() {
			defer func() {
				if p := recover(); p != nil {
					lease.Finish()
					cancel(context.Canceled)
					panic(p)
				}
			}()
			options.OnAdmitted(identity)
		}()
	}
	return scope, nil
}

func (e *Engine) runOwnedSQLiteExecution(ctx context.Context, st *store.SQLiteStore, task *types.Task, options SQLiteOwnedExecutionOptions, run func(context.Context, *types.Task) error) error {
	scope, err := e.PrepareOwnedSQLiteExecution(ctx, st, task, options)
	if err != nil {
		return err
	}
	return scope.Execute(run)
}

// Execute consumes the prepared scope. A nil callback uses the selected Engine
// entry. Trusted callbacks may wait for a semaphore before entering the Engine.
func (s *PreparedSQLiteOwnedExecution) Execute(run func(context.Context, *types.Task) error) error {
	return s.finish(run, nil, true)
}

// Abort records a non-entry exit and flushes the reloaded checkpoint, without
// invoking the Engine. Cancellation does not stop renewal during final cleanup.
func (s *PreparedSQLiteOwnedExecution) Abort(reason error) error {
	return s.finish(nil, reason, false)
}

func (s *PreparedSQLiteOwnedExecution) finish(run func(context.Context, *types.Task) error, executionErr error, execute bool) error {
	if !s.consumed.CompareAndSwap(false, true) {
		return types.ErrExecutionAdmission
	}
	st, task, options, identity, ownedCtx, lease := s.store, s.task, s.options, s.identity, s.ctx, s.lease
	taskID, tenantID := identity.TaskID, identity.TenantID
	defer s.cancel(context.Canceled)
	stopped := false
	defer func() {
		if !stopped {
			lease.Finish()
		}
	}()
	entered := false
	// Preflight may have rejected a status CAS. Never flush the earlier copy
	// over the durable checkpoint on a path that did not enter execution.
	if !execute {
		reloadCtx, reloadCancel := context.WithTimeout(context.WithoutCancel(ownedCtx), 10*time.Second)
		latest, err := st.GetTask(reloadCtx, taskID)
		reloadCancel()
		if err != nil || latest.ID != taskID || latest.TenantID != tenantID {
			s.loaded = false
			executionErr = errors.Join(executionErr, ErrSQLiteOwnedFlush)
		} else {
			*task = *latest
			s.loaded = true
		}
	}
	if execute && s.loaded {
		entered = true
		if run != nil {
			executionErr = run(ownedCtx, task)
		} else if options.RunAll {
			executionErr = s.engine.RunAll(ownedCtx, task)
		} else {
			executionErr = s.engine.Next(ownedCtx, task)
		}
	}

	outcome := "returned"
	if !entered {
		outcome = "not-entered"
	} else if errors.Is(executionErr, types.ErrExecutionAdmission) {
		outcome = "admission-denied"
	} else if errors.Is(executionErr, context.Canceled) || errors.Is(executionErr, context.DeadlineExceeded) {
		outcome = "cancelled"
	} else if executionErr != nil {
		outcome = "error"
	}
	exitSHA := sqliteOwnedDigest(struct {
		Schema  string
		Scope   SQLiteOwnedExecutionIdentity
		Outcome string
	}{"sqlite-engine-return-v1", identity, outcome})
	// Engine has returned synchronously. This local receipt never claims all
	// tools/hosts have exited; cleanup is a distinct mandatory stage below.
	exitCtx, exitCancel := context.WithTimeout(context.WithoutCancel(ownedCtx), 10*time.Second)
	exitErr := st.ExitSQLiteExecutionAttempt(exitCtx, taskID, tenantID, identity.AttemptID, identity.InstanceSHA256, exitSHA)
	exitCancel()

	flushSHA := ""
	var flushErr error
	if !s.loaded || task.ID != taskID || task.TenantID != tenantID {
		flushErr = ErrSQLiteOwnedFlush
	} else if err := lease.Err(); err != nil {
		flushErr = err
	} else {
		flushCtx, flushCancel := context.WithTimeout(context.WithoutCancel(ownedCtx), 10*time.Second)
		if err := st.SaveFullTask(flushCtx, task); err != nil {
			flushErr = ErrSQLiteOwnedFlush
		} else {
			baseline, err := st.ReadSQLiteRestoreBaseline(flushCtx, store.TaskRestoreLocalObservationRequest{TaskID: taskID, TenantID: tenantID, MaxBytes: store.MaxTaskRestoreObservationBytes, MaxTasks: store.MaxTaskRestoreObservationTasks})
			if err != nil {
				flushErr = ErrSQLiteOwnedFlush
			} else {
				flushSHA = sqliteOwnedDigest(struct {
					Schema         string
					Scope          SQLiteOwnedExecutionIdentity
					BaselineSHA256 string
				}{"sqlite-final-flush-v1", identity, baseline.SHA256})
			}
		}
		flushCancel()
	}
	// Cleanup is attempted even when persistence/exit recording failed. Its
	// success cannot erase an earlier failure or independently finalize an attempt.
	cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ownedCtx), 10*time.Second)
	defer cleanupCancel()
	cleanupSHA, cleanupErr := options.Cleanup(cleanupCtx, identity)
	if cleanupErr != nil || cleanupCtx.Err() != nil || !sqliteOwnedValidDigest(cleanupSHA) {
		cleanupErr = ErrSQLiteOwnedCleanup
	}
	cleanupCancel()
	lease.Finish()
	stopped = true
	ownershipErr := lease.Err()
	var finalizeErr error
	if exitErr == nil && flushErr == nil && cleanupErr == nil && ownershipErr == nil {
		finalizeCtx, finalizeCancel := context.WithTimeout(context.WithoutCancel(ownedCtx), 10*time.Second)
		finalizeErr = st.FinalizeSQLiteExecutionAttempt(finalizeCtx, taskID, tenantID, identity.AttemptID, identity.InstanceSHA256, flushSHA, cleanupSHA)
		finalizeCancel()
	}
	return errors.Join(executionErr, exitErr, flushErr, cleanupErr, ownershipErr, finalizeErr)
}

func sqliteOwnedDigest(value any) string {
	body, _ := json.Marshal(value)
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
func sqliteOwnedValidDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32 && hex.EncodeToString(decoded) == value
}

// RunOwnedSQLiteApprovalRecovery reuses the owned task lease and renewal. The
// approval's own consumption lease/CAS remain the recovery engine's authority;
// no task lease is reacquired or released by its nested recovery cleanup.
func (e *Engine) RunOwnedSQLiteApprovalRecovery(ctx context.Context, st *store.SQLiteStore, task *types.Task, approval *types.DurableApproval, options SQLiteOwnedExecutionOptions) (bool, error) {
	if task == nil || approval == nil || approval.TaskID != task.ID || approval.TenantID != task.TenantID || (approval.Status != types.ApprovalApproved && approval.Status != types.ApprovalRejected) {
		return false, types.ErrExecutionAdmission
	}
	recovered := false
	err := e.runOwnedSQLiteExecution(ctx, st, task, options, func(owned context.Context, current *types.Task) error {
		var err error
		owner := "execution-attempt:" + options.AttemptID
		if approval.Status == types.ApprovalApproved {
			recovered, err = e.RecoverApprovedApproval(owned, current, approval, owner)
		} else {
			recovered, err = e.RecoverRejectedApproval(owned, current, approval, owner)
		}
		return err
	})
	return recovered, err
}
