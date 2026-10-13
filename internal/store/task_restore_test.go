package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/types"
)

func restoreFixture(t *testing.T) []byte {
	t.Helper()
	at := time.Date(2025, 1, 2, 3, 4, 5, 123456789, time.UTC)
	snap := &TaskExportSnapshot{
		Task:   &types.Task{ID: "restore-task", TenantID: "a", SessionID: "s", SequenceNo: 1, Status: types.StatusFailed, CreatedAt: at, UpdatedAt: at, Goal: "private-goal"},
		Traces: []TaskTraceEvent{{Sequence: 1, RecordedAt: &at, Trace: types.StepTrace{Step: 3, OccurredAt: &at, Observation: "private-trace"}}, {Sequence: 2, Trace: types.StepTrace{Step: 3}}},
		Dependencies: &TaskExportDependencies{
			Approvals: []*types.DurableApproval{{ID: "approval", TaskID: "restore-task", TenantID: "a", Version: 7, Status: types.ApprovalApproved, Owner: "original-owner", LeaseExpiresAt: at, CreatedAt: at, UpdatedAt: at, ResolvedAt: at, Request: types.ApprovalRequest{ID: "approval", TaskID: "restore-task", Action: "write", RiskLevel: types.RiskLevelHigh, Parameters: map[string]any{"large": json.Number("9007199254740993"), "nested": []any{map[string]any{"x": "private-preview"}}}}, ActionPayload: []byte{0, 255, 7}, ResolutionPayload: []byte{1, 254, 8}}},
			Memories:  []*types.Memory{{ID: "memory", TaskID: "restore-task", TenantID: "a", SessionID: "s", Timestamp: at, Embedding: []float32{.25, .75}}},
			Session:   &TaskExportSession{Session: &types.Session{ID: "s", TenantID: "a", Status: types.SessionStatusActive, CreatedAt: at, UpdatedAt: at}, NextSequence: 9007199254740993},
		}}
	data, err := EncodeTaskExport(t.Context(), snap, "fixture", snap.Task.ID, "a", MaxTaskExportBytes)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestTaskRestoreRoundTripAndConflicts(t *testing.T) {
	data := restoreFixture(t)
	before := bytes.Clone(data)
	sandbox, err := NewTaskRestoreSandbox(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer sandbox.Close()
	if err := sandbox.ImportJoint(t.Context(), data, "a", MaxTaskExportBytes); err != nil {
		t.Fatal(err)
	}
	report, err := sandbox.Verify(t.Context(), "restore-task", "a")
	if err != nil {
		t.Fatal(err)
	}
	if !report.Verified || report.RestoreReady || !report.SourcePreserved || report.TraceCount != 2 || report.ApprovalCount != 1 || report.MemoryCount != 1 || !bytes.Equal(before, data) {
		t.Fatal("wrong restoration report")
	}
	if err := sandbox.ImportJoint(t.Context(), data, "a", MaxTaskExportBytes); !errors.Is(err, ErrTaskRestoreConflict) {
		t.Fatal("overwrite allowed", err)
	}
	if _, err := sandbox.Verify(t.Context(), "restore-task", "b"); err == nil {
		t.Fatal("tenant isolation missing")
	}
	// A stored ciphertext/record change must be detected by actual readback.
	if _, err := sandbox.db.Exec("UPDATE restore_parts SET body=? WHERE kind='approval'", []byte(`null`)); err != nil {
		t.Fatal(err)
	}
	if _, err := sandbox.Verify(t.Context(), "restore-task", "a"); !errors.Is(err, ErrTaskRestoreMismatch) {
		t.Fatal("corrupt restored record accepted", err)
	}
}

func TestTaskRestoreRejectsInvalidArtifactsAtomically(t *testing.T) {
	original := restoreFixture(t)
	cases := map[string]func(*TaskExportArtifact, *TaskExportSnapshot){
		"schema":          func(a *TaskExportArtifact, s *TaskExportSnapshot) { a.Schema = "task-trace-export-v1" },
		"hash":            func(a *TaskExportArtifact, s *TaskExportSnapshot) { a.PayloadSHA256 = "bad" },
		"bytes":           func(a *TaskExportArtifact, s *TaskExportSnapshot) { a.PayloadBytes++ },
		"manifest":        func(a *TaskExportArtifact, s *TaskExportSnapshot) { a.Included = a.Included[:1] },
		"counts":          func(a *TaskExportArtifact, s *TaskExportSnapshot) { *a.ApprovalCount++ },
		"source":          func(a *TaskExportArtifact, s *TaskExportSnapshot) { a.SourcePreserved = false },
		"ready":           func(a *TaskExportArtifact, s *TaskExportSnapshot) { a.RestoreReady = true },
		"tenant":          func(a *TaskExportArtifact, s *TaskExportSnapshot) { s.Task.TenantID = "b" },
		"sequence":        func(a *TaskExportArtifact, s *TaskExportSnapshot) { s.Traces[1].Sequence = 4 },
		"approval_tenant": func(a *TaskExportArtifact, s *TaskExportSnapshot) { s.Dependencies.Approvals[0].TenantID = "b" },
		"approval_duplicate": func(a *TaskExportArtifact, s *TaskExportSnapshot) {
			s.Dependencies.Approvals = append(s.Dependencies.Approvals, s.Dependencies.Approvals[0])
		},
		"approval_status": func(a *TaskExportArtifact, s *TaskExportSnapshot) { s.Dependencies.Approvals[0].Status = "unknown" },
		"memory_owner":    func(a *TaskExportArtifact, s *TaskExportSnapshot) { s.Dependencies.Memories[0].TaskID = "other" },
		"memory_session":  func(a *TaskExportArtifact, s *TaskExportSnapshot) { s.Dependencies.Memories[0].SessionID = "other" },
		"session_missing": func(a *TaskExportArtifact, s *TaskExportSnapshot) { s.Dependencies.Session = nil },
		"session_counter": func(a *TaskExportArtifact, s *TaskExportSnapshot) { s.Dependencies.Session.NextSequence = 0 },
		"embedded_trace":  func(a *TaskExportArtifact, s *TaskExportSnapshot) { s.Task.Trace = []types.StepTrace{{Step: 1}} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			var a TaskExportArtifact
			json.Unmarshal(original, &a)
			var snap TaskExportSnapshot
			if err := strictRestoreJSON(a.Payload, &snap); err != nil {
				t.Fatal(err)
			}
			mutate(&a, &snap)
			if name != "hash" && name != "bytes" {
				a.Payload, _ = json.Marshal(&snap)
				sum := sha256.Sum256(a.Payload)
				a.PayloadSHA256 = hex.EncodeToString(sum[:])
				a.PayloadBytes = len(a.Payload)
			}
			data, _ := json.Marshal(&a)
			s, err := NewTaskRestoreSandbox(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if err := s.ImportJoint(t.Context(), data, "a", MaxTaskExportBytes); err == nil {
				t.Fatal("invalid artifact accepted")
			}
			var count int
			if err := s.db.QueryRow("SELECT COUNT(*) FROM restore_parts").Scan(&count); err != nil || count != 0 {
				t.Fatal("partial import", count, err)
			}
		})
	}
	for _, data := range [][]byte{append(bytes.Clone(original), []byte(`{}`)...), bytes.Replace(original, []byte(`"schema":`), []byte(`"schema":"task-joint-export-v2","schema":`), 1), bytes.Replace(original, []byte(`"schema":`), []byte(`"unknown":1,"schema":`), 1)} {
		if _, err := VerifyIsolatedTaskRestore(t.Context(), data, "a", MaxTaskExportBytes); err == nil {
			t.Fatal("ambiguous JSON accepted")
		}
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := VerifyIsolatedTaskRestore(canceled, original, "a", MaxTaskExportBytes); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := VerifyIsolatedTaskRestore(t.Context(), original, "a", 4096); len(original) > 4096 && !errors.Is(err, ErrTaskExportTooLarge) {
		t.Fatal(err)
	}
}

func TestTaskRestoreConcurrentImportHasOneWinner(t *testing.T) {
	data := restoreFixture(t)
	s, err := NewTaskRestoreSandbox(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	results := make(chan error, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { results <- s.ImportJoint(t.Context(), data, "a", MaxTaskExportBytes) })
	}
	wg.Wait()
	close(results)
	winners := 0
	for err := range results {
		if err == nil {
			winners++
		} else if !errors.Is(err, ErrTaskRestoreConflict) {
			t.Fatal(err)
		}
	}
	if winners != 1 {
		t.Fatal("wrong import winner count", winners)
	}
}

func TestTaskRestoreFromSourcePreservesSource(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			var source Store = NewMemoryStore()
			if backend == "sqlite" {
				var err error
				source, err = NewSQLiteStore(filepath.Join(t.TempDir(), "source.db"))
				if err != nil {
					t.Fatal(err)
				}
			}
			defer source.Close()
			task := &types.Task{ID: "live-task", TenantID: "a", Status: types.StatusRunning, Trace: []types.StepTrace{{Step: 1}}}
			if err := source.SaveFullTask(t.Context(), task); err != nil {
				t.Fatal(err)
			}
			reader := source.(TaskJointExportReader)
			before, err := reader.ReadJointTaskExport(t.Context(), task.ID, "a", MaxTaskExportBytes)
			if err != nil {
				t.Fatal(err)
			}
			data, err := EncodeTaskExport(t.Context(), before, backend, task.ID, "a", MaxTaskExportBytes)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := VerifyIsolatedTaskRestore(t.Context(), data, "a", MaxTaskExportBytes); err != nil {
				t.Fatal(err)
			}
			after, err := reader.ReadJointTaskExport(t.Context(), task.ID, "a", MaxTaskExportBytes)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := json.Marshal(before)
			a, _ := json.Marshal(after)
			if !bytes.Equal(b, a) {
				t.Fatal("source changed during drill")
			}
		})
	}
}

func TestTaskRestoreWriteFailureRollsBack(t *testing.T) {
	s, err := NewTaskRestoreSandbox(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.Exec(`CREATE TRIGGER reject_restore BEFORE INSERT ON restore_parts WHEN NEW.kind='approval' BEGIN SELECT RAISE(ABORT,'injected write failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.ImportJoint(t.Context(), restoreFixture(t), "a", MaxTaskExportBytes); err == nil {
		t.Fatal("write failure ignored")
	}
	for _, table := range []string{"restore_runs", "restore_parts"} {
		var count int
		if err := s.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatal("failed import left records", table, count, err)
		}
	}
}

func TestTaskRestoreQuotasAndManifestFields(t *testing.T) {
	data := restoreFixture(t)
	for _, body := range [][]byte{
		bytes.Repeat([]byte("x"), 4097),
		bytes.Replace(data, []byte(`"restore_ready":false,`), nil, 1),
		bytes.Replace(data, []byte(`"schema":`), []byte(`"Schema":`), 1),
	} {
		_, err := VerifyIsolatedTaskRestore(t.Context(), body, "a", 4096)
		if err == nil {
			t.Fatal("quota or required manifest field ignored")
		}
	}
	var a TaskExportArtifact
	if err := json.Unmarshal(data, &a); err != nil {
		t.Fatal(err)
	}
	var snap TaskExportSnapshot
	if err := strictRestoreJSON(a.Payload, &snap); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"trace", "approval", "memory"} {
		t.Run(kind, func(t *testing.T) {
			copy := snap
			deps := *snap.Dependencies
			copy.Dependencies = &deps
			switch kind {
			case "trace":
				copy.Traces = make([]TaskTraceEvent, MaxTaskExportTraces+1)
			case "approval":
				deps.Approvals = make([]*types.DurableApproval, MaxTaskExportDependencies+1)
			case "memory":
				deps.Memories = make([]*types.Memory, MaxTaskExportDependencies+1)
			}
			artifact := a
			artifact.Payload, _ = json.Marshal(&copy)
			sum := sha256.Sum256(artifact.Payload)
			artifact.PayloadSHA256 = hex.EncodeToString(sum[:])
			artifact.PayloadBytes = len(artifact.Payload)
			body, _ := json.Marshal(&artifact)
			if _, err := VerifyIsolatedTaskRestore(t.Context(), body, "a", MaxTaskExportBytes); !errors.Is(err, ErrTaskExportTooLarge) {
				t.Fatal("record quota ignored", err)
			}
		})
	}
}
