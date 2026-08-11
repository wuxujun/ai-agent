package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/spf13/viper"
	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

func TestStores(t *testing.T) {
	// Setup a temporary directory for SQLite file
	tmpDir, err := os.MkdirTemp("", "store_test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	sqlitePath := filepath.Join(tmpDir, "test.db")
	sqliteStore, err := store.NewSQLiteStore(sqlitePath)
	if err != nil {
		t.Fatalf("failed to create SQLiteStore: %v", err)
	}

	memoryStore := store.NewMemoryStore()

	stores := map[string]store.Store{
		"SQLiteStore": sqliteStore,
		"MemoryStore": memoryStore,
	}

	// Optional: PostgreSQL test if TEST_POSTGRES_DSN is provided
	// e.g. TEST_POSTGRES_DSN="postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable"
	pgDsn := os.Getenv("TEST_POSTGRES_DSN")
	if pgDsn != "" {
		pgStore, err := store.NewPostgresStore(pgDsn)
		if err != nil {
			t.Fatalf("failed to create PostgresStore: %v", err)
		}
		stores["PostgresStore"] = pgStore
	} else {
		t.Log("Skipping PostgresStore test: TEST_POSTGRES_DSN environment variable not set")
	}

	// Optional: Redis test if TEST_REDIS_URL is provided
	// e.g. TEST_REDIS_URL="redis://localhost:6379/0"
	redisUrl := os.Getenv("TEST_REDIS_URL")
	if redisUrl != "" {
		redisStore, err := store.NewRedisStoreFromURL(redisUrl)
		if err != nil {
			t.Fatalf("failed to create RedisStore: %v", err)
		}
		stores["RedisStore"] = redisStore
	} else {
		t.Log("Skipping RedisStore test: TEST_REDIS_URL environment variable not set")
	}

	for name, s := range stores {
		t.Run(name, func(t *testing.T) {
			defer s.Close()
			ctx := context.Background()

			// 1. Verify task not found returns sql.ErrNoRows
			_, err := s.GetTask(ctx, "non-existent")
			if err != sql.ErrNoRows {
				t.Errorf("expected sql.ErrNoRows, got %v", err)
			}

			// 2. Save a task
			task := &types.Task{
				ID:                  "task-123",
				TenantID:            "tenant-a",
				Goal:                "Build a cool agent",
				Status:              types.StatusCreated,
				Mode:                "multiagent",
				MaxSteps:            10,
				StepCount:           1,
				Workspace:           "/tmp/workspace",
				Hypothesis:          "Initial hypothesis",
				Unresolved:          []string{"subtask-a", "subtask-b"},
				ToolBudget:          5,
				TokenBudget:         1234,
				LLMCallBudget:       7,
				LLMCostBudgetUSD:    2.5,
				LLMCalls:            3,
				LLMEstimatedCostUSD: 1.25,
				Memories: []types.Memory{
					{
						ID:          "mem-rag-1",
						TaskID:      "prev-task-a",
						Goal:        "earlier related goal",
						FinalAnswer: "earlier answer",
						KeyFindings: "earlier findings",
						Timestamp:   time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
						Embedding:   []float32{0.1, 0.2, 0.3},
					},
				},
				Trace: []types.StepTrace{
					{
						Step:        0,
						Goal:        "Find file",
						Action:      "find_files",
						Query:       "*.go",
						Observation: "found main.go",
						Error:       "sample trace error",
						TokenUsage: types.TokenUsage{
							PromptTokens:     11,
							CompletionTokens: 7,
							TotalTokens:      18,
						},
						Evidence: []types.Evidence{
							{
								Path:  "main.go",
								Lines: []string{"package main"},
								Query: "*.go",
							},
						},
					},
				},
				FinalAnswer: "Done!",
				AnswerAudit: &types.AnswerAuditReport{PipelineVersion: "test", Publishable: true, Stages: []types.AnswerAuditStage{{Name: "freshness", Status: "passed", Findings: []types.AnswerAuditFinding{{Kind: "test", Detail: "kept"}}}}},
			}

			err = s.SaveFullTask(ctx, task)
			if err != nil {
				t.Fatalf("failed to save task: %v", err)
			}
			if approvals, ok := s.(store.DurableApprovalStore); ok {
				testDurableApprovalContract(t, ctx, approvals, task.ID)
			}

			// 3. Retrieve and verify the task
			retrieved, err := s.GetTask(ctx, "task-123")
			if err != nil {
				t.Fatalf("failed to get task: %v", err)
			}

			if retrieved.ID != task.ID {
				t.Errorf("expected ID %q, got %q", task.ID, retrieved.ID)
			}
			if retrieved.Goal != task.Goal {
				t.Errorf("expected Goal %q, got %q", task.Goal, retrieved.Goal)
			}
			if retrieved.Status != task.Status {
				t.Errorf("expected Status %q, got %q", task.Status, retrieved.Status)
			}
			if retrieved.Mode != task.Mode {
				t.Errorf("expected Mode %q, got %q", task.Mode, retrieved.Mode)
			}
			if retrieved.MaxSteps != task.MaxSteps {
				t.Errorf("expected MaxSteps %d, got %d", task.MaxSteps, retrieved.MaxSteps)
			}
			if retrieved.TenantID != task.TenantID {
				t.Errorf("expected TenantID %q, got %q", task.TenantID, retrieved.TenantID)
			}
			if retrieved.StepCount != task.StepCount {
				t.Errorf("expected StepCount %d, got %d", task.StepCount, retrieved.StepCount)
			}
			if retrieved.Workspace != task.Workspace {
				t.Errorf("expected Workspace %q, got %q", task.Workspace, retrieved.Workspace)
			}
			if retrieved.Hypothesis != task.Hypothesis {
				t.Errorf("expected Hypothesis %q, got %q", task.Hypothesis, retrieved.Hypothesis)
			}
			if len(retrieved.Unresolved) != len(task.Unresolved) {
				t.Errorf("expected Unresolved len %d, got %d", len(task.Unresolved), len(retrieved.Unresolved))
			} else {
				for i, v := range task.Unresolved {
					if retrieved.Unresolved[i] != v {
						t.Errorf("expected Unresolved[%d] = %q, got %q", i, v, retrieved.Unresolved[i])
					}
				}
			}
			if retrieved.ToolBudget != task.ToolBudget {
				t.Errorf("expected ToolBudget %d, got %d", task.ToolBudget, retrieved.ToolBudget)
			}
			if retrieved.TokenBudget != task.TokenBudget {
				t.Errorf("expected TokenBudget %d, got %d", task.TokenBudget, retrieved.TokenBudget)
			}
			if retrieved.LLMCallBudget != task.LLMCallBudget || retrieved.LLMCostBudgetUSD != task.LLMCostBudgetUSD || retrieved.LLMCalls != task.LLMCalls || retrieved.LLMEstimatedCostUSD != task.LLMEstimatedCostUSD {
				t.Errorf("expected LLM budget state %+v, got %+v", task, retrieved)
			}
			if retrieved.FinalAnswer != task.FinalAnswer {
				t.Errorf("expected FinalAnswer %q, got %q", task.FinalAnswer, retrieved.FinalAnswer)
			}
			if retrieved.AnswerAudit == nil || retrieved.AnswerAudit.PipelineVersion != "test" || len(retrieved.AnswerAudit.Stages) != 1 {
				t.Fatalf("answer audit did not round-trip: %+v", retrieved.AnswerAudit)
			}
			tenantTasks, err := s.ListTasks(ctx, store.ListFilter{TenantID: "tenant-a"})
			if err != nil || len(tenantTasks) != 1 {
				t.Fatalf("tenant-a list: count=%d err=%v", len(tenantTasks), err)
			}
			otherTenantTasks, err := s.ListTasks(ctx, store.ListFilter{TenantID: "tenant-b"})
			if err != nil || len(otherTenantTasks) != 0 {
				t.Fatalf("tenant-b list: count=%d err=%v", len(otherTenantTasks), err)
			}

			// Memories roundtrip: persisted records keep prompt-facing fields but
			// drop Embedding (~1.5 KB per record). MemoryStore is the in-process
			// exception — it clones the whole struct including embeddings, since
			// QueryMemories still consumes them on the in-memory path.
			if len(retrieved.Memories) != len(task.Memories) {
				t.Fatalf("expected Memories len %d, got %d", len(task.Memories), len(retrieved.Memories))
			}
			gotMem := retrieved.Memories[0]
			wantMem := task.Memories[0]
			if gotMem.ID != wantMem.ID || gotMem.Goal != wantMem.Goal || gotMem.FinalAnswer != wantMem.FinalAnswer || gotMem.KeyFindings != wantMem.KeyFindings {
				t.Errorf("Memories[0] mismatch: got %+v, want %+v", gotMem, wantMem)
			}
			if name == "MemoryStore" {
				// in-memory store preserves embeddings for QueryMemories
				if len(gotMem.Embedding) != len(wantMem.Embedding) {
					t.Errorf("MemoryStore: expected Embedding kept (len %d), got %d", len(wantMem.Embedding), len(gotMem.Embedding))
				}
			} else {
				if gotMem.Embedding != nil {
					t.Errorf("%s: expected Embedding stripped on persistence, got %v", name, gotMem.Embedding)
				}
			}

			// 4. Verify step traces
			if len(retrieved.Trace) != len(task.Trace) {
				t.Fatalf("expected Trace len %d, got %d", len(task.Trace), len(retrieved.Trace))
			}

			tr1 := task.Trace[0]
			tr2 := retrieved.Trace[0]
			if tr1.Step != tr2.Step {
				t.Errorf("expected trace Step %d, got %d", tr1.Step, tr2.Step)
			}
			if tr1.Goal != tr2.Goal {
				t.Errorf("expected trace Goal %q, got %q", tr1.Goal, tr2.Goal)
			}
			if tr1.Action != tr2.Action {
				t.Errorf("expected trace Action %q, got %q", tr1.Action, tr2.Action)
			}
			if tr1.Query != tr2.Query {
				t.Errorf("expected trace Query %q, got %q", tr1.Query, tr2.Query)
			}
			if tr1.Observation != tr2.Observation {
				t.Errorf("expected trace Observation %q, got %q", tr1.Observation, tr2.Observation)
			}
			if tr1.Error != tr2.Error {
				t.Errorf("expected trace Error %q, got %q", tr1.Error, tr2.Error)
			}
			if tr1.TokenUsage != tr2.TokenUsage {
				t.Errorf("expected trace TokenUsage %+v, got %+v", tr1.TokenUsage, tr2.TokenUsage)
			}

			if len(tr1.Evidence) != len(tr2.Evidence) {
				t.Fatalf("expected evidence len %d, got %d", len(tr1.Evidence), len(tr2.Evidence))
			}
			ev1 := tr1.Evidence[0]
			ev2 := tr2.Evidence[0]
			if ev1.Path != ev2.Path {
				t.Errorf("expected evidence Path %q, got %q", ev1.Path, ev2.Path)
			}
			if ev1.Query != ev2.Query {
				t.Errorf("expected evidence Query %q, got %q", ev1.Query, ev2.Query)
			}
			if len(ev1.Lines) != len(ev2.Lines) || ev1.Lines[0] != ev2.Lines[0] {
				t.Errorf("expected evidence Lines %v, got %v", ev1.Lines, ev2.Lines)
			}

			// 5. Test Memory Save and Query
			mem1 := &types.Memory{
				ID:          "mem-task-1",
				TaskID:      "task-1",
				Goal:        "find config file and check API keys",
				FinalAnswer: "The API key is abc123 in config.yaml",
				KeyFindings: "Step 1: found config.yaml. Step 2: read API key.",
				Timestamp:   time.Now(),
				Embedding:   []float32{1.0, 0.0, 0.0},
			}
			err = s.SaveMemory(ctx, mem1)
			if err != nil {
				t.Fatalf("failed to save memory 1: %v", err)
			}

			mem2 := &types.Memory{
				ID:          "mem-task-2",
				TaskID:      "task-2",
				Goal:        "compile go binary and run tests",
				FinalAnswer: "Build succeeded and all tests passed",
				KeyFindings: "Step 1: run go build. Step 2: run go test.",
				Timestamp:   time.Now(),
				Embedding:   []float32{0.0, 1.0, 0.0},
			}
			err = s.SaveMemory(ctx, mem2)
			if err != nil {
				t.Fatalf("failed to save memory 2: %v", err)
			}

			// Query with embedding similar to mem1
			mems, err := s.QueryMemories(ctx, "find API key", []float32{0.9, 0.1, 0.0}, 2)
			if err != nil {
				t.Fatalf("failed to query memories: %v", err)
			}

			if len(mems) == 0 {
				t.Fatalf("expected at least 1 memory, got 0")
			}
			if mems[0].ID != mem1.ID {
				t.Errorf("expected closest memory to be %q, got %q", mem1.ID, mems[0].ID)
			}

			// Query with query string (fallback keyword overlap test)
			mems2, err := s.QueryMemories(ctx, "run tests", nil, 2)
			if err != nil {
				t.Fatalf("failed to query memories by query string: %v", err)
			}
			if len(mems2) == 0 {
				t.Fatalf("expected at least 1 memory matching string, got 0")
			}
			if mems2[0].ID != mem2.ID {
				t.Errorf("expected keyword match to favor %q, got %q", mem2.ID, mems2[0].ID)
			}

			// ── Scenario D: TryTransitionTaskStatus ────────
			// 1. Invalid status from list (should fail)
			success, err := s.TryTransitionTaskStatus(ctx, task.ID, []types.TaskStatus{types.StatusCompleted}, types.StatusRunning)
			if err != nil {
				t.Fatalf("TryTransitionTaskStatus error: %v", err)
			}
			if success {
				t.Errorf("expected transition to fail, but succeeded")
			}

			// 2. Valid status transition (should succeed)
			success, err = s.TryTransitionTaskStatus(ctx, task.ID, []types.TaskStatus{types.StatusCreated}, types.StatusRunning)
			if err != nil {
				t.Fatalf("TryTransitionTaskStatus error: %v", err)
			}
			if !success {
				t.Errorf("expected transition to succeed, but failed")
			}

			// Verify status in DB is indeed updated
			gotTrans, err := s.GetTask(ctx, task.ID)
			if err != nil {
				t.Fatalf("failed to retrieve task: %v", err)
			}
			if gotTrans.Status != types.StatusRunning {
				t.Errorf("expected status to be 'running', got %s", gotTrans.Status)
			}

			// 3. Try transition on non-existent task
			_, err = s.TryTransitionTaskStatus(ctx, "non-existent-task-id", []types.TaskStatus{types.StatusCreated}, types.StatusRunning)
			if err != sql.ErrNoRows {
				t.Errorf("expected sql.ErrNoRows for non-existent task, got %v", err)
			}

			// ── Scenario E: owner-scoped execution lease ────────
			leaseID := "lease-" + name
			acquired, err := s.AcquireTaskLease(ctx, leaseID, "owner-a", time.Minute)
			if err != nil || !acquired {
				t.Fatalf("AcquireTaskLease owner-a = %v, %v; want true, nil", acquired, err)
			}
			acquired, err = s.AcquireTaskLease(ctx, leaseID, "owner-b", time.Minute)
			if err != nil || acquired {
				t.Fatalf("AcquireTaskLease competing owner = %v, %v; want false, nil", acquired, err)
			}
			if err := s.ReleaseTaskLease(ctx, leaseID, "owner-b"); err != nil {
				t.Fatalf("ReleaseTaskLease wrong owner: %v", err)
			}
			acquired, err = s.AcquireTaskLease(ctx, leaseID, "owner-b", time.Minute)
			if err != nil || acquired {
				t.Fatalf("wrong-owner release removed lease: acquired=%v err=%v", acquired, err)
			}
			if err := s.ReleaseTaskLease(ctx, leaseID, "owner-a"); err != nil {
				t.Fatalf("ReleaseTaskLease owner-a: %v", err)
			}
			acquired, err = s.AcquireTaskLease(ctx, leaseID, "owner-b", time.Minute)
			if err != nil || !acquired {
				t.Fatalf("AcquireTaskLease after release = %v, %v; want true, nil", acquired, err)
			}
			_ = s.ReleaseTaskLease(ctx, leaseID, "owner-b")

			expiringLeaseID := leaseID + "-expiring"
			acquired, err = s.AcquireTaskLease(ctx, expiringLeaseID, "owner-a", 10*time.Millisecond)
			if err != nil || !acquired {
				t.Fatalf("AcquireTaskLease expiring owner = %v, %v", acquired, err)
			}
			time.Sleep(25 * time.Millisecond)
			acquired, err = s.AcquireTaskLease(ctx, expiringLeaseID, "owner-b", time.Minute)
			if err != nil || !acquired {
				t.Fatalf("AcquireTaskLease after expiry = %v, %v; want true, nil", acquired, err)
			}
			_ = s.ReleaseTaskLease(ctx, expiringLeaseID, "owner-b")

			ledger, ok := s.(types.TenantUsageLedger)
			if !ok {
				t.Fatalf("%s does not implement TenantUsageLedger", name)
			}
			period := time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC)
			budget := types.TenantLLMBudget{MaxCalls: 2, MaxEstimatedCostUSD: 1}
			for i := 1; i <= 2; i++ {
				usage, allowed, err := ledger.ReserveTenantLLMCall(ctx, "quota-"+name, period, budget)
				if err != nil || !allowed || usage.Calls != i {
					t.Fatalf("tenant reservation %d: usage=%+v allowed=%v err=%v", i, usage, allowed, err)
				}
			}
			usage, allowed, err := ledger.ReserveTenantLLMCall(ctx, "quota-"+name, period, budget)
			if err != nil || allowed || usage.Calls != 2 {
				t.Fatalf("tenant call quota: usage=%+v allowed=%v err=%v", usage, allowed, err)
			}
			costTenant := "cost-quota-" + name
			if _, allowed, err := ledger.ReserveTenantLLMCall(ctx, costTenant, period, budget); err != nil || !allowed {
				t.Fatalf("initial cost reservation: allowed=%v err=%v", allowed, err)
			}
			if err := ledger.AddTenantLLMCost(ctx, costTenant, period, 1.25); err != nil {
				t.Fatal(err)
			}
			usage, allowed, err = ledger.ReserveTenantLLMCall(ctx, costTenant, period, budget)
			if err != nil || allowed || usage.EstimatedCostUSD != 1.25 {
				t.Fatalf("tenant cost quota: usage=%+v allowed=%v err=%v", usage, allowed, err)
			}
			storedUsage, err := ledger.GetTenantLLMUsage(ctx, costTenant, period)
			if err != nil || storedUsage != usage {
				t.Fatalf("tenant usage read: got=%+v want=%+v err=%v", storedUsage, usage, err)
			}

			for _, tenantID := range []string{"tenant-a", "tenant-b"} {
				mem := &types.Memory{ID: "isolated-memory-" + tenantID + "-" + name, TenantID: tenantID, TaskID: "isolated-task-" + tenantID, Goal: "isolated " + tenantID, KeyFindings: tenantID, Timestamp: time.Now()}
				if err := s.SaveMemory(ctx, mem); err != nil {
					t.Fatal(err)
				}
			}
			scopedMemories, err := s.QueryMemories(store.WithTenantScope(ctx, "tenant-a"), "isolated", nil, 20)
			if err != nil {
				t.Fatal(err)
			}
			if len(scopedMemories) == 0 {
				t.Fatal("tenant memory isolation query returned no results")
			}
			for _, mem := range scopedMemories {
				if mem.TenantID != "tenant-a" {
					t.Fatalf("tenant memory isolation failed: %+v", scopedMemories)
				}
			}

			associatedMemory := &types.Memory{ID: "mem-" + task.ID, TaskID: task.ID, Goal: "owned memory", Timestamp: time.Now()}
			if err := s.SaveMemory(ctx, associatedMemory); err != nil {
				t.Fatal(err)
			}
			deleter, ok := s.(store.TaskDeletionStore)
			if !ok {
				t.Fatalf("%s does not implement TaskDeletionStore", name)
			}
			deleted, err := deleter.DeleteTask(ctx, task.ID)
			if err != nil || !deleted {
				t.Fatalf("DeleteTask = %v, %v; want true, nil", deleted, err)
			}
			if _, err := s.GetTask(ctx, task.ID); err != sql.ErrNoRows {
				t.Fatalf("deleted task GetTask error = %v, want sql.ErrNoRows", err)
			}
			deleted, err = deleter.DeleteTask(ctx, task.ID)
			if err != nil || deleted {
				t.Fatalf("second DeleteTask = %v, %v; want false, nil", deleted, err)
			}

			for _, id := range []string{"clear-a-" + name, "clear-b-" + name} {
				if err := s.SaveFullTask(ctx, &types.Task{ID: id, Status: types.StatusCreated}); err != nil {
					t.Fatal(err)
				}
			}
			count, err := deleter.DeleteAllTasks(ctx)
			if err != nil || count != 2 {
				t.Fatalf("DeleteAllTasks = %d, %v; want 2, nil", count, err)
			}

			memoryManager, ok := s.(store.MemoryManagementStore)
			if !ok {
				t.Fatalf("%s does not implement MemoryManagementStore", name)
			}
			for _, mem := range []*types.Memory{
				{ID: "managed-a-" + name, TenantID: "managed-a", Goal: "first", Timestamp: time.Now().Add(time.Second)},
				{ID: "managed-b-" + name, TenantID: "managed-b", Goal: "second", Timestamp: time.Now()},
			} {
				if err := s.SaveMemory(ctx, mem); err != nil {
					t.Fatal(err)
				}
			}
			managedA, err := memoryManager.ListMemories(ctx, store.ListMemoryFilter{TenantID: "managed-a", Limit: 10})
			if err != nil || len(managedA) != 1 || managedA[0].TenantID != "managed-a" {
				t.Fatalf("managed-a list = %+v, err=%v", managedA, err)
			}
			deleted, err = memoryManager.DeleteMemory(ctx, "managed-b-"+name, "managed-a")
			if err != nil || deleted {
				t.Fatalf("cross-tenant DeleteMemory = %v, %v; want false, nil", deleted, err)
			}
			deleted, err = memoryManager.DeleteMemory(ctx, "managed-a-"+name, "managed-a")
			if err != nil || !deleted {
				t.Fatalf("DeleteMemory = %v, %v; want true, nil", deleted, err)
			}
			count, err = memoryManager.DeleteAllMemories(ctx, "managed-b")
			if err != nil || count < 1 {
				t.Fatalf("DeleteAllMemories(managed-b) = %d, %v; want >=1, nil", count, err)
			}
		})
	}
}

func testDurableApprovalContract(t *testing.T, ctx context.Context, approvals store.DurableApprovalStore, taskID string) {
	t.Helper()
	record := &types.DurableApproval{
		ID: "contract-approval", TaskID: taskID, TenantID: "tenant-a",
		Request: types.ApprovalRequest{
			ID: "contract-approval", TaskID: taskID, Action: "write_file",
			RiskLevel: types.RiskLevelHigh, ParameterSummary: []string{"path: redacted"},
		},
		ActionPayload: []byte("encrypted-action"), Status: types.ApprovalPending,
	}
	if err := approvals.CreateApproval(ctx, record); err != nil {
		t.Fatalf("CreateApproval: %v", err)
	}
	got, err := approvals.GetApproval(ctx, record.ID, record.TenantID)
	if err != nil || got.Version != 1 || got.Status != types.ApprovalPending {
		t.Fatalf("GetApproval = %#v, %v", got, err)
	}
	if _, err := approvals.GetApproval(ctx, record.ID, "tenant-b"); err != sql.ErrNoRows {
		t.Fatalf("cross-tenant GetApproval error = %v", err)
	}
	matched, err := approvals.TransitionApproval(ctx, record.ID, record.TenantID, 1, types.ApprovalPending, types.ApprovalApproved, []byte("encrypted-resolution"))
	if err != nil || !matched {
		t.Fatalf("TransitionApproval = %v, %v", matched, err)
	}
	matched, err = approvals.TransitionApproval(ctx, record.ID, record.TenantID, 1, types.ApprovalPending, types.ApprovalRejected, nil)
	if err != nil || matched {
		t.Fatalf("replayed TransitionApproval = %v, %v", matched, err)
	}
	acquired, err := approvals.AcquireApprovalLease(ctx, record.ID, "owner-a", time.Minute)
	if err != nil || !acquired {
		t.Fatalf("AcquireApprovalLease(owner-a) = %v, %v", acquired, err)
	}
	acquired, err = approvals.AcquireApprovalLease(ctx, record.ID, "owner-b", time.Minute)
	if err != nil || acquired {
		t.Fatalf("AcquireApprovalLease(owner-b) = %v, %v", acquired, err)
	}
	if err := approvals.ReleaseApprovalLease(ctx, record.ID, "owner-a"); err != nil {
		t.Fatalf("ReleaseApprovalLease: %v", err)
	}
	listed, err := approvals.ListTaskApprovals(ctx, taskID, record.TenantID, types.ApprovalApproved)
	if err != nil || len(listed) != 1 {
		t.Fatalf("ListTaskApprovals = %#v, %v", listed, err)
	}
}

func TestRedisListTasksBeyondThousand(t *testing.T) {
	redisURL := os.Getenv("TEST_REDIS_URL")
	if redisURL == "" {
		t.Skip("TEST_REDIS_URL not set")
	}
	st, err := store.NewRedisStoreFromURL(redisURL)
	if err != nil {
		t.Fatalf("NewRedisStoreFromURL: %v", err)
	}
	defer st.Close()

	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		t.Fatalf("redis.ParseURL: %v", err)
	}
	admin := redis.NewClient(opts)
	defer admin.Close()

	ctx := context.Background()
	prefix := fmt.Sprintf("pagination-%d", time.Now().UnixNano())
	status := types.TaskStatus(prefix)
	ids := make([]string, 1005)
	keys := make([]string, len(ids))
	for i := range ids {
		ids[i] = fmt.Sprintf("%s-%04d", prefix, i)
		keys[i] = "task:" + ids[i]
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		pipe := admin.TxPipeline()
		pipe.Del(cleanupCtx, keys...)
		members := make([]any, len(ids))
		for i, id := range ids {
			members[i] = id
		}
		pipe.ZRem(cleanupCtx, "tasks:index", members...)
		pipe.ZRem(cleanupCtx, "tasks:index:v2", members...)
		pipe.Del(cleanupCtx, "tasks:status:"+string(status))
		_, _ = pipe.Exec(cleanupCtx)
	})

	for i := range ids {
		task := &types.Task{
			ID:         ids[i],
			Status:     status,
			MaxSteps:   1,
			ToolBudget: 1,
		}
		if err := st.SaveFullTask(ctx, task); err != nil {
			t.Fatalf("SaveFullTask %d: %v", i, err)
		}
	}

	var got []string
	for _, offset := range []int{0, 500, 1000} {
		page, err := st.ListTasks(ctx, store.ListFilter{Status: status, Limit: 500, Offset: offset})
		if err != nil {
			t.Fatalf("ListTasks offset %d: %v", offset, err)
		}
		for _, task := range page {
			got = append(got, task.ID)
		}
	}
	if len(got) != len(ids) {
		t.Fatalf("listed %d tasks, want %d", len(got), len(ids))
	}
	for i := range ids {
		if got[i] != ids[i] {
			t.Fatalf("task %d = %q, want %q", i, got[i], ids[i])
		}
	}
}

// TestAppendTraces verifies the append-only write optimisation in ReplaceTraces:
//   - Scenario A: incremental saves only write new traces (no write amplification).
//   - Scenario B: idempotency — calling SaveFullTask twice with the same traces is safe.
//   - Scenario C: truncation — shortening task.Trace removes the surplus DB rows.
func TestAppendTraces(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "append_traces_test")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	s, err := store.NewSQLiteStore(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer s.Close()

	ctx := context.Background()

	makeTrace := func(step int, action string) types.StepTrace {
		return types.StepTrace{
			Step:        step,
			Goal:        "test goal",
			Action:      action,
			Query:       "q",
			Observation: "obs",
		}
	}

	task := &types.Task{
		ID:         "trace-append-task",
		Goal:       "Trace append test",
		Status:     types.StatusRunning,
		MaxSteps:   10,
		StepCount:  1,
		Workspace:  "/tmp/ws",
		ToolBudget: 10,
		Trace:      []types.StepTrace{makeTrace(1, "find_files")},
	}

	// ── Scenario A: first save writes step 1 ──────────────────────────────────
	if err := s.SaveFullTask(ctx, task); err != nil {
		t.Fatalf("first save: %v", err)
	}
	got, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("get after first save: %v", err)
	}
	if len(got.Trace) != 1 {
		t.Fatalf("scenario A: expected 1 trace, got %d", len(got.Trace))
	}

	// Append step 2 — only step 2 should be written to DB.
	task.Trace = append(task.Trace, makeTrace(2, "search_text"))
	task.StepCount = 2
	if err := s.SaveFullTask(ctx, task); err != nil {
		t.Fatalf("second save: %v", err)
	}
	got, err = s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("get after second save: %v", err)
	}
	if len(got.Trace) != 2 {
		t.Fatalf("scenario A: expected 2 traces after append, got %d", len(got.Trace))
	}
	if got.Trace[0].Action != "find_files" || got.Trace[1].Action != "search_text" {
		t.Errorf("scenario A: unexpected actions %v", []string{got.Trace[0].Action, got.Trace[1].Action})
	}

	// ── Scenario B: idempotency — re-saving the same traces is a no-op ────────
	if err := s.SaveFullTask(ctx, task); err != nil {
		t.Fatalf("idempotent save: %v", err)
	}
	got, err = s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("get after idempotent save: %v", err)
	}
	if len(got.Trace) != 2 {
		t.Fatalf("scenario B: expected still 2 traces, got %d", len(got.Trace))
	}

	// ── Scenario C: truncation — shrink Trace to 1 step; DB row for step 2 must be gone ──
	task.Trace = task.Trace[:1]
	task.StepCount = 1
	if err := s.SaveFullTask(ctx, task); err != nil {
		t.Fatalf("truncation save: %v", err)
	}
	got, err = s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("get after truncation: %v", err)
	}
	if len(got.Trace) != 1 {
		t.Fatalf("scenario C: expected 1 trace after truncation, got %d", len(got.Trace))
	}
	if got.Trace[0].Action != "find_files" {
		t.Errorf("scenario C: expected 'find_files', got %q", got.Trace[0].Action)
	}
}

// TestQueryMemoriesRespectsCandidateLimit is the regression test for the silent
// truncation in SQLiteStore.QueryMemories. Before exposing
// store.memory_candidate_limit, the constant 200 cap meant that once the
// memories table grew beyond 200 rows, the oldest rows were silently excluded
// from cosine/keyword ranking — an older perfect-match memory could be lost
// behind 200 recent unrelated memories. The fix reads the live config so
// operators can raise the cap when recall matters more than scan latency.
//
// Scenario:
//  1. Insert 10 memories at strictly increasing timestamps; the OLDEST memory
//     (mem-0) is the perfect cosine match for our query.
//  2. With candidate_limit=5, the oldest 5 (including mem-0) are excluded by
//     the ORDER BY timestamp DESC LIMIT 5 — ranking returns a *recent*
//     non-match.
//  3. Raise candidate_limit to 100 (live config reload), repeat the query —
//     mem-0 must now win, proving the cap is config-driven and hot-reloadable.
func TestQueryMemoriesRespectsCandidateLimit(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "query_memories_limit_test")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	s, err := store.NewSQLiteStore(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer s.Close()

	ctx := context.Background()

	// mem-0 is the OLDEST and the perfect cosine match for the query embedding.
	// mem-1..mem-9 are progressively newer but orthogonal to the query.
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 10; i++ {
		emb := []float32{0, 1, 0}
		if i == 0 {
			emb = []float32{1, 0, 0}
		}
		if err := s.SaveMemory(ctx, &types.Memory{
			ID:          "mem-" + string(rune('0'+i)),
			TaskID:      "task",
			Goal:        "goal-" + string(rune('0'+i)),
			FinalAnswer: "ans",
			KeyFindings: "find",
			Timestamp:   base.Add(time.Duration(i) * time.Minute),
			Embedding:   emb,
		}); err != nil {
			t.Fatalf("save mem-%d: %v", i, err)
		}
	}

	queryEmb := []float32{1, 0, 0}

	// Stash the original viper value so test pollution can't leak.
	originalLimit := viper.GetInt("store.memory_candidate_limit")
	t.Cleanup(func() {
		viper.Set("store.memory_candidate_limit", originalLimit)
		_, _, _ = config.Reload()
	})

	// Phase 1: cap=5 excludes mem-0 from the candidate set entirely.
	viper.Set("store.memory_candidate_limit", 5)
	if _, _, err := config.Reload(); err != nil {
		t.Fatalf("config reload phase1: %v", err)
	}
	got, err := s.QueryMemories(ctx, "", queryEmb, 1)
	if err != nil {
		t.Fatalf("QueryMemories phase1: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("phase1: expected 1 result, got %d", len(got))
	}
	if got[0].ID == "mem-0" {
		t.Errorf("phase1: with candidate_limit=5, mem-0 should be excluded by the ORDER BY timestamp DESC cap, but it ranked first — truncation cap not effective")
	}

	// Phase 2: cap=100 lets mem-0 back into the candidate set; cosine ranking
	// must surface it as the top result.
	viper.Set("store.memory_candidate_limit", 100)
	if _, _, err := config.Reload(); err != nil {
		t.Fatalf("config reload phase2: %v", err)
	}
	got, err = s.QueryMemories(ctx, "", queryEmb, 1)
	if err != nil {
		t.Fatalf("QueryMemories phase2: %v", err)
	}
	if len(got) != 1 || got[0].ID != "mem-0" {
		ids := make([]string, 0, len(got))
		for _, m := range got {
			ids = append(ids, m.ID)
		}
		t.Errorf("phase2: with candidate_limit=100, expected mem-0 (perfect cosine match) to win, got %v — config reload of memory_candidate_limit may not be hot-reloadable", ids)
	}
}
