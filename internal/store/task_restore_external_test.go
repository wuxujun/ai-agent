package store

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wuxujun/ai-agent/internal/types"
)

// These fixtures intentionally preserve their source records. Never point this
// test at an online database: both opt-in gates and a dedicated URL are needed.
func TestExternalRedisPersistentTaskRestore(t *testing.T) {
	requireExternalIntegration(t)
	if os.Getenv("AI_AGENT_RESTORE_DEDICATED_DB") != "true" {
		t.Skip("dedicated restore fixture database opt-in missing")
	}
	address := os.Getenv("TEST_REDIS_URL")
	if address == "" {
		t.Skip("TEST_REDIS_URL missing")
	}
	source, err := NewRedisStoreFromURL(address)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	checkPersistentRestoreFromSource(t, source, "redis")
}

func TestExternalPostgresPersistentTaskRestore(t *testing.T) {
	requireExternalIntegration(t)
	if os.Getenv("AI_AGENT_RESTORE_DEDICATED_DB") != "true" {
		t.Skip("dedicated restore fixture database opt-in missing")
	}
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN missing")
	}
	source, err := NewPostgresStore(dsn)
	if err != nil {
		t.Fatal("cannot open dedicated PostgreSQL fixture")
	}
	defer source.Close()
	checkPersistentRestoreFromSource(t, source, "postgres")
}

func checkPersistentRestoreFromSource(t *testing.T, source Store, backend string) {
	t.Helper()
	id := "persistent-restore-" + uuid.NewString()
	tenant := id + "-tenant"
	session := &types.Session{ID: id + "-session", TenantID: tenant, Status: types.SessionStatusActive, Title: "private-session"}
	if err := source.(SessionStore).CreateSession(t.Context(), session); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := source.(SessionStore).NextSessionTaskSequence(t.Context(), session.ID, tenant); err != nil {
			t.Fatal(err)
		}
	}
	task := &types.Task{ID: id, TenantID: tenant, SessionID: session.ID, SequenceNo: 1, Status: types.StatusFailed, Goal: "private-task", Trace: []types.StepTrace{{Step: 1, Observation: "private-trace"}, {Step: 2}}}
	if err := source.SaveFullTask(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	approval := &types.DurableApproval{ID: id + "-approval", TaskID: id, TenantID: tenant, Version: 1, Status: types.ApprovalPending, Request: types.ApprovalRequest{ID: id + "-approval", TaskID: id, Action: "write", RiskLevel: types.RiskLevelHigh, Parameters: map[string]any{"large": int64(9007199254740993)}}, ActionPayload: []byte{0, 1, 255}}
	if err := source.(DurableApprovalStore).CreateApproval(t.Context(), approval); err != nil {
		t.Fatal(err)
	}
	if ok, err := source.(DurableApprovalStore).TransitionApproval(t.Context(), approval.ID, tenant, 1, types.ApprovalPending, types.ApprovalApproved, []byte{7, 8, 254}); err != nil || !ok {
		t.Fatal("approval fixture failed", err)
	}
	memory := &types.Memory{ID: id + "-memory", TaskID: id, TenantID: tenant, SessionID: session.ID, Timestamp: time.Now().UTC(), Goal: "private-memory", Embedding: []float32{.125, .875}}
	if err := source.SaveMemory(t.Context(), memory); err != nil {
		t.Fatal(err)
	}
	reader := source.(TaskJointExportReader)
	before, err := reader.ReadJointTaskExport(t.Context(), id, tenant, MaxTaskExportBytes)
	if err != nil {
		t.Fatal(err)
	}
	var beforeKeys map[string]string
	if redis, ok := source.(*RedisStore); ok {
		beforeKeys = jointRedisDump(t, redis, id)
	}
	artifact, err := EncodeTaskExport(t.Context(), before, backend, id, tenant, MaxTaskExportBytes)
	if err != nil {
		t.Fatal(err)
	}
	report, dir, err := VerifyPersistentTaskRestore(t.Context(), artifact, tenant, t.TempDir(), MaxTaskExportBytes)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Verified || !report.Reopened || report.RestoreReady || !report.SourcePreserved || report.SourceBackend != backend || report.TraceCount != 2 || report.ApprovalCount != 1 || report.MemoryCount != 1 {
		t.Fatal("wrong persisted restore report")
	}
	reopened, err := OpenPersistentTaskRestoreSandbox(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Verify(t.Context(), id, tenant); err != nil {
		t.Fatal(err)
	}
	reopened.Close()
	after, err := reader.ReadJointTaskExport(t.Context(), id, tenant, MaxTaskExportBytes)
	if err != nil {
		t.Fatal(err)
	}
	beforeJSON, _ := json.Marshal(before)
	afterJSON, _ := json.Marshal(after)
	if !bytes.Equal(beforeJSON, afterJSON) {
		t.Fatal("persistent restore changed source")
	}
	if redis, ok := source.(*RedisStore); ok && !reflect.DeepEqual(beforeKeys, jointRedisDump(t, redis, id)) {
		t.Fatal("persistent restore wrote Redis application keys")
	}
	reportJSON, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("persistent restore report: %s", reportJSON)
}

func TestPersistentTaskRestoreSourceContracts(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			var source Store = NewMemoryStore()
			if backend == "sqlite" {
				var err error
				source, err = NewSQLiteStore(t.TempDir() + "/source.sqlite")
				if err != nil {
					t.Fatal(err)
				}
			}
			defer source.Close()
			checkPersistentRestoreFromSource(t, source, backend)
		})
	}
}
