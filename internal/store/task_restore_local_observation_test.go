package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/types"
)

func localObservationStore(t *testing.T, backend string) Store {
	t.Helper()
	if backend == "memory" {
		return NewMemoryStore()
	}
	st, err := NewSQLiteStore(filepath.Join(t.TempDir(), "local.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}
func seedLocalObservation(t *testing.T, st Store, count int) TaskRestoreLocalObservationRequest {
	t.Helper()
	session := &types.Session{ID: "session", TenantID: "a", Title: "private-session", Status: types.SessionStatusActive, NextSequence: 9007199254740993}
	if err := st.(SessionStore).CreateSession(t.Context(), session); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < count; i++ {
		task := &types.Task{ID: fmt.Sprintf("task-%04d", i), TenantID: "a", SessionID: session.ID, SequenceNo: int64(i*2 + 1), Goal: "private-goal", Status: types.StatusPaused}
		if i == 0 {
			task.Trace = []types.StepTrace{{Step: 1, Action: "multiagent_workflow_checkpoint", Observation: "private-checkpoint"}}
		}
		if err := st.SaveFullTask(t.Context(), task); err != nil {
			t.Fatal(err)
		}
	}
	return TaskRestoreLocalObservationRequest{TaskID: "task-0000", TenantID: "a", MaxBytes: MaxTaskRestoreObservationBytes, MaxTasks: MaxTaskRestoreObservationTasks}
}
func seedLocalLease(t *testing.T, st Store, id, owner string, expiry time.Time) {
	t.Helper()
	switch s := st.(type) {
	case *MemoryStore:
		s.mu.Lock()
		s.leases[id] = memoryLease{owner: owner, expiresAt: expiry}
		s.mu.Unlock()
	case *SQLiteStore:
		if _, err := s.db.ExecContext(t.Context(), "INSERT INTO task_leases(task_id,owner,expires_at) VALUES(?,?,?) ON CONFLICT(task_id) DO UPDATE SET owner=excluded.owner,expires_at=excluded.expires_at", id, owner, expiry.UnixNano()); err != nil {
			t.Fatal(err)
		}
	}
}
func observeLocal(t *testing.T, st Store, q TaskRestoreLocalObservationRequest) *TaskRestoreLocalObservation {
	t.Helper()
	o, err := st.(TaskRestoreLocalObservationReader).ReadTaskRestoreLocalObservation(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestTaskRestoreLocalObservationCompleteReadOnly(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			st := localObservationStore(t, backend)
			q := seedLocalObservation(t, st, 501)
			seedLocalLease(t, st, "task-0000", "private-owner", time.Now().Add(time.Hour))
			seedLocalLease(t, st, "task-0001", "expired-owner", time.Now().Add(-time.Hour))
			beforeTask, _ := st.GetTask(t.Context(), q.TaskID)
			beforeSession, _ := st.(SessionStore).GetSession(t.Context(), "session", "a")
			var beforeDB, beforeWAL []byte
			var path string
			var changes int64
			var leases map[string]memoryLease
			if s, ok := st.(*SQLiteStore); ok {
				// Enforce read-only SQL at the connection level: a no-op lease UPDATE fails.
				if _, err := s.db.Exec("PRAGMA query_only=ON"); err != nil {
					t.Fatal(err)
				}
				if err := s.db.QueryRow("PRAGMA database_list").Scan(new(int), new(string), &path); err != nil {
					t.Fatal(err)
				}
				beforeDB, _ = os.ReadFile(path)
				beforeWAL, _ = os.ReadFile(path + "-wal")
				if err := s.db.QueryRow("SELECT total_changes()").Scan(&changes); err != nil {
					t.Fatal(err)
				}
			} else {
				s := st.(*MemoryStore)
				s.mu.RLock()
				leases = map[string]memoryLease{}
				for id, l := range s.leases {
					leases[id] = l
				}
				s.mu.RUnlock()
			}
			o := observeLocal(t, st, q)
			if len(o.Members) != 501 || o.HighWatermark != 9007199254740993 || o.Status != "unverified" || len(o.Blockers) != 5 || o.Members[0].Lease.State != "active" || o.Members[1].Lease.State != "expired" || o.Members[2].Lease.State != "absent" {
				t.Fatal("incomplete diagnostic")
			}
			snap, err := st.(TaskJointExportReader).ReadJointTaskExport(t.Context(), q.TaskID, "a", MaxTaskExportBytes)
			if err != nil {
				t.Fatal(err)
			}
			taskBytes, _ := json.Marshal(snap.Task)
			traceBytes, _ := json.Marshal(snap.Traces)
			sessionBytes, _ := json.Marshal(snap.Dependencies.Session)
			if o.Members[0].TaskSHA256 != restoreDigest(taskBytes) || o.Members[0].TraceSHA256 != restoreDigest(traceBytes) || o.SessionSHA256 != restoreDigest(sessionBytes) {
				t.Fatal("snapshot digest diverged from export")
			}
			reportBytes, _ := json.Marshal(o)
			for _, secret := range []string{"private-goal", "private-session", "private-owner", "private-checkpoint"} {
				if bytes.Contains(reportBytes, []byte(secret)) {
					t.Fatal("body leaked")
				}
			}
			// Returned slices are owned and cannot mutate the Store.
			o.Members[0].ID = "tampered"
			o.Blockers[0] = "tampered"
			next := observeLocal(t, st, q)
			if next.Members[0].ID != q.TaskID || next.Blockers[0] == "tampered" {
				t.Fatal("snapshot alias")
			}
			afterTask, _ := st.GetTask(t.Context(), q.TaskID)
			afterSession, _ := st.(SessionStore).GetSession(t.Context(), "session", "a")
			if !reflect.DeepEqual(beforeTask, afterTask) || !reflect.DeepEqual(beforeSession, afterSession) {
				t.Fatal("read changed task/session")
			}
			if s, ok := st.(*SQLiteStore); ok {
				afterDB, _ := os.ReadFile(path)
				afterWAL, _ := os.ReadFile(path + "-wal")
				var afterChanges int64
				if err := s.db.QueryRow("SELECT total_changes()").Scan(&afterChanges); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(beforeDB, afterDB) || !bytes.Equal(beforeWAL, afterWAL) || changes != afterChanges {
					t.Fatal("read wrote SQLite")
				}
			} else {
				s := st.(*MemoryStore)
				s.mu.RLock()
				defer s.mu.RUnlock()
				if !reflect.DeepEqual(leases, s.leases) {
					t.Fatal("read changed leases")
				}
			}
			// Local observations are deliberately not trusted authority adapters.
			if _, ok := st.(TaskRestoreSessionDependencyReader); ok {
				t.Fatal("local Store presented as authority")
			}
			if _, ok := st.(TaskRestoreExecutionObservationReader); ok {
				t.Fatal("local Store presented as authority")
			}
		})
	}
}

func TestTaskRestoreLocalObservationFailClosed(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			for _, scenario := range []string{"empty_tenant", "wrong_tenant", "missing", "task_budget", "byte_budget", "cancel", "duplicate_sequence", "foreign_member", "missing_session"} {
				t.Run(scenario, func(t *testing.T) {
					st := localObservationStore(t, backend)
					q := seedLocalObservation(t, st, 2)
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					switch scenario {
					case "empty_tenant":
						q.TenantID = ""
					case "wrong_tenant":
						q.TenantID = "b"
					case "missing":
						q.TaskID = "missing"
					case "task_budget":
						q.MaxTasks = 1
					case "byte_budget":
						q.MaxBytes = 4096
						task, _ := st.GetTask(t.Context(), q.TaskID)
						task.Goal = strings.Repeat("x", 4096)
						if err := st.SaveFullTask(t.Context(), task); err != nil {
							t.Fatal(err)
						}
					case "cancel":
						cancel()
					case "duplicate_sequence", "foreign_member":
						task, _ := st.GetTask(t.Context(), "task-0001")
						if scenario == "duplicate_sequence" {
							task.SequenceNo = 1
						} else {
							task.TenantID = "b"
						}
						if err := st.SaveFullTask(t.Context(), task); err != nil {
							t.Fatal(err)
						}
					case "missing_session":
						if s, ok := st.(*MemoryStore); ok {
							s.mu.Lock()
							delete(s.sessions, "session")
							s.mu.Unlock()
						} else {
							if _, err := st.(*SQLiteStore).db.Exec("DELETE FROM sessions WHERE id='session'"); err != nil {
								t.Fatal(err)
							}
						}
					}
					o, err := st.(TaskRestoreLocalObservationReader).ReadTaskRestoreLocalObservation(ctx, q)
					if err == nil || o != nil {
						t.Fatal("unsafe observation returned", scenario)
					}
					if scenario == "cancel" && !errors.Is(err, context.Canceled) {
						t.Fatal("lost cancellation", err)
					}
					if (scenario == "task_budget" || scenario == "byte_budget") && !errors.Is(err, ErrTaskExportTooLarge) {
						t.Fatal("lost budget error", err)
					}
				})
			}
		})
	}
}

func TestTaskRestoreLocalObservationChangesAreContentOnly(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			st := localObservationStore(t, backend)
			q := seedLocalObservation(t, st, 1)
			before := observeLocal(t, st, q)
			// Update one checkpoint without changing the physical event count.
			task, _ := st.GetTask(t.Context(), q.TaskID)
			task.Trace[0].Observation = "changed-checkpoint"
			if err := st.SaveFullTask(t.Context(), task); err != nil {
				t.Fatal(err)
			}
			after := observeLocal(t, st, q)
			if before.ContentSHA256 == after.ContentSHA256 || before.Members[0].TraceSHA256 == after.Members[0].TraceSHA256 || before.Members[0].TraceCount != after.Members[0].TraceCount {
				t.Fatal("checkpoint mutation lost")
			}
			seedLocalLease(t, st, q.TaskID, "owner", time.Now().Add(time.Hour))
			withLease := observeLocal(t, st, q)
			if withLease.ContentSHA256 == after.ContentSHA256 {
				t.Fatal("lease mutation lost")
			}
			// No fake monotonic token is invented; even a current snapshot stays unverified.
			if withLease.Status != "unverified" {
				t.Fatal("content became authority")
			}
		})
	}
}

func TestSQLiteTaskRestoreLocalObservationSingleSnapshot(t *testing.T) {
	st := localObservationStore(t, "sqlite").(*SQLiteStore)
	q := seedLocalObservation(t, st, 2)
	var path string
	if err := st.db.QueryRow("PRAGMA database_list").Scan(new(int), new(string), &path); err != nil {
		t.Fatal(err)
	}
	writer, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	tx, err := st.db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var old int64
	if err := tx.QueryRowContext(t.Context(), "SELECT next_sequence FROM sessions WHERE id='session'").Scan(&old); err != nil {
		t.Fatal(err)
	}
	// Commit an atomic new member/lease/high-watermark while the reader is pinned.
	writeTx, err := writer.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writeTx.Rollback()
	if _, err = writeTx.Exec("UPDATE sessions SET next_sequence=next_sequence+1 WHERE id='session'"); err != nil {
		t.Fatal(err)
	}
	if _, err = writeTx.Exec("UPDATE tasks SET goal='new-goal' WHERE id='task-0000'"); err != nil {
		t.Fatal(err)
	}
	if _, err = writeTx.Exec(`INSERT INTO tasks(id,tenant_id,session_id,sequence_no,created_at,updated_at,goal,status,max_steps,step_count,workspace,hypothesis,unresolved_json,tool_budget,final_answer)
		VALUES('task-0002','a','session',5,?,?, 'new-member','created',0,0,'','','[]',0,'')`, time.Now().UTC(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err = writeTx.Exec("INSERT INTO task_leases(task_id,owner,expires_at) VALUES('task-0000','new-owner',?)", time.Now().Add(time.Hour).UnixNano()); err != nil {
		t.Fatal(err)
	}
	if err = writeTx.Commit(); err != nil {
		t.Fatal(err)
	}
	observed, err := readSQLiteLocalObservation(t.Context(), tx, q, st.getTaskQuery)
	if err != nil {
		t.Fatal(err)
	}
	if observed.HighWatermark != old || observed.Members[0].Lease.Present || len(observed.Members) != 2 {
		t.Fatal("mixed SQL snapshots")
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	next := observeLocal(t, st, q)
	if next.HighWatermark != old+1 || !next.Members[0].Lease.Present || next.Members[0].TaskSHA256 == observed.Members[0].TaskSHA256 || len(next.Members) != 3 {
		t.Fatal("new snapshot not observed")
	}
}

func TestTaskRestoreLocalObservationNoSession(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			st := localObservationStore(t, backend)
			task := &types.Task{ID: "single", TenantID: "a", Status: types.StatusCreated}
			if err := st.SaveFullTask(t.Context(), task); err != nil {
				t.Fatal(err)
			}
			q := TaskRestoreLocalObservationRequest{TaskID: task.ID, TenantID: "a", MaxBytes: MaxTaskRestoreObservationBytes, MaxTasks: 1}
			o := observeLocal(t, st, q)
			if len(o.Members) != 1 || o.SessionID != "" || o.SessionSHA256 != "" || o.HighWatermark != 0 || o.Status != "unverified" {
				t.Fatal(o)
			}
		})
	}
}

func TestMemoryTaskRestoreLocalObservationCancellationDuringContention(t *testing.T) {
	st := NewMemoryStore()
	q := seedLocalObservation(t, st, 1)
	st.mu.Lock()
	defer st.mu.Unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	o, err := st.ReadTaskRestoreLocalObservation(ctx, q)
	if o != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("contention ignored cancellation", err)
	}
}

func TestMemoryTaskRestoreLocalObservationAtomicRead(t *testing.T) {
	st := NewMemoryStore()
	q := seedLocalObservation(t, st, 1)
	setState := func(high int64, goal string) {
		st.sessions["session"].NextSequence = high
		st.tasks[q.TaskID].Goal = goal
	}
	st.mu.Lock()
	setState(1, "state-a")
	st.mu.Unlock()
	a := observeLocal(t, st, q)
	st.mu.Lock()
	setState(3, "state-b")
	st.mu.Unlock()
	b := observeLocal(t, st, q)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 100 {
			st.mu.Lock()
			setState(1, "state-a")
			st.mu.Unlock()
			st.mu.Lock()
			setState(3, "state-b")
			st.mu.Unlock()
		}
	}()
	for range 100 {
		o := observeLocal(t, st, q)
		if o.ContentSHA256 != a.ContentSHA256 && o.ContentSHA256 != b.ContentSHA256 {
			t.Fatal("mixed lock snapshots")
		}
	}
	<-done
}
