package store

import (
	"context"
	"errors"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/types"
)

// Opt-in only: these contracts write solely to explicitly selected disposable
// backends, just like the existing external integration suite.
func TestExternalSessionCreationConflictAndBackendFailure(t *testing.T) {
	requireExternalIntegration(t)
	for _, backend := range []string{"postgres", "redis"} {
		t.Run(backend, func(t *testing.T) {
			var st Store
			var err error
			if backend == "postgres" {
				dsn := os.Getenv("TEST_POSTGRES_DSN")
				if dsn == "" {
					t.Skip("TEST_POSTGRES_DSN missing")
				}
				st, err = NewPostgresStore(dsn)
			} else {
				address := os.Getenv("TEST_REDIS_URL")
				if address == "" {
					t.Skip("TEST_REDIS_URL missing")
				}
				st, err = NewRedisStoreFromURL(address)
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			sessions := st.(SessionStore)
			id := "session-optimization-" + uuid.NewString()
			first := &types.Session{ID: id, TenantID: id + "-tenant", Title: "original"}
			if err := sessions.CreateSession(t.Context(), first); err != nil {
				t.Fatal(err)
			}
			before, err := sessions.GetSession(t.Context(), id, first.TenantID)
			if err != nil {
				t.Fatal(err)
			}
			for _, tenant := range []string{first.TenantID, id + "-other"} {
				if err := sessions.CreateSession(t.Context(), &types.Session{ID: id, TenantID: tenant, Title: "replacement"}); !errors.Is(err, ErrSessionExists) {
					t.Fatalf("duplicate not classified: %v", err)
				}
				after, err := sessions.GetSession(t.Context(), id, first.TenantID)
				if err != nil || !reflect.DeepEqual(before, after) {
					t.Fatalf("duplicate altered session: %+v %v", after, err)
				}
			}
			canceled, cancel := context.WithCancel(t.Context())
			cancel()
			if err := sessions.CreateSession(canceled, &types.Session{ID: id + "-canceled"}); err == nil || errors.Is(err, ErrSessionExists) {
				t.Fatalf("cancellation misclassified: %v", err)
			}
			// Remove only this fixture's record before deliberately closing its
			// client. Dedicated backends may also be reused by a later CI run.
			switch backend := st.(type) {
			case *PostgresStore:
				if _, err := backend.db.ExecContext(t.Context(), `DELETE FROM sessions WHERE id=$1`, id); err != nil {
					t.Fatal(err)
				}
			case *RedisStore:
				if err := backend.client.Del(t.Context(), backend.sessionKey(id)).Err(); err != nil {
					t.Fatal(err)
				}
				if err := backend.client.ZRem(t.Context(), "sessions:tenant:"+first.TenantID, id).Err(); err != nil {
					t.Fatal(err)
				}
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			if err := sessions.CreateSession(t.Context(), &types.Session{ID: id + "-unavailable"}); err == nil || errors.Is(err, ErrSessionExists) {
				t.Fatalf("closed backend misclassified: %v", err)
			}
		})
	}
}

func TestExternalPostgresTaskProgress(t *testing.T) {
	requireExternalIntegration(t)
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN missing")
	}
	st, err := NewPostgresStore(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := t.Context()
	task := capacityTask(2)
	task.ID = "progress-optimization-" + uuid.NewString()
	task.TenantID = task.ID
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = st.DeleteTask(context.Background(), task.ID) })
	before, err := st.ListTaskTraces(ctx, task.ID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := CaptureTaskProgress(st, task)
	if err := st.SaveTaskProgress(WithTaskLease(ctx, task.ID, "missing"), task, checkpoint); !errors.Is(err, ErrTaskLeaseLost) {
		t.Fatalf("missing lease: %v", err)
	}
	if acquired, err := st.AcquireTaskLease(ctx, task.ID, "progress-owner", time.Minute); err != nil || !acquired {
		t.Fatalf("acquire lease: %v %v", acquired, err)
	}
	defer func() { _ = st.ReleaseTaskLease(context.Background(), task.ID, "progress-owner") }()
	scoped := WithTaskLease(ctx, task.ID, "progress-owner")
	if err := st.SaveTaskProgress(WithTaskLease(ctx, task.ID, "wrong-owner"), task, checkpoint); !errors.Is(err, ErrTaskLeaseLost) {
		t.Fatalf("wrong owner: %v", err)
	}

	// A BEFORE INSERT trigger also fires for an upsert that would do nothing.
	// Rejecting old positions proves the append path never resubmits the prefix.
	removeTrigger := installProgressRejection(t, st, task.ID, "NEW.step <= 2")
	task.Trace = append(task.Trace, types.StepTrace{Step: 9, Action: "new"})
	if err := st.SaveTaskProgress(scoped, task, checkpoint); err != nil {
		t.Fatal(err)
	}
	after, err := st.ListTaskTraces(ctx, task.ID, 0, 10)
	if err != nil || len(after) != 3 || !reflect.DeepEqual(before, after[:2]) || after[2].Sequence != 3 {
		t.Fatalf("prefix/order/recorded_at changed: %v", err)
	}
	removeTrigger()

	for _, change := range []func(){
		func() { task.Trace[0].Evidence[0].Lines[0] = "changed" },
		func() { task.Trace[0], task.Trace[1] = task.Trace[1], task.Trace[0] },
		func() { task.Trace = task.Trace[:1] },
	} {
		checkpoint = CaptureTaskProgress(st, task)
		change()
		if err := st.SaveTaskProgress(scoped, task, checkpoint); err != nil {
			t.Fatal(err)
		}
		loaded, err := st.GetTask(ctx, task.ID)
		if err != nil || !reflect.DeepEqual(loaded.Trace, task.Trace) {
			t.Fatalf("rewrite lost: %v", err)
		}
	}
	checkpoint = CaptureTaskProgress(st, task)
	peer := types.CloneTask(task)
	peer.Goal = "peer changed"
	if err := st.SaveFullTask(scoped, peer); err != nil {
		t.Fatal(err)
	}
	task.Trace = append(task.Trace, types.StepTrace{Action: "stale"})
	if err := st.SaveTaskProgress(scoped, task, checkpoint); !errors.Is(err, ErrTaskProgressConflict) {
		t.Fatalf("stale checkpoint: %v", err)
	}
	stable, err := st.GetTask(ctx, task.ID)
	if err != nil || stable.Goal != "peer changed" || len(stable.Trace) != 1 {
		t.Fatal("stale write changed task")
	}

	task, err = st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint = CaptureTaskProgress(st, task)
	removeTrigger = installProgressRejection(t, st, task.ID, "NEW.action = 'reject'")
	task.Goal = "metadata that must roll back"
	task.Trace = append(task.Trace, types.StepTrace{Action: "valid"}, types.StepTrace{Action: "reject"})
	if err := st.SaveTaskProgress(scoped, task, checkpoint); err == nil {
		t.Fatal("injected error did not abort")
	}
	loaded, err := st.GetTask(ctx, task.ID)
	if err != nil || !reflect.DeepEqual(stable, loaded) {
		t.Fatal("failed suffix committed metadata/version/traces")
	}
	removeTrigger()

	// Two writers starting from one acknowledged version must yield one winner.
	// Both use the same valid lease; the maintenance version provides the CAS.
	task = loaded
	checkpoint = CaptureTaskProgress(st, task)
	copies := []*types.Task{types.CloneTask(task), types.CloneTask(task)}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i, item := range copies {
		item.Trace = append(item.Trace, types.StepTrace{Action: []string{"writer-a", "writer-b"}[i]})
		wg.Add(1)
		go func(item *types.Task) { defer wg.Done(); results <- st.SaveTaskProgress(scoped, item, checkpoint) }(item)
	}
	wg.Wait()
	close(results)
	successes, conflicts := 0, 0
	for err := range results {
		if err == nil {
			successes++
		} else if errors.Is(err, ErrTaskProgressConflict) {
			conflicts++
		} else {
			t.Fatalf("unexpected writer error: %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("writers: success=%d conflicts=%d", successes, conflicts)
	}
	loaded, err = st.GetTask(ctx, task.ID)
	if err != nil || len(loaded.Trace) != 2 || loaded.MaintenanceVersion != task.MaintenanceVersion+1 {
		t.Fatal("concurrent progress inconsistent")
	}
}

func installProgressRejection(t *testing.T, st *PostgresStore, taskID, condition string) func() {
	t.Helper()
	name := "progress_reject_" + uuid.NewString()[:8]
	identifier := pq.QuoteIdentifier(name)
	body := `CREATE FUNCTION ` + identifier + `() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.task_id = ` + pq.QuoteLiteral(taskID) + ` AND (` + condition + `) THEN RAISE EXCEPTION 'injected progress failure'; END IF; RETURN NEW; END; $$`
	if _, err := st.db.ExecContext(t.Context(), body); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(t.Context(), `CREATE TRIGGER `+identifier+` BEFORE INSERT ON traces FOR EACH ROW EXECUTE FUNCTION `+identifier+`()`); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := st.db.ExecContext(ctx, `DROP TRIGGER `+identifier+` ON traces`); err != nil {
				t.Error(err)
			}
			if _, err := st.db.ExecContext(ctx, `DROP FUNCTION `+identifier+`() `); err != nil {
				t.Error(err)
			}
		})
	}
	t.Cleanup(cleanup)
	return cleanup
}

func TestExternalPostgresIndexedMemoryParent(t *testing.T) {
	requireExternalIntegration(t)
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN missing")
	}
	for _, mode := range []string{"in_process", "pgvector"} {
		t.Run(mode, func(t *testing.T) {
			t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) { cfg.Store.VectorSearch = mode; cfg.Store.PGVectorDimensions = 3 }))
			st, err := NewPostgresStore(dsn)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			id := "indexed-memory-" + uuid.NewString()
			task := &types.Task{ID: id, TenantID: id, Status: types.StatusRunning}
			if err := st.CreateTask(t.Context(), task); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _, _ = st.DeleteTask(context.Background(), id) })
			mem := &types.Memory{ID: id, TaskID: id, TenantID: id, Goal: "fixture", Timestamp: time.Now().UTC(), Embedding: []float32{1, 0, 0}}
			ctx := context.WithValue(t.Context(), indexedMemoryKey{}, true)
			if err := st.SaveMemory(ctx, mem); err != nil {
				t.Fatal(err)
			}
			items, err := st.ListMemories(ctx, ListMemoryFilter{TenantID: id})
			if err != nil || len(items) != 1 {
				t.Fatalf("parented memory missing: %v", err)
			}
			if deleted, err := st.DeleteTask(ctx, id); err != nil || !deleted {
				t.Fatalf("delete parent: %v %v", deleted, err)
			}
			if err := st.SaveMemory(ctx, mem); err != nil {
				t.Fatal(err)
			}
			items, err = st.ListMemories(ctx, ListMemoryFilter{TenantID: id})
			if err != nil || len(items) != 0 {
				t.Fatalf("deleted parent memory resurrected: %v", err)
			}
		})
	}
}

func TestExternalPostgresExportBytePreflight(t *testing.T) {
	requireExternalIntegration(t)
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN missing")
	}
	st, err := NewPostgresStore(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var measured, binarySize int
	var text string
	payload := []byte{0, 255, 1, 128, 42}
	expression := exportSQLBytes([]string{"request_json", "action_payload"}, true)
	query := "SELECT " + expression + ", request_json::text, octet_length(action_payload) FROM (SELECT $1::jsonb AS request_json, $2::bytea AS action_payload) AS fixture"
	if err := st.db.QueryRowContext(t.Context(), query, `{"nested":{"text":"汉字"}}`, payload).Scan(&measured, &text, &binarySize); err != nil {
		t.Fatal(err)
	}
	if binarySize != len(payload) || measured != len([]byte(text))+len(payload) {
		t.Fatalf("byte estimate=%d json=%d binary=%d", measured, len([]byte(text)), binarySize)
	}
}

func TestExternalRedisReadOnlySnapshotExpiry(t *testing.T) {
	requireExternalIntegration(t)
	address := os.Getenv("TEST_REDIS_URL")
	if address == "" {
		t.Skip("TEST_REDIS_URL missing")
	}
	st, err := NewRedisStoreFromURL(address)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	id := "snapshot-fixture-" + uuid.NewString()
	owned, unrelated := id+":owned", "snapshot-unrelated-"+uuid.NewString()
	defer st.client.Del(context.Background(), owned)
	defer st.client.Del(context.Background(), unrelated)
	if err := st.client.Set(t.Context(), owned, "before", time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	if err := st.client.Set(t.Context(), unrelated, "transient", time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	before := jointRedisDump(t, st, id)
	if _, ok := before[owned]; !ok {
		t.Fatal("fixture TTL key omitted")
	}
	if _, ok := before[unrelated]; ok {
		t.Fatal("unrelated TTL key included")
	}
	if err := st.client.PExpireAt(t.Context(), unrelated, time.Unix(0, 0)).Err(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, jointRedisDump(t, st, id)) {
		t.Fatal("unrelated expiry changed snapshot")
	}
	if err := st.client.Set(t.Context(), owned, "after", time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(before, jointRedisDump(t, st, id)) {
		t.Fatal("fixture mutation was hidden")
	}
}
