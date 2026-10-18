package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wuxujun/ai-agent/internal/types"
)

func TestSQLiteAuditQueryFiltersProjectsAndPaginates(t *testing.T) {
	st, err := NewSQLiteStore(filepath.Join(t.TempDir(), "audits.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	report := func(confidence string, publish bool, stages ...types.AnswerAuditStage) *types.AnswerAuditReport {
		return &types.AnswerAuditReport{FinalConfidence: confidence, Publishable: publish, Enforcement: "observe", Stages: stages}
	}
	tasks := []*types.Task{
		{ID: "a", TenantID: "tenant", Status: types.StatusCompleted, AnswerAudit: report("high", true, types.AnswerAuditStage{Name: "freshness", Status: "passed"}), FinalAnswer: "answer"},
		{ID: "b", TenantID: "tenant", Status: types.StatusFailed, AnswerAudit: report("low", false, types.AnswerAuditStage{Name: "freshness", Status: "warned"}, types.AnswerAuditStage{Name: "numeric", Status: "passed"}), FinalAnswer: "answer"},
		{ID: "c", TenantID: "tenant", Status: types.StatusCompleted, FinalAnswer: "answer"},
		{ID: "d", TenantID: "other", Status: types.StatusCompleted, AnswerAudit: report("high", true), FinalAnswer: "answer"},
		{ID: "e", TenantID: "tenant", Status: types.StatusRunning, AnswerAudit: report("high", true), FinalAnswer: "draft"},
		{ID: "f", TenantID: "tenant", Status: types.StatusPartial, AnswerAudit: report("high", true), FinalAnswer: " \n\t\u3000"},
	}
	for _, task := range tasks {
		task.Trace = []types.StepTrace{{Action: "large-private-trace"}}
		if err := st.SaveFullTask(t.Context(), task); err != nil {
			t.Fatal(err)
		}
	}
	// Indexing must be quiescent before deliberately corrupting fields that an
	// audit projection must not read or decode.
	st.memoryIndexGate.close()
	if _, err := st.db.Exec("UPDATE tasks SET unresolved_json='invalid JSON', memories_json='invalid JSON', goal='private' "); err != nil {
		t.Fatal(err)
	}
	yes, no := true, false
	cases := []struct {
		name   string
		filter AuditFilter
		ids    []string
	}{
		{"all", AuditFilter{TenantID: "tenant"}, []string{"a", "b", "e", "f"}},
		{"confidence", AuditFilter{TenantID: "tenant", Confidence: "low"}, []string{"b"}},
		{"unpublishable", AuditFilter{TenantID: "tenant", Publishable: &no}, []string{"b"}},
		{"publishable", AuditFilter{TenantID: "tenant", Publishable: &yes, EligibleOnly: true}, []string{"a"}},
		{"stage same row", AuditFilter{TenantID: "tenant", Stage: "freshness", StageState: "passed"}, []string{"a"}},
		{"stage status", AuditFilter{TenantID: "tenant", StageState: "warned"}, []string{"b"}},
		{"enforcement", AuditFilter{TenantID: "tenant", Enforcement: "strict"}, []string{}},
		{"page", AuditFilter{TenantID: "tenant", Limit: 1, Offset: 1}, []string{"b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := st.ListAuditTasks(t.Context(), tc.filter)
			if err != nil {
				t.Fatal(err)
			}
			ids := []string{}
			for _, task := range got {
				ids = append(ids, task.ID)
				if len(task.Trace) != 0 || task.Goal != "" || task.FinalAnswer != "" {
					t.Fatal("audit loaded full task")
				}
			}
			if !reflect.DeepEqual(ids, tc.ids) {
				t.Fatalf("ids=%v want=%v", ids, tc.ids)
			}
		})
	}
	n, err := st.CountAuditEligibleTasks(t.Context(), "tenant", "")
	if err != nil || n != 3 {
		t.Fatalf("eligible=%d err=%v", n, err)
	}
}

func TestExternalPostgresAuditProjectionAndDeletion(t *testing.T) {
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
	id := "audit-query-" + uuid.NewString()
	tenant := id + "-tenant"
	// Keep the test independent of any embedding provider.
	st.memoryIndexGate.close()
	task := &types.Task{ID: id, TenantID: tenant, Status: types.StatusCompleted, FinalAnswer: "answer", AnswerAudit: &types.AnswerAuditReport{Publishable: true, FinalConfidence: "high", Enforcement: "observe", Stages: []types.AnswerAuditStage{{Name: "freshness", Status: "passed"}}}}
	if err := st.SaveFullTask(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = st.DeleteTask(context.Background(), id) })
	yes := true
	reports, err := st.ListAuditTasks(t.Context(), AuditFilter{TenantID: tenant, Stage: "freshness", StageState: "passed", Publishable: &yes, EligibleOnly: true})
	if err != nil || len(reports) != 1 || reports[0].ID != id || reports[0].FinalAnswer != "" {
		t.Fatalf("reports=%+v err=%v", reports, err)
	}
	if n, err := st.CountAuditEligibleTasks(t.Context(), tenant, ""); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	record := &types.DurableApproval{ID: id, TaskID: id, TenantID: tenant, Status: types.ApprovalPending, ActionPayload: []byte("fixture"), Request: types.ApprovalRequest{ID: id, TaskID: id, Action: "write_file", RiskLevel: types.RiskLevelHigh}}
	if err := st.CreateApproval(t.Context(), record); err != nil {
		t.Fatal(err)
	}
	mem := &types.Memory{ID: id, TaskID: id, TenantID: tenant, SessionID: id, Timestamp: time.Now()}
	indexed := context.WithValue(t.Context(), indexedMemoryKey{}, true)
	if err := st.SaveMemory(indexed, mem); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.DeleteTask(t.Context(), id); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if _, err := st.GetApproval(t.Context(), id, tenant); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("orphan approval", err)
	}
	if err := st.SaveMemory(indexed, mem); err != nil {
		t.Fatal(err)
	}
	memories, err := st.ListMemories(t.Context(), ListMemoryFilter{TenantID: tenant, SessionID: id})
	if err != nil || len(memories) != 0 {
		t.Fatalf("orphan memories=%d err=%v", len(memories), err)
	}
}
