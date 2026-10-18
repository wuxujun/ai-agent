package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/types"
)

func TestStoreCloseDrainsIndexingAndClosesAdmission(t *testing.T) {
	for _, kind := range []string{"memory", "sqlite", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			var st Store
			var gate *memoryIndexGate
			switch kind {
			case "memory":
				s := NewMemoryStore()
				st = s
				gate = &s.memoryIndexGate
			case "sqlite":
				s, err := NewSQLiteStore(filepath.Join(t.TempDir(), "close.db"))
				if err != nil {
					t.Fatal(err)
				}
				st = s
				gate = &s.memoryIndexGate
			case "postgres":
				db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "close.db"))
				if err != nil {
					t.Fatal(err)
				}
				s := &PostgresStore{db: db}
				st = s
				gate = &s.memoryIndexGate
			}
			if !gate.tryStart("pending") {
				t.Fatal("job not admitted")
			}
			done := make(chan error, 1)
			go func() { done <- st.Close() }()
			select {
			case err := <-done:
				t.Fatalf("close skipped pending index: %v", err)
			case <-time.After(20 * time.Millisecond):
			}
			gate.done("pending")
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("close did not drain")
			}
			if gate.tryStart("late") {
				t.Fatal("index admitted after close")
			}
		})
	}
}

func TestTaskDeletionCleansApprovalsAndMemories(t *testing.T) {
	for _, kind := range []string{"memory", "sqlite"} {
		for _, all := range []bool{false, true} {
			t.Run(kind+"/"+map[bool]string{false: "one", true: "all"}[all], func(t *testing.T) {
				var st Store
				if kind == "memory" {
					st = NewMemoryStore()
				} else {
					s, err := NewSQLiteStore(filepath.Join(t.TempDir(), "delete.db"))
					if err != nil {
						t.Fatal(err)
					}
					st = s
				}
				defer st.Close()
				ctx := t.Context()
				for _, id := range []string{"remove", "keep"} {
					if err := st.SaveFullTask(ctx, &types.Task{ID: id, TenantID: "tenant", Status: types.StatusCreated}); err != nil {
						t.Fatal(err)
					}
					if err := st.(DurableApprovalStore).CreateApproval(ctx, &types.DurableApproval{ID: id, TaskID: id, TenantID: "tenant", Status: types.ApprovalPending, ActionPayload: []byte("fixture"), Request: types.ApprovalRequest{ID: id, TaskID: id, Action: "write_file", RiskLevel: types.RiskLevelHigh}}); err != nil {
						t.Fatal(err)
					}
					if err := st.SaveMemory(ctx, &types.Memory{ID: "content-" + id, TaskID: id, TenantID: "tenant", SessionID: "session", Timestamp: time.Now()}); err != nil {
						t.Fatal(err)
					}
				}
				if err := st.SaveMemory(ctx, &types.Memory{ID: "independent", TenantID: "tenant", Timestamp: time.Now()}); err != nil {
					t.Fatal(err)
				}
				ds := st.(TaskDeletionStore)
				if all {
					if n, err := ds.DeleteAllTasks(ctx); err != nil || n != 2 {
						t.Fatal(n, err)
					}
				} else {
					if ok, err := ds.DeleteTask(ctx, "remove"); err != nil || !ok {
						t.Fatal(ok, err)
					}
				}
				if _, err := st.(DurableApprovalStore).GetApproval(ctx, "remove", "tenant"); !errors.Is(err, sql.ErrNoRows) {
					t.Fatalf("orphan approval: %v", err)
				}
				if !all {
					if _, err := st.(DurableApprovalStore).GetApproval(ctx, "keep", "tenant"); err != nil {
						t.Fatal("other task approval lost", err)
					}
				}
				memories, err := st.(MemoryManagementStore).ListMemories(ctx, ListMemoryFilter{TenantID: "tenant", SessionID: "session"})
				if err != nil {
					t.Fatal(err)
				}
				want := 1
				if all {
					want = 0
				}
				if len(memories) != want {
					t.Fatalf("session memories=%d want=%d", len(memories), want)
				}
				if s, ok := st.(*SQLiteStore); ok {
					lateCtx := context.WithValue(ctx, indexedMemoryKey{}, true)
					if err := s.SaveMemory(lateCtx, &types.Memory{ID: "late", TaskID: "remove", TenantID: "tenant", SessionID: "session", Timestamp: time.Now()}); err != nil {
						t.Fatal(err)
					}
					var n int
					if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM memories WHERE id = 'late'").Scan(&n); err != nil || n != 0 {
						t.Fatal("deleted task memory resurrected", n, err)
					}
				}
			})
		}
	}
}
func TestSQLiteForeignKeysRemainEnabledOnReopenedConnection(t *testing.T) {
	st, err := NewSQLiteStore(filepath.Join(t.TempDir(), "foreign.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.db.SetMaxIdleConns(0)
	for i := 0; i < 2; i++ {
		var enabled int
		if err := st.db.QueryRow("PRAGMA foreign_keys").Scan(&enabled); err != nil || enabled != 1 {
			t.Fatal(enabled, err)
		}
	}
}
