package store

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/types"
)

func TestRetentionMetadataReadOnlyAndScoped(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			var st Store = NewMemoryStore()
			if backend == "sqlite" {
				var err error
				st, err = NewSQLiteStore(filepath.Join(t.TempDir(), "retention.db"))
				if err != nil {
					t.Fatal(err)
				}
			}
			defer st.Close()
			reader := st.(TaskRetentionReader)
			now := time.Now().UTC().Truncate(time.Microsecond)
			for _, task := range []*types.Task{{ID: "same-a", TenantID: "a", Goal: "private", Status: types.StatusFailed, CreatedAt: now, Trace: []types.StepTrace{{Observation: "private"}}}, {ID: "same-z", TenantID: "a", Status: types.StatusRunning, CreatedAt: now}, {ID: "foreign", TenantID: "b", Status: types.StatusFailed, CreatedAt: now}} {
				if err := st.(TaskCreationStore).CreateTask(t.Context(), task); err != nil {
					t.Fatal(err)
				}
			}
			if ok, err := st.AcquireTaskLease(t.Context(), "same-a", "worker", time.Minute); !ok || err != nil {
				t.Fatal(err)
			}
			approval := &types.DurableApproval{ID: "approval-a", TaskID: "same-a", TenantID: "a", Status: types.ApprovalPending, Version: 1, Request: types.ApprovalRequest{ID: "approval-a", TaskID: "same-a", Action: "write", RiskLevel: types.RiskLevelHigh}, ActionPayload: []byte("fixture-ciphertext")}
			if err := st.(DurableApprovalStore).CreateApproval(t.Context(), approval); err != nil {
				t.Fatal(err)
			}
			before, err := st.GetTask(t.Context(), "same-a")
			if err != nil {
				t.Fatal(err)
			}
			first, err := reader.ListTaskRetentionObservations(t.Context(), TaskSummaryFilter{TenantID: "a", Limit: 1})
			if err != nil || len(first) != 1 || first[0].ID != "same-z" {
				t.Fatalf("first page: %+v %v", first, err)
			}
			second, err := reader.ListTaskRetentionObservations(t.Context(), TaskSummaryFilter{TenantID: "a", BeforeCreatedAt: first[0].CreatedAt, BeforeID: first[0].ID, Limit: 1})
			if err != nil || len(second) != 1 || second[0].ID != "same-a" || second[0].TraceCount == nil || *second[0].TraceCount != 1 || second[0].LeaseActive == nil || !*second[0].LeaseActive || second[0].ApprovalsUnresolved == nil || !*second[0].ApprovalsUnresolved {
				t.Fatalf("second page: %+v %v", second, err)
			}
			after, err := st.GetTask(t.Context(), "same-a")
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("assessment modified Task")
			}
			if ok, err := st.AcquireTaskLease(t.Context(), "same-a", "other", time.Minute); ok || err != nil {
				t.Fatal("assessment altered lease")
			}
			canceled, cancel := context.WithCancel(t.Context())
			cancel()
			if _, err := reader.ListTaskRetentionObservations(canceled, TaskSummaryFilter{TenantID: "a", Limit: 1}); err == nil {
				t.Fatal("cancellation ignored")
			}
			if _, err := reader.ListTaskRetentionObservations(t.Context(), TaskSummaryFilter{Limit: 1}); err == nil {
				t.Fatal("unscoped scan accepted")
			}
		})
	}
}
