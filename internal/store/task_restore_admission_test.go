package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type admissionTestReader struct {
	state                   TaskRestoreAdmissionState
	final                   *TaskRestoreAdmissionState
	observations            map[string]TaskRestoreEvidenceObservation
	stateErr, errorEvidence error
	reads, checks           int
	cancel                  context.CancelFunc
}

func (r *admissionTestReader) ReadState(context.Context) (TaskRestoreAdmissionState, error) {
	r.reads++
	if r.reads > 1 && r.final != nil {
		return *r.final, r.stateErr
	}
	return r.state, r.stateErr
}
func (r *admissionTestReader) CheckEvidence(_ context.Context, c TaskRestoreEvidenceClaim) (TaskRestoreEvidenceObservation, error) {
	r.checks++
	if r.cancel != nil {
		r.cancel()
	}
	return r.observations[c.Kind], r.errorEvidence
}

func admissionFixture(t *testing.T) ([]byte, TaskRestoreDependencyManifest, *admissionTestReader, time.Time) {
	t.Helper()
	artifact := restoreFixture(t)
	a, _, _, err := decodeJointRestore(t.Context(), artifact, "a", MaxTaskExportBytes)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	b := TaskRestoreAdmissionBinding{TaskID: "restore-task", TenantID: "a", PayloadSHA256: a.PayloadSHA256, TargetID: restoreDigest([]byte("isolated-target")), TargetVersion: "isolated-sqlite-v1", RuntimeVersion: "test-v1", PolicyVersion: "policy-v1", StateRevision: restoreDigest([]byte("revision-1"))}
	manifest := TaskRestoreDependencyManifest{Schema: TaskRestoreAdmissionSchema, Binding: b, CreatedAt: now.Add(-time.Minute), ExpiresAt: now.Add(5 * time.Minute), Evidence: []TaskRestoreEvidenceClaim{}}
	reader := &admissionTestReader{state: TaskRestoreAdmissionState{Binding: b, SandboxVerified: true}, observations: map[string]TaskRestoreEvidenceObservation{}}
	for _, kind := range restoreAdmissionKinds {
		digest := restoreDigest([]byte(kind))
		manifest.Evidence = append(manifest.Evidence, TaskRestoreEvidenceClaim{Kind: kind, SHA256: digest})
		reader.observations[kind] = TaskRestoreEvidenceObservation{Binding: b, SHA256: digest, Status: "verified", VerifiedAt: now.Add(-time.Minute), ExpiresAt: now.Add(5 * time.Minute)}
	}
	return artifact, manifest, reader, now
}

func admissionJSON(t *testing.T, m TaskRestoreDependencyManifest) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func hasAdmissionBlocker(r *TaskRestoreAdmissionReport, kind, code string) bool {
	for _, b := range r.Blockers {
		if b.Kind == kind && b.Code == code {
			return true
		}
	}
	return false
}

func TestTaskRestoreAdmissionCompleteEvidenceStillBlocked(t *testing.T) {
	artifact, m, reader, now := admissionFixture(t)
	before := bytes.Clone(artifact)
	data := admissionJSON(t, m)
	manifestBefore := bytes.Clone(data)
	report, err := EvaluateTaskRestoreAdmission(t.Context(), artifact, data, "a", MaxTaskExportBytes, reader, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if !report.EvidenceComplete || report.RestoreReady || !report.SourcePreserved || report.Decision != "blocked" || len(report.Blockers) != 1 || !hasAdmissionBlocker(report, "execution", "online_restore_not_implemented") || reader.reads != 2 || reader.checks != len(restoreAdmissionKinds) {
		t.Fatal("wrong diagnostic result", report, reader)
	}
	if !bytes.Equal(before, artifact) || !bytes.Equal(manifestBefore, data) {
		t.Fatal("input mutated")
	}
}

func TestTaskRestoreAdmissionFailClosed(t *testing.T) {
	cases := []struct {
		name, kind, code string
		mutate           func(*TaskRestoreDependencyManifest, *admissionTestReader, time.Time)
	}{
		{"missing", "workspace", "missing", func(m *TaskRestoreDependencyManifest, r *admissionTestReader, n time.Time) {
			m.Evidence = m.Evidence[1:]
		}},
		{"manifest_tenant", "manifest", "artifact_binding_mismatch", func(m *TaskRestoreDependencyManifest, r *admissionTestReader, n time.Time) { m.Binding.TenantID = "b" }},
		{"manifest_task", "manifest", "artifact_binding_mismatch", func(m *TaskRestoreDependencyManifest, r *admissionTestReader, n time.Time) {
			m.Binding.TaskID = "other"
		}},
		{"manifest_hash", "manifest", "artifact_binding_mismatch", func(m *TaskRestoreDependencyManifest, r *admissionTestReader, n time.Time) {
			m.Binding.PayloadSHA256 = restoreDigest([]byte("other"))
		}},
		{"expired", "manifest", "expired_or_invalid_time", func(m *TaskRestoreDependencyManifest, r *admissionTestReader, n time.Time) { m.ExpiresAt = n }},
		{"future", "manifest", "expired_or_invalid_time", func(m *TaskRestoreDependencyManifest, r *admissionTestReader, n time.Time) {
			m.CreatedAt = n.Add(time.Minute)
		}},
		{"long_lived", "manifest", "expired_or_invalid_time", func(m *TaskRestoreDependencyManifest, r *admissionTestReader, n time.Time) {
			m.ExpiresAt = n.Add(time.Hour)
		}},
		{"unverified_target", "target", "sandbox_unverified", func(m *TaskRestoreDependencyManifest, r *admissionTestReader, n time.Time) {
			r.state.SandboxVerified = false
		}},
		{"target_error", "target", "unverified", func(m *TaskRestoreDependencyManifest, r *admissionTestReader, n time.Time) {
			r.stateErr = errors.New("secret-target-path")
		}},
		{"missing_key", "key_management", "missing", func(m *TaskRestoreDependencyManifest, r *admissionTestReader, n time.Time) {
			o := r.observations["key_management"]
			o.Status = "missing"
			r.observations["key_management"] = o
		}},
		{"id_conflict", "session_dependencies", "conflict", func(m *TaskRestoreDependencyManifest, r *admissionTestReader, n time.Time) {
			o := r.observations["session_dependencies"]
			o.Status = "conflict"
			r.observations["session_dependencies"] = o
		}},
		{"waiver", "workspace", "unverified", func(m *TaskRestoreDependencyManifest, r *admissionTestReader, n time.Time) {
			o := r.observations["workspace"]
			o.Status = "not_applicable"
			r.observations["workspace"] = o
		}},
		{"reader_error", "workspace", "unverified", func(m *TaskRestoreDependencyManifest, r *admissionTestReader, n time.Time) {
			r.errorEvidence = errors.New("secret-credential")
		}},
		{"evidence_tenant", "workspace", "evidence_binding_mismatch", func(m *TaskRestoreDependencyManifest, r *admissionTestReader, n time.Time) {
			o := r.observations["workspace"]
			o.Binding.TenantID = "b"
			r.observations["workspace"] = o
		}},
		{"evidence_hash", "workspace", "evidence_binding_mismatch", func(m *TaskRestoreDependencyManifest, r *admissionTestReader, n time.Time) {
			o := r.observations["workspace"]
			o.SHA256 = restoreDigest([]byte("changed"))
			r.observations["workspace"] = o
		}},
		{"stale_evidence", "execution_leases", "expired_or_invalid_time", func(m *TaskRestoreDependencyManifest, r *admissionTestReader, n time.Time) {
			o := r.observations["execution_leases"]
			o.ExpiresAt = n
			r.observations["execution_leases"] = o
		}},
		{"state_changed", "target", "state_changed", func(m *TaskRestoreDependencyManifest, r *admissionTestReader, n time.Time) {
			s := r.state
			s.Binding.StateRevision = restoreDigest([]byte("changed"))
			r.final = &s
		}},
		{"verification_lost", "target", "state_changed", func(m *TaskRestoreDependencyManifest, r *admissionTestReader, n time.Time) {
			s := r.state
			s.SandboxVerified = false
			r.final = &s
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			artifact, m, r, now := admissionFixture(t)
			tc.mutate(&m, r, now)
			report, err := EvaluateTaskRestoreAdmission(t.Context(), artifact, admissionJSON(t, m), "a", MaxTaskExportBytes, r, func() time.Time { return now })
			if err != nil || report.EvidenceComplete || report.RestoreReady || !hasAdmissionBlocker(report, tc.kind, tc.code) {
				t.Fatal("failure not blocked", report, err)
			}
			body, _ := json.Marshal(report)
			if bytes.Contains(body, []byte("secret-")) || bytes.Contains(body, []byte("private-")) {
				t.Fatal("report exposed secret")
			}
			if tc.kind == "manifest" && r.checks != 0 {
				t.Fatal("unbound manifest consulted evidence")
			}
		})
	}
	// Every identity/version dimension invalidates otherwise complete evidence.
	for _, field := range []string{"target", "target_version", "runtime", "policy", "revision"} {
		t.Run(field, func(t *testing.T) {
			artifact, m, r, now := admissionFixture(t)
			switch field {
			case "target":
				r.state.Binding.TargetID = restoreDigest([]byte("other"))
			case "target_version":
				r.state.Binding.TargetVersion = "v2"
			case "runtime":
				r.state.Binding.RuntimeVersion = "v2"
			case "policy":
				r.state.Binding.PolicyVersion = "v2"
			case "revision":
				r.state.Binding.StateRevision = restoreDigest([]byte("other"))
			}
			report, err := EvaluateTaskRestoreAdmission(t.Context(), artifact, admissionJSON(t, m), "a", MaxTaskExportBytes, r, func() time.Time { return now })
			if err != nil || report.EvidenceComplete || !hasAdmissionBlocker(report, "target", "state_binding_mismatch") || r.checks != 0 {
				t.Fatal(report, err)
			}
		})
	}
}

func TestTaskRestoreAdmissionStrictManifestAndBudget(t *testing.T) {
	artifact, m, r, now := admissionFixture(t)
	valid := admissionJSON(t, m)
	bad := [][]byte{nil, []byte("null"), append(bytes.Clone(valid), []byte(" {}")...), []byte(strings.Replace(string(valid), `"schema":`, `"unknown":1,"schema":`, 1)), []byte(strings.Replace(string(valid), `"schema":`, `"schema":"duplicate","schema":`, 1)), bytes.Repeat([]byte("x"), MaxTaskRestoreManifestBytes+1)}
	duplicate := m
	duplicate.Evidence = append([]TaskRestoreEvidenceClaim{m.Evidence[0]}, m.Evidence[:len(m.Evidence)-1]...)
	bad = append(bad, admissionJSON(t, duplicate))
	unknown := m
	unknown.Schema = "v2"
	bad = append(bad, admissionJSON(t, unknown))
	unknown = m
	unknown.Evidence = []TaskRestoreEvidenceClaim{{Kind: "skip-all", SHA256: restoreDigest(nil)}}
	bad = append(bad, admissionJSON(t, unknown))
	unknown = m
	unknown.Evidence = []TaskRestoreEvidenceClaim{{Kind: "workspace", SHA256: "bad"}}
	bad = append(bad, admissionJSON(t, unknown))
	for i, data := range bad {
		report, err := EvaluateTaskRestoreAdmission(t.Context(), artifact, data, "a", MaxTaskExportBytes, r, func() time.Time { return now })
		if err == nil || report != nil {
			t.Fatal("invalid manifest accepted", i)
		}
	}
	if r.reads != 0 || r.checks != 0 {
		t.Fatal("invalid manifest accessed trusted reader")
	}
	if report, err := EvaluateTaskRestoreAdmission(t.Context(), artifact, valid, "b", MaxTaskExportBytes, r, nil); err == nil || report != nil {
		t.Fatal("wrong artifact tenant accepted")
	}
	report, err := EvaluateTaskRestoreAdmission(t.Context(), artifact, valid, "a", MaxTaskExportBytes, nil, func() time.Time { return now })
	if err != nil || report.EvidenceComplete || !hasAdmissionBlocker(report, "target", "unverified") {
		t.Fatal("nil reader accepted", err)
	}
}

func TestTaskRestoreAdmissionExpiryAndCancellation(t *testing.T) {
	for _, mode := range []string{"manifest", "evidence", "backwards", "transient_backwards"} {
		t.Run(mode, func(t *testing.T) {
			artifact, m, r, now := admissionFixture(t)
			if mode == "evidence" {
				o := r.observations["workspace"]
				o.ExpiresAt = now.Add(time.Second)
				r.observations["workspace"] = o
			}
			calls := 0
			clock := func() time.Time {
				calls++
				if mode == "transient_backwards" && calls == 2 {
					return now.Add(-time.Minute)
				}
				if calls <= 1+len(restoreAdmissionKinds) {
					return now
				}
				if mode == "backwards" {
					return now.Add(-time.Minute)
				}
				if mode == "evidence" {
					return now.Add(2 * time.Second)
				}
				return now.Add(6 * time.Minute)
			}
			report, err := EvaluateTaskRestoreAdmission(t.Context(), artifact, admissionJSON(t, m), "a", MaxTaskExportBytes, r, clock)
			if err != nil || report.EvidenceComplete {
				t.Fatal("assessment expiration accepted", report, err)
			}
			kind, code := "manifest", "expired_during_assessment"
			if mode == "evidence" {
				kind = "workspace"
			} else if mode == "backwards" || mode == "transient_backwards" {
				kind = "assessment"
				code = "clock_moved_backwards"
			}
			if !hasAdmissionBlocker(report, kind, code) {
				t.Fatal(report)
			}
		})
	}
	artifact, m, r, now := admissionFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r.cancel = cancel
	report, err := EvaluateTaskRestoreAdmission(ctx, artifact, admissionJSON(t, m), "a", MaxTaskExportBytes, r, func() time.Time { return now })
	if !errors.Is(err, context.Canceled) || report != nil {
		t.Fatal("canceled evaluation returned report", err)
	}
}

func TestPersistentTaskRestoreAdmissionReadOnly(t *testing.T) {
	_, dir, err := VerifyPersistentTaskRestore(t.Context(), restoreFixture(t), "a", t.TempDir(), MaxTaskExportBytes)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, taskRestoreDatabaseName)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	report, err := AssessPersistentTaskRestoreAdmission(t.Context(), dir, "restore-task", "a", nil)
	if err != nil || report.EvidenceComplete || report.RestoreReady || report.Binding == nil {
		t.Fatal(report, err)
	}
	for _, kind := range restoreAdmissionKinds {
		if !hasAdmissionBlocker(report, kind, "missing") {
			t.Fatal("missing category absent", kind)
		}
	}
	// A complete user-supplied digest list cannot manufacture trusted evidence.
	m := TaskRestoreDependencyManifest{Schema: TaskRestoreAdmissionSchema, Binding: *report.Binding, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(5 * time.Minute)}
	for _, kind := range restoreAdmissionKinds {
		m.Evidence = append(m.Evidence, TaskRestoreEvidenceClaim{Kind: kind, SHA256: restoreDigest([]byte(kind))})
	}
	report, err = AssessPersistentTaskRestoreAdmission(t.Context(), dir, "restore-task", "a", admissionJSON(t, m))
	if err != nil || report.EvidenceComplete || report.RestoreReady {
		t.Fatal(report, err)
	}
	for _, kind := range restoreAdmissionKinds {
		if !hasAdmissionBlocker(report, kind, "unverified") {
			t.Fatal("unverified category absent", kind)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("assessment changed database", err)
	}
	afterEntries, err := os.ReadDir(dir)
	if err != nil || len(entries) != len(afterEntries) {
		t.Fatal("assessment created files", err)
	}
	if report, err := AssessPersistentTaskRestoreAdmission(t.Context(), dir, "restore-task", "b", nil); err == nil || report != nil {
		t.Fatal("wrong tenant admitted")
	}
}

func TestPersistentTaskRestoreAdmissionRejectsOversizedDatabase(t *testing.T) {
	_, dir, err := VerifyPersistentTaskRestore(t.Context(), restoreFixture(t), "a", t.TempDir(), MaxTaskExportBytes)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, taskRestoreDatabaseName)
	if err := os.Truncate(path, maxTaskRestoreAdmissionDatabaseBytes+1); err != nil {
		t.Fatal(err)
	}
	report, err := AssessPersistentTaskRestoreAdmission(t.Context(), dir, "restore-task", "a", nil)
	if !errors.Is(err, ErrTaskRestoreTarget) || report != nil {
		t.Fatal("oversized database accepted", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() != maxTaskRestoreAdmissionDatabaseBytes+1 {
		t.Fatal("oversized evidence changed", err)
	}
}
