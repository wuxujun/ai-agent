package store

import (
	"context"
	"errors"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wuxujun/ai-agent/internal/types"
)

type externalIntegrationStore interface {
	Store
	SessionStore
	MemoryManagementStore
	TaskDeletionStore
}

func TestExternalStoresSessionLeaseAndIsolation(t *testing.T) {
	requireExternalIntegration(t)

	t.Run("postgres", func(t *testing.T) {
		dsn := os.Getenv("TEST_POSTGRES_DSN")
		if dsn == "" {
			t.Skip("TEST_POSTGRES_DSN is not set")
		}
		st, err := NewPostgresStore(dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		runExternalStoreContract(t, st, func(ctx context.Context, ids externalIntegrationIDs) {
			_, _ = st.db.ExecContext(ctx, `DELETE FROM sessions WHERE id IN ($1, $2)`, ids.sessionA, ids.sessionB)
		})
	})

	t.Run("redis", func(t *testing.T) {
		redisURL := os.Getenv("TEST_REDIS_URL")
		if redisURL == "" {
			t.Skip("TEST_REDIS_URL is not set")
		}
		st, err := NewRedisStoreFromURL(redisURL)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		runExternalStoreContract(t, st, func(ctx context.Context, ids externalIntegrationIDs) {
			pipe := st.client.TxPipeline()
			pipe.Del(ctx, st.sessionKey(ids.sessionA))
			pipe.Del(ctx, st.sessionKey(ids.sessionB))
			pipe.ZRem(ctx, "sessions:tenant:"+ids.tenantA, ids.sessionA)
			pipe.ZRem(ctx, "sessions:tenant:"+ids.tenantB, ids.sessionB)
			_, _ = pipe.Exec(ctx)
		})
	})
}

type externalIntegrationIDs struct {
	tenantA  string
	tenantB  string
	sessionA string
	sessionB string
	taskA    string
	taskB    string
	memoryA  string
	memoryB  string
	lease    string
}

func newExternalIntegrationIDs() externalIntegrationIDs {
	prefix := "integration-" + uuid.NewString()
	return externalIntegrationIDs{
		tenantA:  prefix + "-tenant-a",
		tenantB:  prefix + "-tenant-b",
		sessionA: prefix + "-session-a",
		sessionB: prefix + "-session-b",
		taskA:    prefix + "-task-a",
		taskB:    prefix + "-task-b",
		memoryA:  prefix + "-memory-a",
		memoryB:  prefix + "-memory-b",
		lease:    prefix + "-lease",
	}
}

func runExternalStoreContract(t *testing.T, st externalIntegrationStore, cleanupSessions func(context.Context, externalIntegrationIDs)) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	ids := newExternalIntegrationIDs()
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		_ = st.ReleaseTaskLease(cleanupCtx, ids.lease, "owner-a")
		_ = st.ReleaseTaskLease(cleanupCtx, ids.lease, "owner-b")
		_, _ = st.DeleteMemory(cleanupCtx, ids.memoryA, ids.tenantA)
		_, _ = st.DeleteMemory(cleanupCtx, ids.memoryB, ids.tenantB)
		_, _ = st.DeleteTask(cleanupCtx, ids.taskA)
		_, _ = st.DeleteTask(cleanupCtx, ids.taskB)
		cleanupSessions(cleanupCtx, ids)
	})

	for _, session := range []*types.Session{
		{ID: ids.sessionA, TenantID: ids.tenantA, Title: "integration A"},
		{ID: ids.sessionB, TenantID: ids.tenantB, Title: "integration B"},
	} {
		if err := st.CreateSession(ctx, session); err != nil {
			t.Fatalf("CreateSession(%s): %v", session.ID, err)
		}
	}
	if _, err := st.GetSession(ctx, ids.sessionA, ids.tenantB); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("cross-tenant GetSession error = %v", err)
	}

	const sequenceCount = 8
	sequences := make(chan int64, sequenceCount)
	errorsCh := make(chan error, sequenceCount)
	var wg sync.WaitGroup
	for range sequenceCount {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sequence, err := st.NextSessionTaskSequence(ctx, ids.sessionA, ids.tenantA)
			if err != nil {
				errorsCh <- err
				return
			}
			sequences <- sequence
		}()
	}
	wg.Wait()
	close(sequences)
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatalf("concurrent NextSessionTaskSequence: %v", err)
		}
	}
	gotSequences := make([]int, 0, sequenceCount)
	for sequence := range sequences {
		gotSequences = append(gotSequences, int(sequence))
	}
	sort.Ints(gotSequences)
	for i, sequence := range gotSequences {
		if want := i + 1; sequence != want {
			t.Fatalf("sequences = %v; missing or duplicate value %d", gotSequences, want)
		}
	}

	now := time.Now().UTC()
	for _, task := range []*types.Task{
		{ID: ids.taskA, TenantID: ids.tenantA, SessionID: ids.sessionA, SequenceNo: 1, Goal: "integration isolated evidence A", Status: types.StatusCreated, MaxSteps: 1, ToolBudget: 1},
		{ID: ids.taskB, TenantID: ids.tenantB, SessionID: ids.sessionB, SequenceNo: 1, Goal: "integration isolated evidence B", Status: types.StatusCreated, MaxSteps: 1, ToolBudget: 1},
	} {
		if err := st.SaveFullTask(ctx, task); err != nil {
			t.Fatalf("SaveFullTask(%s): %v", task.ID, err)
		}
	}
	for _, memory := range []*types.Memory{
		{ID: ids.memoryA, TenantID: ids.tenantA, SessionID: ids.sessionA, TaskID: ids.taskA, Goal: "integration isolated evidence A", KeyFindings: "unique A", Timestamp: now},
		{ID: ids.memoryB, TenantID: ids.tenantB, SessionID: ids.sessionB, TaskID: ids.taskB, Goal: "integration isolated evidence B", KeyFindings: "unique B", Timestamp: now.Add(time.Millisecond)},
	} {
		if err := st.SaveMemory(ctx, memory); err != nil {
			t.Fatalf("SaveMemory(%s): %v", memory.ID, err)
		}
	}

	tasks, err := st.ListTasks(ctx, ListFilter{TenantID: ids.tenantA, SessionID: ids.sessionA, Limit: 10})
	if err != nil || len(tasks) != 1 || tasks[0].ID != ids.taskA {
		t.Fatalf("tenant/session tasks = %+v, err=%v", tasks, err)
	}
	memCtx := WithSessionScope(WithTenantScope(ctx, ids.tenantA), ids.sessionA)
	memories, err := st.QueryMemories(memCtx, "integration isolated evidence", nil, 10)
	if err != nil || len(memories) != 1 || memories[0].ID != ids.memoryA {
		t.Fatalf("tenant/session memories = %+v, err=%v", memories, err)
	}

	acquired, err := st.AcquireTaskLease(ctx, ids.lease, "owner-a", 5*time.Second)
	if err != nil || !acquired {
		t.Fatalf("owner-a AcquireTaskLease = %v, %v", acquired, err)
	}
	acquired, err = st.AcquireTaskLease(ctx, ids.lease, "owner-b", 5*time.Second)
	if err != nil || acquired {
		t.Fatalf("owner-b competing AcquireTaskLease = %v, %v", acquired, err)
	}
	if err := st.ReleaseTaskLease(ctx, ids.lease, "owner-a"); err != nil {
		t.Fatal(err)
	}
	acquired, err = st.AcquireTaskLease(ctx, ids.lease, "owner-b", 5*time.Second)
	if err != nil || !acquired {
		t.Fatalf("owner-b AcquireTaskLease after release = %v, %v", acquired, err)
	}

	session, err := st.GetSession(ctx, ids.sessionA, ids.tenantA)
	if err != nil {
		t.Fatal(err)
	}
	session.Status = types.SessionStatusArchived
	if err := st.UpdateSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	if _, err := st.NextSessionTaskSequence(ctx, ids.sessionA, ids.tenantA); !errors.Is(err, ErrSessionArchived) {
		t.Fatalf("archived NextSessionTaskSequence error = %v", err)
	}
}

func requireExternalIntegration(t *testing.T) {
	t.Helper()
	if os.Getenv("AI_AGENT_RUN_EXTERNAL_INTEGRATION") != "true" {
		t.Skip("set AI_AGENT_RUN_EXTERNAL_INTEGRATION=true to run tests against dedicated external services")
	}
	if os.Getenv("TEST_POSTGRES_DSN") == "" && os.Getenv("TEST_REDIS_URL") == "" {
		t.Skip("TEST_POSTGRES_DSN and TEST_REDIS_URL are both unset")
	}
}
