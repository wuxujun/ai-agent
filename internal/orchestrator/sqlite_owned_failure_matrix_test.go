package orchestrator

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

func TestSQLiteOwnedPersistenceFailureRollsBackAndStillCleans(t *testing.T) {
	engine, st, task, options := sqliteOwnedFixture(t)
	cleaned := false
	options.Cleanup = func(ctx context.Context, _ SQLiteOwnedExecutionIdentity) (string, error) {
		cleaned = true
		out := ownedSnapshot(t, st)
		saved, err := st.GetTask(ctx, "task")
		if err != nil || saved.Goal != "fixture" || out.Attempts[0].State != "exited" {
			t.Fatal("failed flush partially committed", err)
		}
		return strings.Repeat("c", 64), nil
	}
	scope, err := engine.PrepareOwnedSQLiteExecution(t.Context(), st, task, options)
	if err != nil {
		t.Fatal(err)
	}
	err = scope.Execute(func(context.Context, *types.Task) error {
		task.Goal = "must roll back"
		// Real SQLite NOT NULL failure, with unchanged task/tenant identity. No schema
		// modifications or production failure-injection hooks are involved.
		task.LLMCostBudgetUSD = math.NaN()
		return nil
	})
	if !errors.Is(err, ErrSQLiteOwnedFlush) || !cleaned {
		t.Fatal("flush failure not propagated", err)
	}
	out := ownedSnapshot(t, st)
	if out.Attempts[0].State != "exited" || out.Attempts[0].FlushSHA256 != "" || out.Attempts[0].CleanupSHA256 != "" || out.LeaseState == "absent" || out.ReceiptsComplete {
		t.Fatal("failed persistence finalized")
	}
}

func TestSQLiteOwnedOwnershipLossPreventsFinalFlushAndFinalize(t *testing.T) {
	for _, stage := range []string{"execution", "cleanup"} {
		t.Run(stage, func(t *testing.T) {
			engine, st, task, options := sqliteOwnedFixture(t)
			var scope *PreparedSQLiteOwnedExecution
			cleaned := false
			options.Cleanup = func(ctx context.Context, _ SQLiteOwnedExecutionIdentity) (string, error) {
				cleaned = true
				if stage == "cleanup" {
					scope.lease.Lose()
				}
				if ctx.Err() != nil {
					t.Fatal("cleanup context canceled")
				}
				return strings.Repeat("c", 64), nil
			}
			var err error
			scope, err = engine.PrepareOwnedSQLiteExecution(t.Context(), st, task, options)
			if err != nil {
				t.Fatal(err)
			}
			err = scope.Execute(func(ctx context.Context, current *types.Task) error {
				current.Goal = "execution result"
				if stage == "execution" {
					scope.lease.Lose()
					select {
					case <-ctx.Done():
					case <-time.After(time.Second):
						t.Fatal("lease loss did not cancel")
					}
					return store.ErrTaskLeaseLost
				}
				return nil
			})
			if !errors.Is(err, store.ErrTaskLeaseLost) || !cleaned {
				t.Fatal("ownership loss ignored", err)
			}
			out := ownedSnapshot(t, st)
			saved, err := st.GetTask(t.Context(), "task")
			if err != nil {
				t.Fatal(err)
			}
			want := "execution result"
			if stage == "execution" {
				want = "fixture"
			}
			if saved.Goal != want || out.Attempts[0].State != "exited" || out.Attempts[0].FlushSHA256 != "" || out.Attempts[0].CleanupSHA256 != "" || out.LeaseState == "absent" || out.ReceiptsComplete {
				t.Fatal("lost ownership finalized or stale flush")
			}
		})
	}
}

func TestSQLiteOwnedExpiredPreparedScopeCannotFlushOrRevive(t *testing.T) {
	engine, st, task, options := sqliteOwnedFixture(t)
	options.TTL = 300 * time.Millisecond
	scope, err := engine.PrepareOwnedSQLiteExecution(t.Context(), st, task, options)
	if err != nil {
		t.Fatal(err)
	}
	// Stop only this fixture's renewal to model a stalled/disconnected owner.
	scope.lease.Finish()
	time.Sleep(350 * time.Millisecond)
	err = scope.Execute(func(ctx context.Context, current *types.Task) error {
		current.Goal = "stale owner"
		return executionLeaseError(ctx)
	})
	if !errors.Is(err, store.ErrTaskLeaseLost) {
		t.Fatal("expired scope accepted", err)
	}
	out := ownedSnapshot(t, st)
	saved, err := st.GetTask(t.Context(), "task")
	if err != nil || saved.Goal != "fixture" || out.Attempts[0].State != "exited" || out.LeaseState != "expired-unresolved" || out.ReceiptsComplete {
		t.Fatal("expiry inferred completion", err)
	}
	options.AttemptID = "replacement"
	if err := engine.RunOwnedSQLiteExecution(t.Context(), st, task, options); !errors.Is(err, store.ErrSQLiteExecutionConflict) {
		t.Fatal("expired unresolved owner replaced", err)
	}
}

func TestSQLiteOwnedCleanupLateConfirmationRejected(t *testing.T) {
	engine, st, task, options := sqliteOwnedFixture(t)
	options.Cleanup = func(ctx context.Context, _ SQLiteOwnedExecutionIdentity) (string, error) {
		<-ctx.Done()
		// Even a syntactically valid digest cannot confirm cleanup after its deadline.
		return strings.Repeat("c", 64), nil
	}
	err := engine.RunOwnedSQLiteExecution(t.Context(), st, task, options)
	if !errors.Is(err, ErrSQLiteOwnedCleanup) {
		t.Fatal("late confirmation accepted", err)
	}
	out := ownedSnapshot(t, st)
	if out.Attempts[0].State != "exited" || out.Attempts[0].CleanupSHA256 != "" || out.LeaseState == "absent" || out.ReceiptsComplete {
		t.Fatal("late cleanup finalized")
	}
}
