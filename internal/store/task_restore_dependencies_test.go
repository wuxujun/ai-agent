package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/types"
)

type dependencyAuthorityFixture struct {
	session      TaskRestoreSessionObservation
	execution    TaskRestoreExecutionObservation
	raw          []byte
	err          error
	cancel       context.CancelFunc
	sessionReads int
	mutateAt     int
}

func (f *dependencyAuthorityFixture) ReadSessionDependencies(ctx context.Context, q TaskRestoreDependencyRequest) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.sessionReads++
	if f.cancel != nil {
		f.cancel()
	}
	if f.mutateAt > 0 && f.sessionReads >= f.mutateAt {
		f.session.Scope.Revision = "generation-2"
	}
	if f.err != nil {
		return nil, f.err
	}
	if f.raw != nil {
		return f.raw, nil
	}
	return json.Marshal(f.session)
}
func (f *dependencyAuthorityFixture) ReadExecutionObservation(ctx context.Context, q TaskRestoreDependencyRequest) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.err != nil {
		return nil, f.err
	}
	return json.Marshal(f.execution)
}

func dependencyFixture(t *testing.T) ([]byte, *admissionTestReader, *restoreDependencyAdmissionReader, *dependencyAuthorityFixture) {
	t.Helper()
	artifact, m, base, _ := admissionFixture(t)
	now := time.Now().UTC()
	f := &dependencyAuthorityFixture{}
	reader, err := NewTaskRestoreDependencyAdmissionReader(t.Context(), base, artifact, "a", TaskRestoreDependencyReaders{SourceID: restoreDigest([]byte("source")), Session: f, Execution: f})
	if err != nil {
		t.Fatal(err)
	}
	r := reader.(*restoreDependencyAdmissionReader)
	q := r.request
	scope := TaskRestoreObservationScope{TaskID: q.TaskID, TenantID: q.TenantID, PayloadSHA256: q.PayloadSHA256, SourceID: q.SourceID, TargetID: m.Binding.TargetID, Revision: "generation-1", TrustRevision: "trust-1", CreatedAt: now.Add(-time.Minute), ExpiresAt: now.Add(5 * time.Minute)}
	snapshot := TaskRestoreSessionSnapshot{SessionID: q.SessionID, SessionSHA256: q.SessionSHA256, Status: types.SessionStatusActive, HighWatermark: 9007199254740993, Revision: "session-1", Members: []TaskRestoreDependencyMember{{ID: q.TaskID, TenantID: q.TenantID, SessionID: q.SessionID, SequenceNo: q.SequenceNo, TaskSHA256: q.TaskSHA256, TraceSHA256: q.CheckpointSHA256, Revision: "task-1"}}, Edges: []TaskRestoreDependencyEdge{}}
	// restoreFixture uses a high sequence; preserve it exactly through JSON.
	snapshot.HighWatermark = q.HighWatermark
	f.session = TaskRestoreSessionObservation{Schema: TaskRestoreSessionObservationSchema, Scope: scope, DependencyContract: "complete-same-session-dag-v1", BaselinePayloadSHA256: q.PayloadSHA256, BaselineRevision: "session-1", Source: snapshot, Target: snapshot}
	f.session.Target.Members = append([]TaskRestoreDependencyMember(nil), snapshot.Members...)
	f.execution = TaskRestoreExecutionObservation{Schema: TaskRestoreExecutionObservationSchema, Scope: scope, Policy: "terminal-diagnostic-only-v1", SourceStatus: q.Status, TargetStatus: q.Status, TaskSHA256: q.TaskSHA256, CheckpointSHA256: q.CheckpointSHA256, RuntimeVersion: "task-restore-diagnostic-v1", LeaseRevision: "lease-1", TargetLeaseRevision: "target-lease-1", IsolationContract: "dispatch-disabled-executors-stopped-v1", IsolationRevision: "isolation-1", IsolationExpiresAt: scope.ExpiresAt}
	return artifact, base, r, f
}

func TestTaskRestoreDependenciesSessionValidation(t *testing.T) {
	cases := []struct {
		name, status string
		mutate       func(*TaskRestoreSessionObservation)
	}{
		{"valid_with_gaps", "verified", func(o *TaskRestoreSessionObservation) {}},
		{"missing_root", "missing", func(o *TaskRestoreSessionObservation) { o.Target.Members = []TaskRestoreDependencyMember{} }},
		{"wrong_tenant", "conflict", func(o *TaskRestoreSessionObservation) { o.Target.Members[0].TenantID = "b" }},
		{"wrong_target", "unverified", func(o *TaskRestoreSessionObservation) { o.Scope.TargetID = restoreDigest(nil) }},
		{"no_baseline", "unverified", func(o *TaskRestoreSessionObservation) { o.BaselineRevision = "" }},
		{"source_changed", "conflict", func(o *TaskRestoreSessionObservation) { o.Source.Revision = "session-2" }},
		{"highwater_decreased", "conflict", func(o *TaskRestoreSessionObservation) { o.Target.HighWatermark-- }},
		{"zero_sequence", "conflict", func(o *TaskRestoreSessionObservation) { o.Target.Members[0].SequenceNo = 0 }},
		{"duplicate_member", "unverified", func(o *TaskRestoreSessionObservation) {
			o.Target.Members = append(o.Target.Members, o.Target.Members[0])
		}},
		{"dangling_edge", "conflict", func(o *TaskRestoreSessionObservation) {
			o.Target.Edges = []TaskRestoreDependencyEdge{{From: o.Target.Members[0].ID, To: "missing"}}
		}},
		{"cycle", "conflict", func(o *TaskRestoreSessionObservation) {
			id := o.Target.Members[0].ID
			o.Target.Edges = []TaskRestoreDependencyEdge{{From: id, To: id}}
		}},
		{"unknown_contract", "unverified", func(o *TaskRestoreSessionObservation) { o.DependencyContract = "partial-list" }},
		{"revoked", "unverified", func(o *TaskRestoreSessionObservation) { o.Scope.TrustRevision = "" }},
		{"expired", "unverified", func(o *TaskRestoreSessionObservation) { o.Scope.ExpiresAt = time.Now().Add(-time.Second) }},
		{"future", "unverified", func(o *TaskRestoreSessionObservation) { o.Scope.CreatedAt = time.Now().Add(time.Minute) }},
		{"long_window", "unverified", func(o *TaskRestoreSessionObservation) { o.Scope.ExpiresAt = time.Now().Add(time.Hour) }},
		{"missing_edges", "unverified", func(o *TaskRestoreSessionObservation) { o.Source.Edges = nil }},
		{"budget", "unverified", func(o *TaskRestoreSessionObservation) {
			o.Source.Members = make([]TaskRestoreDependencyMember, MaxTaskRestoreObservationTasks+1)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, r, f := dependencyFixture(t)
			tc.mutate(&f.session)
			q := r.request
			q.TargetID = f.execution.Scope.TargetID
			if got := validateSessionObservation(f.session, q, time.Now()); got != tc.status {
				t.Fatalf("status=%s want=%s", got, tc.status)
			}
		})
	}
}

func TestTaskRestoreDependenciesExecutionValidation(t *testing.T) {
	cases := []struct {
		name, status string
		mutate       func(*TaskRestoreExecutionObservation, *TaskRestoreDependencyRequest)
	}{
		{"terminal", "verified", func(o *TaskRestoreExecutionObservation, q *TaskRestoreDependencyRequest) {
			q.Status = types.StatusCompleted
			o.SourceStatus = q.Status
			o.TargetStatus = q.Status
		}},
		{"nonterminal", "unverified", func(o *TaskRestoreExecutionObservation, q *TaskRestoreDependencyRequest) {
			q.Status = types.StatusRunning
			o.SourceStatus = q.Status
			o.TargetStatus = q.Status
		}},
		{"active_lease", "conflict", func(o *TaskRestoreExecutionObservation, q *TaskRestoreDependencyRequest) {
			q.Status = types.StatusCompleted
			o.SourceStatus = q.Status
			o.TargetStatus = q.Status
			o.LeasePresent = true
			o.OwnerSHA256 = restoreDigest(nil)
			o.LeaseExpiresAt = time.Now().Add(time.Minute)
		}},
		{"expired_without_stop", "unverified", func(o *TaskRestoreExecutionObservation, q *TaskRestoreDependencyRequest) {
			q.Status = types.StatusCompleted
			o.SourceStatus = q.Status
			o.TargetStatus = q.Status
			o.LeasePresent = true
			o.OwnerSHA256 = restoreDigest(nil)
			o.LeaseExpiresAt = time.Now().Add(-time.Minute)
			o.IsolationContract = ""
		}},
		{"expired_with_stop", "verified", func(o *TaskRestoreExecutionObservation, q *TaskRestoreDependencyRequest) {
			q.Status = types.StatusFailed
			o.SourceStatus = q.Status
			o.TargetStatus = q.Status
			o.LeasePresent = true
			o.OwnerSHA256 = restoreDigest(nil)
			o.LeaseExpiresAt = time.Now().Add(-time.Minute)
		}},
		{"checkpoint_changed", "conflict", func(o *TaskRestoreExecutionObservation, q *TaskRestoreDependencyRequest) {
			o.CheckpointSHA256 = restoreDigest(nil)
		}},
		{"lease_generation_missing", "unverified", func(o *TaskRestoreExecutionObservation, q *TaskRestoreDependencyRequest) { o.LeaseRevision = "" }},
		{"runtime_unknown", "unverified", func(o *TaskRestoreExecutionObservation, q *TaskRestoreDependencyRequest) {
			o.RuntimeVersion = "unknown"
		}},
		{"unknown_policy", "unverified", func(o *TaskRestoreExecutionObservation, q *TaskRestoreDependencyRequest) { o.Policy = "rerun" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, r, f := dependencyFixture(t)
			q := r.request
			q.TargetID = f.execution.Scope.TargetID
			tc.mutate(&f.execution, &q)
			if got := validateExecutionObservation(f.execution, q, time.Now()); got != tc.status {
				t.Fatalf("status=%s want=%s", got, tc.status)
			}
		})
	}
}

func TestTaskRestoreDependenciesStrictInputAndCancellation(t *testing.T) {
	for _, mode := range []string{"unknown", "duplicate", "trailing", "oversize", "error", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			_, _, r, f := dependencyFixture(t)
			data, _ := json.Marshal(f.session)
			switch mode {
			case "unknown":
				f.raw = bytes.Replace(data, []byte(`"schema":`), []byte(`"unexpected":1,"schema":`), 1)
			case "duplicate":
				f.raw = bytes.Replace(data, []byte(`"schema":`), []byte(`"schema":"duplicate","schema":`), 1)
			case "trailing":
				f.raw = append(data, []byte(" {}")...)
			case "oversize":
				f.raw = make([]byte, MaxTaskRestoreObservationBytes+1)
			case "error":
				f.err = errors.New("secret-credential")
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if mode == "cancel" {
				f.cancel = cancel
			}
			result, err := r.scan(ctx, "session_dependencies", f.sessionBinding())
			if mode == "cancel" {
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
				return
			}
			if err != nil || result.status != "unverified" {
				t.Fatal(result, err)
			}
		})
	}
}
func (f *dependencyAuthorityFixture) sessionBinding() TaskRestoreAdmissionBinding {
	return TaskRestoreAdmissionBinding{TaskID: f.session.Scope.TaskID, TenantID: f.session.Scope.TenantID, PayloadSHA256: f.session.Scope.PayloadSHA256, TargetID: f.session.Scope.TargetID}
}

func TestTaskRestoreDependenciesCompositeBinding(t *testing.T) {
	artifact, base, r, f := dependencyFixture(t)
	state, err := r.ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if state.Binding == base.state.Binding {
		t.Fatal("provider not covered by binding")
	}
	data, _ := json.Marshal(f.session)
	claim := TaskRestoreEvidenceClaim{Kind: "session_dependencies", SHA256: restoreDigest(data)}
	o, err := r.CheckEvidence(t.Context(), claim)
	if err != nil || o.Status != "verified" || o.Binding != state.Binding {
		t.Fatal(o, err)
	}
	// Advance monotonic generation without changing the records (ABA).
	f.session.Scope.Revision = "generation-2"
	changed, err := r.ReadState(t.Context())
	if err != nil || changed.Binding == state.Binding {
		t.Fatal("generation not covered", err)
	}
	f.session.Scope.Revision = "generation-1" // fresh fake fixture for evaluation
	m := TaskRestoreDependencyManifest{Schema: TaskRestoreAdmissionSchema, Binding: state.Binding, CreatedAt: time.Now().Add(-time.Minute), ExpiresAt: time.Now().Add(time.Minute), Evidence: []TaskRestoreEvidenceClaim{claim}}
	report, err := EvaluateTaskRestoreAdmission(t.Context(), artifact, admissionJSON(t, m), "a", MaxTaskExportBytes, r, nil)
	if err != nil || report.RestoreReady || report.Decision != "blocked" || len(report.VerifiedEvidence) != 1 {
		t.Fatal(report, err)
	}
	f.mutateAt = f.sessionReads + 2
	report, err = EvaluateTaskRestoreAdmission(t.Context(), artifact, admissionJSON(t, m), "a", MaxTaskExportBytes, r, nil)
	if err != nil || len(report.VerifiedEvidence) != 0 || report.RestoreReady {
		t.Fatal("changed observation verified", report, err)
	}
}

func TestPersistentTaskRestoreDependenciesReadOnly(t *testing.T) {
	artifact, _, _, f := dependencyFixture(t)
	_, dir, err := VerifyPersistentTaskRestore(t.Context(), artifact, "a", t.TempDir(), MaxTaskExportBytes)
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := AssessPersistentTaskRestoreAdmission(t.Context(), dir, "restore-task", "a", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.session.Scope.TargetID = baseline.Binding.TargetID
	f.execution.Scope.TargetID = baseline.Binding.TargetID
	path := filepath.Join(dir, taskRestoreDatabaseName)
	before, _ := os.ReadFile(path)
	entries, _ := os.ReadDir(dir)
	report, err := AssessPersistentTaskRestoreAdmissionWithOptions(t.Context(), dir, "restore-task", "a", nil, TaskRestoreAdmissionOptions{DependencyReaders: TaskRestoreDependencyReaders{SourceID: f.session.Scope.SourceID, Session: f, Execution: f}})
	if err != nil || report.RestoreReady || report.Decision != "blocked" {
		t.Fatal(report, err)
	}
	if len(report.VerifiedEvidence) != 2 || report.VerifiedEvidence[0].Kind != "session_dependencies" {
		t.Fatal(report)
	}
	// A terminal fixture may verify both categories; online recovery stays blocked.
	if !hasAdmissionBlocker(report, "execution", "online_restore_not_implemented") {
		t.Fatal(report)
	}
	after, _ := os.ReadFile(path)
	afterEntries, _ := os.ReadDir(dir)
	if !bytes.Equal(before, after) || len(entries) != len(afterEntries) {
		t.Fatal("source changed")
	}
}

func TestTaskRestoreDependenciesCompleteFakeEvidenceStillBlocked(t *testing.T) {
	artifact, base, r, f := dependencyFixture(t)
	now := time.Now().UTC()
	state, err := r.ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	manifest := TaskRestoreDependencyManifest{Schema: TaskRestoreAdmissionSchema, Binding: state.Binding, CreatedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Minute)}
	for _, kind := range restoreAdmissionKinds {
		digest := restoreDigest([]byte(kind))
		switch kind {
		case "session_dependencies":
			data, _ := json.Marshal(f.session)
			digest = restoreDigest(data)
		case "execution_leases":
			data, _ := json.Marshal(f.execution)
			digest = restoreDigest(data)
		default:
			base.observations[kind] = TaskRestoreEvidenceObservation{Binding: base.state.Binding, SHA256: digest, Status: "verified", VerifiedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Minute)}
		}
		manifest.Evidence = append(manifest.Evidence, TaskRestoreEvidenceClaim{Kind: kind, SHA256: digest})
	}
	report, err := EvaluateTaskRestoreAdmission(t.Context(), artifact, admissionJSON(t, manifest), "a", MaxTaskExportBytes, r, nil)
	if err != nil || !report.EvidenceComplete || len(report.VerifiedEvidence) != 9 || report.RestoreReady || !report.SourcePreserved || report.Decision != "blocked" || len(report.Blockers) != 1 {
		t.Fatal(report, err)
	}
	data, _ := json.Marshal(report)
	for _, secret := range []string{"private-goal", "private-trace", "original-owner", "dispatch-disabled", "generation-1"} {
		if bytes.Contains(data, []byte(secret)) {
			t.Fatal("report leaked private observation")
		}
	}
}

func TestTaskRestoreDependenciesNoProviderAndInvalidConstructor(t *testing.T) {
	artifact, base, _, f := dependencyFixture(t)
	reader, err := NewTaskRestoreDependencyAdmissionReader(t.Context(), base, artifact, "a", TaskRestoreDependencyReaders{})
	if err != nil {
		t.Fatal(err)
	}
	state, err := reader.ReadState(t.Context())
	if err != nil || state != base.state {
		t.Fatal(state, err)
	}
	observation, err := reader.CheckEvidence(t.Context(), TaskRestoreEvidenceClaim{Kind: "session_dependencies", SHA256: restoreDigest(nil)})
	if err != nil || observation.Status == "verified" {
		t.Fatal(observation, err)
	}
	for _, tenant := range []string{"", "b"} {
		if _, err := NewTaskRestoreDependencyAdmissionReader(t.Context(), base, artifact, tenant, TaskRestoreDependencyReaders{SourceID: restoreDigest(nil), Session: f}); err == nil {
			t.Fatal("invalid scope accepted")
		}
	}
	if _, err := NewTaskRestoreDependencyAdmissionReader(t.Context(), base, artifact, "a", TaskRestoreDependencyReaders{SourceID: "not-a-pin", Session: f}); err == nil {
		t.Fatal("invalid source accepted")
	}
	if f.sessionReads != 0 {
		t.Fatal("unbound constructor called authority")
	}
}

func TestTaskRestoreDependenciesVersionAndBaseChanges(t *testing.T) {
	for _, change := range []string{"trust", "lease", "isolation", "checkpoint", "target_lease"} {
		t.Run(change, func(t *testing.T) {
			_, _, r, f := dependencyFixture(t)
			before, err := r.ReadState(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "trust":
				f.session.Scope.TrustRevision = "trust-2"
			case "lease":
				f.execution.LeaseRevision = "lease-2"
			case "isolation":
				f.execution.IsolationRevision = "isolation-2"
			case "checkpoint":
				f.execution.CheckpointSHA256 = restoreDigest(nil)
			case "target_lease":
				f.execution.TargetLeasePresent = true
			}
			after, err := r.ReadState(t.Context())
			if err != nil || after.Binding == before.Binding {
				t.Fatal("change did not invalidate", err)
			}
		})
	}
	_, base, r, _ := dependencyFixture(t)
	changed := base.state
	changed.Binding.StateRevision = restoreDigest([]byte("base changed"))
	base.final = &changed
	if _, err := r.ReadState(t.Context()); !errors.Is(err, ErrTaskRestoreMismatch) {
		t.Fatal("base mutation accepted", err)
	}
}

func TestTaskRestoreDependenciesSessionTraceBinding(t *testing.T) {
	for _, mode := range []string{"complete", "missing_root_trace", "missing_other_trace", "root_trace_changed", "other_trace_mismatch", "legacy_schema"} {
		t.Run(mode, func(t *testing.T) {
			_, _, r, f := dependencyFixture(t)
			q := r.request
			q.TargetID = f.session.Scope.TargetID
			other := TaskRestoreDependencyMember{ID: "restore-task-z", TenantID: q.TenantID, SessionID: q.SessionID, SequenceNo: q.SequenceNo + 1, TaskSHA256: restoreDigest([]byte("other-task")), TraceSHA256: restoreDigest([]byte("other-physical-traces")), Revision: "other-1"}
			f.session.Source.Members = append(f.session.Source.Members, other)
			f.session.Target.Members = append(f.session.Target.Members, other)
			expected := "verified"
			switch mode {
			case "missing_root_trace":
				f.session.Source.Members[0].TraceSHA256 = ""
				expected = "unverified"
			case "missing_other_trace":
				f.session.Target.Members[1].TraceSHA256 = ""
				expected = "unverified"
			case "root_trace_changed":
				f.session.Source.Members[0].TraceSHA256 = restoreDigest([]byte("changed"))
				f.session.Target.Members[0].TraceSHA256 = f.session.Source.Members[0].TraceSHA256
				expected = "conflict"
			case "other_trace_mismatch":
				f.session.Target.Members[1].TraceSHA256 = restoreDigest([]byte("different-checkpoint"))
				expected = "conflict"
			case "legacy_schema":
				f.session.Schema = "task-restore-session-observation-v1"
				expected = "unverified"
			}
			if got := validateSessionObservation(f.session, q, time.Now()); got != expected {
				t.Fatalf("status=%s want=%s", got, expected)
			}
		})
	}
}
