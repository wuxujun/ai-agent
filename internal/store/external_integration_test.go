package store

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/wuxujun/ai-agent/internal/config"
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
			pipe.Del(ctx, st.sessionSequenceKey(ids.sessionA))
			pipe.Del(ctx, st.sessionSequenceKey(ids.sessionB))
			pipe.ZRem(ctx, "sessions:tenant:"+ids.tenantA, ids.sessionA)
			pipe.ZRem(ctx, "sessions:tenant:"+ids.tenantB, ids.sessionB)
			_, _ = pipe.Exec(ctx)
		})
	})
}

func TestExternalPostgresPGVectorRanking(t *testing.T) {
	requireExternalIntegration(t)
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not set")
	}
	restore := config.OverrideForTesting(func(cfg *config.Config) {
		cfg.Store.VectorSearch = "pgvector"
		cfg.Store.PGVectorDimensions = 3
	})
	defer restore()

	st, err := NewPostgresStore(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	prefix := "integration-pgvector-" + uuid.NewString()
	tenantID := prefix + "-tenant"
	sessionID := prefix + "-session"
	memoryNear := prefix + "-near"
	memoryFar := prefix + "-far"
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = st.DeleteMemory(cleanupCtx, memoryNear, tenantID)
		_, _ = st.DeleteMemory(cleanupCtx, memoryFar, tenantID)
	})

	now := time.Now().UTC()
	for _, memory := range []*types.Memory{
		{ID: memoryNear, TenantID: tenantID, SessionID: sessionID, Goal: "near vector", Timestamp: now, Embedding: []float32{1, 0, 0}},
		{ID: memoryFar, TenantID: tenantID, SessionID: sessionID, Goal: "far vector", Timestamp: now, Embedding: []float32{0, 1, 0}},
	} {
		if err := st.SaveMemory(t.Context(), memory); err != nil {
			t.Fatalf("SaveMemory(%s): %v", memory.ID, err)
		}
	}
	queryCtx := WithSessionScope(WithTenantScope(t.Context(), tenantID), sessionID)
	memories, err := st.QueryMemories(queryCtx, "", []float32{1, 0, 0}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(memories) != 2 || memories[0].ID != memoryNear || memories[1].ID != memoryFar {
		t.Fatalf("pgvector ranking = %+v", memories)
	}
	var extensionVersion string
	if err := st.db.QueryRowContext(t.Context(), `SELECT extversion FROM pg_extension WHERE extname = 'vector'`).Scan(&extensionVersion); err != nil {
		t.Fatalf("pgvector extension not enabled: %v", err)
	}
	if extensionVersion == "" {
		t.Fatal("pgvector extension version is empty")
	}
}

func TestExternalPostgresMigrationUpgradesLegacySchema(t *testing.T) {
	requireExternalIntegration(t)
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not set")
	}
	parsedDSN, err := url.Parse(dsn)
	if err != nil || parsedDSN.Scheme == "" {
		t.Fatalf("TEST_POSTGRES_DSN must be a URL for isolated schema testing: %v", err)
	}
	admin, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schemaName := "integration_legacy_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quotedSchema := pq.QuoteIdentifier(schemaName)
	if _, err := admin.ExecContext(t.Context(), `CREATE SCHEMA `+quotedSchema); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = admin.ExecContext(cleanupCtx, `DROP SCHEMA `+quotedSchema+` CASCADE`)
	}()
	legacyDSN := *parsedDSN
	query := legacyDSN.Query()
	query.Set("search_path", schemaName)
	legacyDSN.RawQuery = query.Encode()
	legacyDB, err := sql.Open("postgres", legacyDSN.String())
	if err != nil {
		t.Fatal(err)
	}
	legacySchema := `
CREATE TABLE tasks (
    id VARCHAR(255) PRIMARY KEY, goal TEXT NOT NULL, status VARCHAR(50) NOT NULL,
    max_steps INT NOT NULL, step_count INT NOT NULL, workspace TEXT NOT NULL,
    hypothesis TEXT NOT NULL, unresolved_json TEXT NOT NULL, tool_budget INT NOT NULL,
    final_answer TEXT NOT NULL
);
CREATE TABLE traces (
    id SERIAL PRIMARY KEY, task_id VARCHAR(255) NOT NULL, step INT NOT NULL,
    goal TEXT NOT NULL, action TEXT NOT NULL, query TEXT NOT NULL,
    observation TEXT NOT NULL, evidence_json TEXT NOT NULL
);
CREATE TABLE memories (
    id VARCHAR(255) PRIMARY KEY, task_id VARCHAR(255) NOT NULL, goal TEXT NOT NULL,
    final_answer TEXT NOT NULL, key_findings TEXT NOT NULL, timestamp TIMESTAMP NOT NULL,
    embedding_json TEXT NOT NULL
);
INSERT INTO tasks (id, goal, status, max_steps, step_count, workspace, hypothesis, unresolved_json, tool_budget, final_answer)
VALUES ('legacy-task', 'legacy goal', 'created', 1, 0, '', '', '[]', 1, '');`
	if _, err := legacyDB.ExecContext(t.Context(), legacySchema); err != nil {
		_ = legacyDB.Close()
		t.Fatal(err)
	}
	if err := legacyDB.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := NewPostgresStore(legacyDSN.String())
	if err != nil {
		t.Fatalf("NewPostgresStore() legacy migration: %v", err)
	}
	defer st.Close()
	assertPostgresColumns(t, st.db, "tasks", "tenant_id", "session_id", "sequence_no", "created_at", "updated_at", "token_budget", "llm_call_budget", "memories_json", "answer_audit_json")
	assertPostgresColumns(t, st.db, "traces", "agent_role", "error_text", "prompt_tokens", "completion_tokens", "total_tokens")
	assertPostgresColumns(t, st.db, "memories", "tenant_id", "session_id")
	var createdAt, updatedAt sql.NullTime
	if err := st.db.QueryRowContext(t.Context(), `SELECT created_at, updated_at FROM tasks WHERE id = 'legacy-task'`).Scan(&createdAt, &updatedAt); err != nil {
		t.Fatal(err)
	}
	if !createdAt.Valid || !updatedAt.Valid {
		t.Fatalf("legacy timestamps were not backfilled: created=%v updated=%v", createdAt, updatedAt)
	}
}

func TestExternalStoresPersistPausedTaskAcrossClients(t *testing.T) {
	requireExternalIntegration(t)
	tests := []struct {
		name    string
		enabled bool
		open    func() (externalIntegrationStore, error)
	}{
		{
			name:    "postgres",
			enabled: os.Getenv("TEST_POSTGRES_DSN") != "",
			open: func() (externalIntegrationStore, error) {
				return NewPostgresStore(os.Getenv("TEST_POSTGRES_DSN"))
			},
		},
		{
			name:    "redis",
			enabled: os.Getenv("TEST_REDIS_URL") != "",
			open: func() (externalIntegrationStore, error) {
				return NewRedisStoreFromURL(os.Getenv("TEST_REDIS_URL"))
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !tt.enabled {
				t.Skip("backend DSN is not set")
			}
			first, err := tt.open()
			if err != nil {
				t.Fatal(err)
			}
			taskID := "integration-restart-" + uuid.NewString()
			task := &types.Task{
				ID: taskID, TenantID: taskID + "-tenant", Goal: "resume after restart",
				Status: types.StatusPaused, MaxSteps: 2, ToolBudget: 1,
				Trace: []types.StepTrace{{Step: 1, Action: "multiagent_workflow_checkpoint", Observation: `{"version":1,"state":"paused"}`}},
			}
			if err := first.SaveFullTask(t.Context(), task); err != nil {
				_ = first.Close()
				t.Fatal(err)
			}
			if err := first.Close(); err != nil {
				t.Fatal(err)
			}

			second, err := tt.open()
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_, _ = second.DeleteTask(cleanupCtx, taskID)
				_ = second.Close()
			}()
			restored, err := second.GetTask(t.Context(), taskID)
			if err != nil {
				t.Fatal(err)
			}
			if restored.Status != types.StatusPaused || len(restored.Trace) != 1 || restored.Trace[0].Action != "multiagent_workflow_checkpoint" {
				t.Fatalf("restored paused task = %+v", restored)
			}
		})
	}
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
	acquired, err = st.AcquireTaskLease(ctx, ids.lease, "owner-b", 150*time.Millisecond)
	if err != nil || !acquired {
		t.Fatalf("owner-b AcquireTaskLease after release = %v, %v", acquired, err)
	}
	expiryDeadline := time.Now().Add(3 * time.Second)
	for {
		acquired, err = st.AcquireTaskLease(ctx, ids.lease, "owner-a", time.Second)
		if err != nil {
			t.Fatalf("owner-a AcquireTaskLease after expiry: %v", err)
		}
		if acquired {
			break
		}
		if time.Now().After(expiryDeadline) {
			t.Fatal("expired lease was not available to a new owner")
		}
		time.Sleep(25 * time.Millisecond)
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

func assertPostgresColumns(t *testing.T, db *sql.DB, table string, expected ...string) {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `SELECT column_name FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = $1`, table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns := make(map[string]bool)
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			t.Fatal(err)
		}
		columns[column] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, column := range expected {
		if !columns[column] {
			t.Errorf("%s.%s was not migrated", table, column)
		}
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
