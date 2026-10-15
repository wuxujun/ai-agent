package orchestrator

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/approvalcrypto"
	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

// The parent kills only the subprocess it created, after a committed boundary.
// No production processes, databases, tools or host cleanup are involved.
func TestSQLiteOwnedCrashHelper(t *testing.T) {
	stage := os.Getenv("AI_AGENT_OWNED_CRASH_STAGE")
	if stage == "" {
		t.Skip("subprocess fixture")
	}
	path := os.Getenv("AI_AGENT_OWNED_CRASH_PATH")
	st, err := store.NewSQLiteStoreWithExecutionGate(path, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close() // Process.Kill must bypass this and every scope defer.
	task := &types.Task{ID: "task", TenantID: "tenant", Goal: "before execution", Status: types.StatusRunning, MaxSteps: 10, ToolBudget: 10}
	if strings.HasPrefix(stage, "approval-") {
		task.Status = types.StatusAwaitingApproval
	}
	if err := st.SaveFullTask(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	epoch, err := st.EnrollSQLiteExecutionGate(t.Context(), task.ID, task.TenantID)
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := st.ReadSQLiteRestoreBaseline(t.Context(), store.TaskRestoreLocalObservationRequest{TaskID: task.ID, TenantID: task.TenantID, MaxBytes: store.MaxTaskRestoreObservationBytes, MaxTasks: store.MaxTaskRestoreObservationTasks})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(baseline.Pin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".pin", raw, 0600); err != nil {
		t.Fatal(err)
	}
	boundary := func() {
		fmt.Fprintln(os.Stdout, "OWNED_CRASH_READY")
		// A timer keeps the helper alive even after finalization stops renewal.
		<-time.After(time.Hour)
		t.Fatal("parent did not terminate fixture")
	}
	options := SQLiteOwnedExecutionOptions{AttemptID: "crash-attempt", InstanceSHA256: strings.Repeat("b", 64), ExpectedGateEpoch: epoch, TTL: time.Second, Cleanup: func(context.Context, SQLiteOwnedExecutionIdentity) (string, error) {
		if stage == "cleanup" {
			boundary()
		}
		return strings.Repeat("c", 64), nil
	}}
	engine := &Engine{Store: st, ExecutionAdmission: st}
	if strings.HasPrefix(stage, "approval-") {
		codec, err := approvalcrypto.New(bytes.Repeat([]byte{0x53}, 32))
		if err != nil {
			t.Fatal(err)
		}
		action, _ := json.Marshal(planner.ActionCall{Action: "write_file", Parameters: map[string]any{"path": "fixture-effect", "content": "test"}})
		payload, err := codec.Encrypt(action)
		if err != nil {
			t.Fatal(err)
		}
		resolution, _ := json.Marshal(types.ApprovalResult{Approved: true})
		encrypted, err := codec.Encrypt(resolution)
		if err != nil {
			t.Fatal(err)
		}
		approval := &types.DurableApproval{ID: "crash-approval", TaskID: task.ID, TenantID: task.TenantID, Status: types.ApprovalApproved, Request: types.ApprovalRequest{ID: "crash-approval", TaskID: task.ID, Action: "write_file", RiskLevel: types.RiskLevelHigh}, ActionPayload: payload, ResolutionPayload: encrypted}
		if err := st.CreateApproval(t.Context(), approval); err != nil {
			t.Fatal(err)
		}
		engine.ApprovalCodec = codec
		engine.Executor = &ownedCrashExecutor{run: func() {
			if stage == "approval-before-effect" {
				boundary()
			}
			if stage != "approval-after-effect" {
				t.Fatalf("unknown approval crash stage %q", stage)
			}
			if err := os.WriteFile(path+".effect", []byte("fixture-effect-once"), 0600); err != nil {
				t.Fatal(err)
			}
			boundary()
		}}
		if err := engine.SetOwnedSQLiteApprovalDispatcher(func(context.Context, string, string) (SQLiteOwnedExecutionOptions, error) { return options, nil }); err != nil {
			t.Fatal(err)
		}
		_, err = engine.DispatchApprovalRecovery(t.Context(), task, approval, "unused-ordinary-owner")
		t.Fatalf("approval crash boundary unexpectedly returned: %v", err)
	}
	scope, err := engine.PrepareOwnedSQLiteExecution(t.Context(), st, task, options)
	if err != nil {
		t.Fatal(err)
	}
	if stage == "prepared" {
		boundary()
	}
	err = scope.Execute(func(context.Context, *types.Task) error {
		task.Goal = "after execution"
		if stage == "execution" {
			boundary()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if stage != "complete" {
		t.Fatalf("unknown crash stage %q", stage)
	}
	boundary()
}

func TestSQLiteOwnedForcedProcessExitDurability(t *testing.T) {
	for _, stage := range []string{"prepared", "execution", "cleanup", "complete", "approval-before-effect", "approval-after-effect"} {
		t.Run(stage, func(t *testing.T) {
			dir, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "crash.sqlite")
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSQLiteOwnedCrashHelper$", "-test.timeout=18s")
			command.Env = append(os.Environ(), "AI_AGENT_OWNED_CRASH_STAGE="+stage, "AI_AGENT_OWNED_CRASH_PATH="+path)
			var stderr bytes.Buffer
			command.Stderr = &stderr
			stdout, err := command.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- command.Wait() }()
			joined := false
			defer func() {
				if !joined {
					_ = command.Process.Kill()
					<-done
				}
			}()
			ready := make(chan string, 1)
			go func() {
				scanner := bufio.NewScanner(stdout)
				var diagnostic strings.Builder
				for scanner.Scan() {
					if scanner.Text() == "OWNED_CRASH_READY" {
						ready <- "OWNED_CRASH_READY"
						return
					}
					if diagnostic.Len() < 4096 {
						diagnostic.WriteString(scanner.Text())
						diagnostic.WriteByte('\n')
					}
				}
				ready <- diagnostic.String()
			}()
			select {
			case message := <-ready:
				if message != "OWNED_CRASH_READY" {
					t.Fatalf("child never reached committed boundary: %s", message)
				}
			case <-ctx.Done():
				t.Fatal("child boundary timeout")
			}
			if err := command.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			err = <-done
			joined = true
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.Success() {
				t.Fatalf("fixture was not forcibly terminated: %v %s", err, stderr.String())
			}
			// SQLite may recover a hot journal left by an overlapping renewal. Open only
			// this disposable fixture through the existing normal Store, then close it.
			restart, err := store.NewSQLiteStore(path)
			if err != nil {
				t.Fatal(err)
			}
			saved, err := restart.GetTask(t.Context(), "task")
			if err != nil {
				restart.Close()
				t.Fatal(err)
			}
			wantGoal := "before execution"
			if stage == "cleanup" || stage == "complete" {
				wantGoal = "after execution"
			}
			if saved.Goal != wantGoal {
				restart.Close()
				t.Fatalf("crash checkpoint = %q, want %q", saved.Goal, wantGoal)
			}
			if strings.HasPrefix(stage, "approval-") {
				approval, err := restart.GetApproval(t.Context(), "crash-approval", "tenant")
				if err != nil || approval.Status != types.ApprovalConsumed || saved.Status != types.StatusAwaitingApproval || len(saved.Trace) != 0 {
					restart.Close()
					t.Fatal("approval crash checkpoint", err)
				}
				effect, readErr := os.ReadFile(path + ".effect")
				if stage == "approval-before-effect" {
					if !errors.Is(readErr, os.ErrNotExist) {
						restart.Close()
						t.Fatal("effect ran before crash boundary", readErr)
					}
				} else if readErr != nil || string(effect) != "fixture-effect-once" {
					restart.Close()
					t.Fatal("fake effect missing", readErr)
				}
				// Consumed is never transformed back to approved on restart. The
				// legacy recovery entry also refuses to replay this record.
				replayed := false
				replayEngine := &Engine{Store: restart, ApprovalCodec: mustOwnedCrashCodec(t), Executor: &ownedCrashExecutor{run: func() { replayed = true }}}
				recovered, replayErr := replayEngine.RecoverApprovedApproval(t.Context(), saved, approval, "restart-replay")
				if recovered || replayErr != nil || replayed {
					restart.Close()
					t.Fatal("consumed effect replayed", replayErr)
				}
			}
			// A normal restarted writer cannot silently bypass the dedicated gate.
			acquired, leaseErr := restart.AcquireTaskLease(t.Context(), "task", "restart-owner", time.Second)
			if acquired || leaseErr == nil {
				restart.Close()
				t.Fatal("normal restart bypassed attempt gate")
			}
			if err := restart.Close(); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(path + ".pin")
			if err != nil {
				t.Fatal(err)
			}
			var pin store.SQLiteRestoreVersionPin
			if err := json.Unmarshal(raw, &pin); err != nil {
				t.Fatal(err)
			}
			reader, err := store.OpenSQLiteExecutionGateReader(path, pin)
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			out, err := reader.ReadSQLiteExecutionGateSnapshot(t.Context(), "task", "tenant")
			if err != nil {
				t.Fatal(err)
			}
			if len(out.Attempts) != 1 || out.Phase != "open" || out.ReceiptsComplete || out.Status != "unverified" {
				t.Fatal("crash inferred isolation")
			}
			attempt := out.Attempts[0]
			switch stage {
			case "prepared", "execution", "approval-before-effect", "approval-after-effect":
				if attempt.State != "running" || attempt.ExitSHA256 != "" || attempt.FlushSHA256 != "" || attempt.CleanupSHA256 != "" {
					t.Fatal("crash fabricated exit")
				}
			case "cleanup":
				if attempt.State != "exited" || attempt.ExitSHA256 == "" || attempt.FlushSHA256 != "" || attempt.CleanupSHA256 != "" {
					t.Fatal("crash fabricated cleanup")
				}
			case "complete":
				if attempt.State != "complete" || attempt.ExitSHA256 == "" || attempt.FlushSHA256 == "" || attempt.CleanupSHA256 == "" || out.LeaseState != "absent" {
					t.Fatal("committed finalize lost")
				}
				return
			}
			// Expiry is diagnostic only. It cannot create receipts or remove the attempt.
			deadline := time.Now().Add(3 * time.Second)
			for out.LeaseState != "expired-unresolved" && time.Now().Before(deadline) {
				time.Sleep(25 * time.Millisecond)
				out, err = reader.ReadSQLiteExecutionGateSnapshot(t.Context(), "task", "tenant")
				if err != nil {
					t.Fatal(err)
				}
			}
			if out.LeaseState != "expired-unresolved" || out.Attempts[0] != attempt || out.ReceiptsComplete {
				t.Fatal("expiry rewrote crash evidence")
			}
		})
	}
}

// This executor writes only the dedicated fixture marker; it does not call tools.
type ownedCrashExecutor struct{ run func() }

func (e *ownedCrashExecutor) Execute(context.Context, *types.Task, *planner.PlanDecision) ([]types.StepTrace, error) {
	e.run()
	return nil, nil
}
func mustOwnedCrashCodec(t *testing.T) *approvalcrypto.Codec {
	t.Helper()
	codec, err := approvalcrypto.New(bytes.Repeat([]byte{0x53}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return codec
}
