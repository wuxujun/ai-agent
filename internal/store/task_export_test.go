package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wuxujun/ai-agent/internal/types"
)

func TestTaskExportContract(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			var st Store = NewMemoryStore()
			if backend == "sqlite" {
				var err error
				st, err = NewSQLiteStore(filepath.Join(t.TempDir(), "export.db"))
				if err != nil {
					t.Fatal(err)
				}
			}
			defer st.Close()
			checkTaskExport(t, st)
		})
	}
}
func checkTaskExport(t *testing.T, st Store) {
	t.Helper()
	ctx := t.Context()
	id := "export-" + uuid.NewString()
	tenant := id + "-tenant"
	at := time.Now().UTC().Add(-time.Hour)
	duration := int64(42)
	task := &types.Task{ID: id, TenantID: tenant, Goal: "0", Status: types.StatusFailed, SequenceNo: (1 << 53) + 1, Trace: []types.StepTrace{{Step: 1, Observation: "0", OccurredAt: &at, DurationMS: &duration, Evidence: []types.Evidence{{Lines: []string{"private <>& \\"}}}}, {Step: 1, Observation: "0"}}}
	if err := st.SaveFullTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	defer st.(TaskDeletionStore).DeleteTask(context.Background(), id)
	reader := st.(TaskExportReader)
	before, err := st.GetTask(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	times, err := st.(TaskTraceStore).ListTaskTraces(ctx, id, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := reader.ReadTaskExport(ctx, id, tenant, MaxTaskExportBytes)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Task.SequenceNo != task.SequenceNo || len(snapshot.Task.Trace) != 0 || !sameExportJSON(snapshot.Traces, times) {
		a, _ := json.Marshal(snapshot.Traces)
		b, _ := json.Marshal(times)
		t.Fatalf("snapshot lost physical/logical Trace: %s vs %s sequence=%d", a, b, snapshot.Task.SequenceNo)
	}
	data, err := EncodeTaskExport(ctx, snapshot, "fixture", id, tenant, MaxTaskExportBytes)
	if err != nil {
		t.Fatal(err)
	}
	var artifact TaskExportArtifact
	if err := json.Unmarshal(data, &artifact); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(artifact.Payload)
	if artifact.PayloadSHA256 != hex.EncodeToString(hash[:]) || artifact.PayloadBytes != len(artifact.Payload) || artifact.TraceCount != 2 || artifact.RestoreReady || !artifact.SourcePreserved || len(artifact.Excluded) == 0 {
		t.Fatal("invalid manifest/hash")
	}
	after, err := st.GetTask(ctx, id)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("export modified source")
	}
	if _, err := reader.ReadTaskExport(ctx, id, "other", MaxTaskExportBytes); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("foreign task exported", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := reader.ReadTaskExport(canceled, id, tenant, MaxTaskExportBytes); err == nil {
		t.Fatal("cancellation ignored")
	}
	snapshot.Traces[0].Sequence = 3
	if _, err := EncodeTaskExport(ctx, snapshot, "fixture", id, tenant, MaxTaskExportBytes); !errors.Is(err, ErrTaskExportUnavailable) {
		t.Fatal("gap accepted", err)
	}
	// A continuously changing task cannot mix metadata and Trace save versions.
	done := make(chan error, 1)
	go func() {
		for n := 1; n <= 30; n++ {
			copy := types.CloneTask(task)
			copy.Goal = strconv.Itoa(n)
			copy.StepCount = n
			for i := range copy.Trace {
				copy.Trace[i].Observation = copy.Goal
			}
			if err := st.SaveFullTask(ctx, copy); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	for n := 0; n < 30; n++ {
		got, err := reader.ReadTaskExport(ctx, id, tenant, MaxTaskExportBytes)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range got.Traces {
			if event.Trace.Observation != got.Task.Goal {
				t.Fatal("mixed save versions")
			}
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	task.Goal = strings.Repeat("size", 4096)
	if err := st.SaveFullTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.ReadTaskExport(ctx, id, tenant, 4096); !errors.Is(err, ErrTaskExportTooLarge) {
		t.Fatal("oversize accepted", err)
	}
}
func TestSQLiteExportReadTransactionSurvivesConcurrentSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "joint.db")
	st, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	writer, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	task := &types.Task{ID: "stable", TenantID: "tenant", Goal: "before", Status: types.StatusFailed, Trace: []types.StepTrace{{Observation: "before"}}}
	if err := st.SaveFullTask(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	got, err := readSQLTaskExport(t.Context(), st.db, task.ID, task.TenantID, MaxTaskExportBytes, false, func(ctx context.Context, q taskSQLReader, id string, trace bool) (*types.Task, error) {
		changed := types.CloneTask(task)
		changed.Goal = "after"
		changed.Trace[0].Observation = "after"
		if err := writer.SaveFullTask(ctx, changed); err != nil {
			t.Fatal(err)
		}
		return st.getTaskQuery(ctx, q, id, trace)
	})
	if err != nil || got.Task.Goal != "before" || got.Traces[0].Trace.Observation != "before" {
		t.Fatalf("mixed transaction: %+v %v", got, err)
	}
	after, err := st.GetTask(t.Context(), task.ID)
	if err != nil || after.Goal != "after" {
		t.Fatal("concurrent writer did not commit", err)
	}
}
func TestExternalRedisTaskExport(t *testing.T) {
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
	checkTaskExport(t, st)
	task := &types.Task{ID: "export-ro-" + uuid.NewString(), TenantID: "export-ro", Status: types.StatusFailed, Trace: []types.StepTrace{{Step: 1}}}
	if err := st.SaveFullTask(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	defer st.DeleteTask(context.Background(), task.ID)
	snapshotKeys := func() map[string]string {
		t.Helper()
		keys, err := st.client.Keys(t.Context(), "*").Result()
		if err != nil {
			t.Fatal(err)
		}
		result := map[string]string{}
		for _, key := range keys {
			raw, err := st.client.Dump(t.Context(), key).Result()
			if err != nil {
				t.Fatal(err)
			}
			result[key] = raw
		}
		return result
	}
	before := snapshotKeys()
	if _, err := st.ReadTaskExport(t.Context(), task.ID, task.TenantID, MaxTaskExportBytes); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, snapshotKeys()) {
		t.Fatal("export wrote Redis keys")
	}
	if err := st.client.RPush(t.Context(), st.taskTraceTimeKey(task.ID), "extra").Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReadTaskExport(t.Context(), task.ID, task.TenantID, MaxTaskExportBytes); !errors.Is(err, ErrTaskExportUnavailable) {
		t.Fatal("mismatching time index accepted", err)
	}
}
func TestExternalPostgresTaskExport(t *testing.T) {
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
	checkTaskExport(t, st)
}

func sameExportJSON(a, b []TaskTraceEvent) bool {
	normalize := func(events []TaskTraceEvent) []TaskTraceEvent {
		result := append([]TaskTraceEvent(nil), events...)
		for i := range result {
			result[i].Trace = types.CloneTask(&types.Task{Trace: []types.StepTrace{events[i].Trace}}).Trace[0]
		}
		return result
	}
	a = normalize(a)
	b = normalize(b)
	left, err := json.Marshal(a)
	if err != nil {
		return false
	}
	right, err := json.Marshal(b)
	return err == nil && string(left) == string(right)
}
