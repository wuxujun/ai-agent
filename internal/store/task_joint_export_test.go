package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wuxujun/ai-agent/internal/types"
)

func TestTaskJointExportContract(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			var st Store = NewMemoryStore()
			if backend == "sqlite" {
				var err error
				st, err = NewSQLiteStore(filepath.Join(t.TempDir(), "joint.db"))
				if err != nil {
					t.Fatal(err)
				}
			}
			defer st.Close()
			checkTaskJointExport(t, st)
		})
	}
}

func checkTaskJointExport(t *testing.T, st Store) {
	t.Helper()
	ctx := t.Context()
	id := "joint-" + uuid.NewString()
	tenant := id + "-tenant"
	sessions := st.(SessionStore)
	session := &types.Session{ID: id + "-session", TenantID: tenant, Title: "private-session", Status: types.SessionStatusActive}
	if err := sessions.CreateSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := sessions.NextSessionTaskSequence(ctx, session.ID, tenant); err != nil {
			t.Fatal(err)
		}
	}
	task := &types.Task{ID: id, TenantID: tenant, SessionID: session.ID, SequenceNo: 1, Status: types.StatusFailed, Trace: []types.StepTrace{{Step: 1}}}
	if err := st.SaveFullTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	defer st.(TaskDeletionStore).DeleteTask(context.Background(), id)
	approvals := st.(DurableApprovalStore)
	for _, suffix := range []string{"z", "a"} {
		a := &types.DurableApproval{ID: id + suffix, TaskID: id, TenantID: tenant, Version: 1, Status: types.ApprovalPending, Request: types.ApprovalRequest{ID: id + suffix, TaskID: id, Action: "write", RiskLevel: types.RiskLevelHigh, Parameters: map[string]any{"integer": int64(9007199254740993), "nested": []any{map[string]any{"preview": "private-preview"}}}}, ActionPayload: []byte("fixture-encrypted-action")}
		if err := approvals.CreateApproval(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	if ok, err := approvals.TransitionApproval(ctx, id+"a", tenant, 1, types.ApprovalPending, types.ApprovalApproved, []byte("fixture-encrypted-resolution")); !ok || err != nil {
		t.Fatal(err)
	}
	mem := &types.Memory{ID: id + "memory", TaskID: id, TenantID: tenant, SessionID: session.ID, Timestamp: time.Now().UTC(), Goal: "private-memory", Embedding: []float32{.25, .75}}
	if err := st.SaveMemory(ctx, mem); err != nil {
		t.Fatal(err)
	}
	unrelated := &types.Memory{ID: id + "unrelated", TaskID: "unrelated", TenantID: tenant, Timestamp: time.Now().UTC(), Goal: "unrelated-body"}
	if err := st.SaveMemory(ctx, unrelated); err != nil {
		t.Fatal(err)
	}
	defer st.(MemoryManagementStore).DeleteMemory(context.Background(), unrelated.ID, tenant)
	beforeTask, _ := st.GetTask(ctx, id)
	beforeApproval, _ := approvals.GetApproval(ctx, id+"a", tenant)
	beforeSession, _ := sessions.GetSession(ctx, session.ID, tenant)
	reader := st.(TaskJointExportReader)
	var beforeKeys map[string]string
	if r, ok := st.(*RedisStore); ok {
		beforeKeys = jointRedisDump(t, r, id)
	}
	snapshot, err := reader.ReadJointTaskExport(ctx, id, tenant, MaxTaskExportBytes)
	if err != nil {
		t.Fatal(err)
	}
	if r, ok := st.(*RedisStore); ok {
		afterKeys := jointRedisDump(t, r, id)
		if !reflect.DeepEqual(beforeKeys, afterKeys) {
			for key, value := range beforeKeys {
				if afterKeys[key] != value {
					t.Logf("changed or removed fixture key: %s", key)
				}
			}
			for key := range afterKeys {
				if _, existed := beforeKeys[key]; !existed {
					t.Logf("added fixture key: %s", key)
				}
			}
			t.Fatal("joint export wrote Redis data")
		}
	}
	d := snapshot.Dependencies
	if d == nil || len(d.Approvals) != 2 || d.Approvals[0].ID != id+"a" || d.Approvals[0].Version != 2 || string(d.Approvals[0].ResolutionPayload) != "fixture-encrypted-resolution" || len(d.Memories) != 1 || !reflect.DeepEqual(d.Memories[0].Embedding, mem.Embedding) || d.Session == nil || d.Session.NextSequence != 2 {
		t.Fatalf("missing joint data: %+v", d)
	}
	data, err := EncodeTaskExport(ctx, snapshot, "fixture", id, tenant, MaxTaskExportBytes)
	if err != nil {
		t.Fatal(err)
	}
	var artifact TaskExportArtifact
	if err := json.Unmarshal(data, &artifact); err != nil {
		t.Fatal(err)
	}
	if artifact.Schema != "task-joint-export-v2" || artifact.RestoreReady || !strings.Contains(string(artifact.Payload), "9007199254740993") || strings.Contains(string(artifact.Payload), "unrelated-body") || !strings.Contains(string(artifact.Payload), `"next_sequence":"2"`) {
		t.Fatal("wrong joint format or number precision")
	}
	if artifact.ApprovalCount == nil || *artifact.ApprovalCount != 2 || artifact.MemoryCount == nil || *artifact.MemoryCount != 1 {
		t.Fatal("joint manifest counts")
	}
	afterTask, _ := st.GetTask(ctx, id)
	afterApproval, _ := approvals.GetApproval(ctx, id+"a", tenant)
	afterSession, _ := sessions.GetSession(ctx, session.ID, tenant)
	if !reflect.DeepEqual(beforeTask, afterTask) || !reflect.DeepEqual(beforeApproval, afterApproval) || !reflect.DeepEqual(beforeSession, afterSession) {
		t.Fatal("export changed source")
	}
	// Changes to a returned snapshot cannot mutate stored dependency values.
	d.Memories[0].Embedding[0] = 99
	d.Session.Title = "changed"
	d.Approvals[0].ActionPayload[0] = 'X'
	d.Approvals[0].Request.Parameters["nested"].([]any)[0].(map[string]any)["preview"] = "changed"
	again, err := reader.ReadJointTaskExport(ctx, id, tenant, MaxTaskExportBytes)
	if err != nil {
		t.Fatal(err)
	}
	if again.Dependencies.Memories[0].Embedding[0] != .25 || again.Dependencies.Session.Title != "private-session" || string(again.Dependencies.Approvals[0].ActionPayload) != "fixture-encrypted-action" || again.Dependencies.Approvals[0].Request.Parameters["nested"].([]any)[0].(map[string]any)["preview"] != "private-preview" {
		t.Fatal("snapshot aliases dependency storage")
	}
	if ok, err := approvals.TransitionApproval(ctx, id+"a", tenant, 2, types.ApprovalApproved, types.ApprovalConsumed, nil); !ok || err != nil {
		t.Fatal(err)
	}
	changed, err := reader.ReadJointTaskExport(ctx, id, tenant, MaxTaskExportBytes)
	if err != nil {
		t.Fatal(err)
	}
	newData, err := EncodeTaskExport(ctx, changed, "fixture", id, tenant, MaxTaskExportBytes)
	if err != nil {
		t.Fatal(err)
	}
	var newArtifact TaskExportArtifact
	json.Unmarshal(newData, &newArtifact)
	if newArtifact.PayloadSHA256 == artifact.PayloadSHA256 || changed.Task.MaintenanceVersion != beforeTask.MaintenanceVersion {
		t.Fatal("independent dependency mutation absent from hash")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := reader.ReadJointTaskExport(canceled, id, tenant, MaxTaskExportBytes); err == nil {
		t.Fatal("canceled joint read accepted")
	}
	mem.Goal = strings.Repeat("large", 4096)
	if err := st.SaveMemory(ctx, mem); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.ReadJointTaskExport(ctx, id, tenant, 4096); !errors.Is(err, ErrTaskExportTooLarge) {
		t.Fatal("dependency budget ignored", err)
	}
}

func TestSQLiteJointExportUsesOneDependencySnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stable.db")
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
	session := &types.Session{ID: "session", TenantID: "tenant", Title: "before", Status: types.SessionStatusActive, NextSequence: 7}
	if err := st.CreateSession(t.Context(), session); err != nil {
		t.Fatal(err)
	}
	task := &types.Task{ID: "task", TenantID: "tenant", SessionID: session.ID, SequenceNo: 7, Goal: "before", Status: types.StatusFailed}
	if err := st.SaveFullTask(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	mem := &types.Memory{ID: "memory", TaskID: task.ID, TenantID: "tenant", Goal: "before", Timestamp: time.Now().UTC()}
	if err := st.SaveMemory(t.Context(), mem); err != nil {
		t.Fatal(err)
	}
	approval := &types.DurableApproval{ID: "approval", TaskID: task.ID, TenantID: task.TenantID, Status: types.ApprovalPending, Version: 1, ActionPayload: []byte("fixture-ciphertext"), Request: types.ApprovalRequest{ID: "approval", TaskID: task.ID, Action: "write", RiskLevel: types.RiskLevelHigh}}
	if err := st.CreateApproval(t.Context(), approval); err != nil {
		t.Fatal(err)
	}
	got, err := readSQLTaskExport(t.Context(), st.db, task.ID, task.TenantID, MaxTaskExportBytes, false, func(ctx context.Context, q taskSQLReader, id string, trace bool) (*types.Task, error) {
		if ok, err := writer.TransitionApproval(ctx, approval.ID, task.TenantID, 1, types.ApprovalPending, types.ApprovalApproved, []byte("new-ciphertext")); !ok || err != nil {
			t.Fatal(err)
		}
		mem.Goal = "after"
		if err := writer.SaveMemory(ctx, mem); err != nil {
			t.Fatal(err)
		}
		session.Title = "after"
		if err := writer.UpdateSession(ctx, session); err != nil {
			t.Fatal(err)
		}
		task.Goal = "after"
		if err := writer.SaveFullTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		return st.getTaskQuery(ctx, q, id, trace)
	}, true)
	if err != nil || got.Task.Goal != "before" || got.Dependencies.Memories[0].Goal != "before" || got.Dependencies.Session.Title != "before" {
		t.Fatalf("mixed dependencies: %+v %v", got, err)
	}
	if got.Dependencies.Approvals[0].Version != 1 || got.Dependencies.Approvals[0].Status != types.ApprovalPending {
		t.Fatal("mixed approval version")
	}
}
func TestExternalRedisTaskJointExport(t *testing.T) {
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
	checkTaskJointExport(t, st)
}
func TestExternalPostgresTaskJointExport(t *testing.T) {
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
	checkTaskJointExport(t, st)
}

func jointRedisDump(t *testing.T, r *RedisStore, fixtureID string) map[string]string {
	t.Helper()
	// EVAL_RO takes an atomic observation. Exclude unrelated expiring leases:
	// their natural expiry between observations is not an export write. Keep all
	// persistent/global index keys and every key belonging to this fixture.
	values, err := r.client.EvalRO(t.Context(), `
 local result={}
 for _,key in ipairs(redis.call('KEYS','*')) do
  local ttl=redis.call('PTTL',key)
  if ttl==-1 or (ttl>=0 and string.find(key,ARGV[1],1,true)) then
   local value=redis.call('DUMP',key)
   if value then result[#result+1]=key;result[#result+1]=value end
  end
 end
 return result`, nil, fixtureID).Slice()
	if err != nil {
		t.Fatal(err)
	}
	result := map[string]string{}
	for i := 0; i < len(values); i += 2 {
		result[values[i].(string)] = values[i+1].(string)
	}
	return result
}

func TestTaskJointExportRejectsInvalidDependencies(t *testing.T) {
	task := &types.Task{ID: "task", TenantID: "tenant", SessionID: "session", SequenceNo: 7}
	valid := func() *TaskExportDependencies {
		return &TaskExportDependencies{Session: &TaskExportSession{Session: &types.Session{ID: "session", TenantID: "tenant"}, NextSequence: 7}}
	}
	for _, scenario := range []string{"missing-session", "foreign-session", "low-watermark", "foreign-memory", "foreign-approval", "duplicate-memory", "too-many"} {
		t.Run(scenario, func(t *testing.T) {
			d := valid()
			want := ErrTaskExportUnavailable
			switch scenario {
			case "missing-session":
				d.Session = nil
			case "foreign-session":
				d.Session.TenantID = "other"
			case "low-watermark":
				d.Session.NextSequence = 6
			case "foreign-memory":
				d.Memories = []*types.Memory{{ID: "memory", TaskID: "task", TenantID: "other"}}
			case "foreign-approval":
				d.Approvals = []*types.DurableApproval{{ID: "approval", TaskID: "task", TenantID: "other", Version: 1}}
			case "duplicate-memory":
				m := &types.Memory{ID: "memory", TaskID: "task", TenantID: "tenant"}
				d.Memories = []*types.Memory{m, m}
			case "too-many":
				d.Memories = make([]*types.Memory, MaxTaskExportDependencies+1)
				want = ErrTaskExportTooLarge
			}
			if err := validateExportDependencies(task, d); !errors.Is(err, want) {
				t.Fatalf("%s: %v", scenario, err)
			}
		})
	}
}
