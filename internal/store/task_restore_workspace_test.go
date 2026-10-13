package store

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/policy/restoreworkspace"
)

func restoreWorkspaceFixture(t *testing.T) (string, []byte, string) {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("workspace reads fail closed on unsupported platform")
	}
	_, dir, err := VerifyPersistentTaskRestore(t.Context(), restoreFixture(t), "a", t.TempDir(), MaxTaskExportBytes)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, restoreworkspace.RelativeRoot("restore-task", "a"))
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "file.txt"), []byte("private-workspace"), 0600); err != nil {
		t.Fatal(err)
	}
	_, _, reader, _ := admissionFixture(t)
	evidence := TaskRestoreWorkspaceEvidence{Schema: TaskRestoreWorkspaceSchema, TaskID: "restore-task", TenantID: "a", PayloadSHA256: reader.state.Binding.PayloadSHA256, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(5 * time.Minute), Files: []restoreworkspace.File{{Path: "sub/file.txt", Size: int64(len("private-workspace")), SHA256: restoreDigest([]byte("private-workspace")), Mode: 0600}}}
	data, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	return dir, data, root
}
func workspaceEvidenceJSON(t *testing.T, e TaskRestoreWorkspaceEvidence) []byte {
	t.Helper()
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
func TestTaskRestoreWorkspaceEvidenceVerifiedReadOnly(t *testing.T) {
	dir, data, root := restoreWorkspaceFixture(t)
	path := filepath.Join(dir, taskRestoreDatabaseName)
	db, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fileBefore, err := os.ReadFile(filepath.Join(root, "sub", "file.txt"))
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	report, err := AssessPersistentTaskRestoreAdmissionWithWorkspace(t.Context(), dir, "restore-task", "a", nil, data)
	if err != nil || report.EvidenceComplete || report.RestoreReady || len(report.VerifiedEvidence) != 1 || report.VerifiedEvidence[0].Kind != "workspace" || report.VerifiedEvidence[0].SHA256 != restoreDigest(data) || report.Binding.PolicyVersion != "read-only-workspace-v1" {
		t.Fatal(report, err)
	}
	for _, b := range report.Blockers {
		if b.Kind == "workspace" {
			t.Fatal("workspace blocked", b)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(db, after) {
		t.Fatal("database changed", err)
	}
	fileAfter, err := os.ReadFile(filepath.Join(root, "sub", "file.txt"))
	if err != nil || !bytes.Equal(fileBefore, fileAfter) {
		t.Fatal("file changed", err)
	}
	afterEntries, err := os.ReadDir(root)
	if err != nil || len(entries) != len(afterEntries) {
		t.Fatal("directory changed", err)
	}
}
func TestTaskRestoreWorkspaceEvidenceRejections(t *testing.T) {
	for _, mode := range []string{"missing", "hash", "size", "mode", "extra", "expired", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			dir, data, root := restoreWorkspaceFixture(t)
			var evidence TaskRestoreWorkspaceEvidence
			if err := json.Unmarshal(data, &evidence); err != nil {
				t.Fatal(err)
			}
			expected := "conflict"
			switch mode {
			case "missing":
				evidence.Files[0].Path = "sub/absent.txt"
				expected = "missing"
			case "hash":
				evidence.Files[0].SHA256 = restoreDigest([]byte("wrong"))
			case "size":
				evidence.Files[0].Size++
			case "mode":
				evidence.Files[0].Mode = 0400
			case "extra":
				if err := os.WriteFile(filepath.Join(root, "extra"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			case "expired":
				evidence.CreatedAt = time.Now().Add(-2 * time.Minute)
				evidence.ExpiresAt = time.Now().Add(-time.Minute)
				expected = "unverified"
			case "symlink":
				if err := os.Symlink(t.TempDir(), filepath.Join(root, "link")); err != nil {
					t.Fatal(err)
				}
			}
			report, err := AssessPersistentTaskRestoreAdmissionWithWorkspace(t.Context(), dir, "restore-task", "a", nil, workspaceEvidenceJSON(t, evidence))
			if err != nil || len(report.VerifiedEvidence) != 0 || !hasAdmissionBlocker(report, "workspace", expected) || report.RestoreReady {
				t.Fatal(report, err)
			}
		})
	}
}
func TestTaskRestoreWorkspaceStrictEvidence(t *testing.T) {
	dir, data, _ := restoreWorkspaceFixture(t)
	var good TaskRestoreWorkspaceEvidence
	if err := json.Unmarshal(data, &good); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"tenant", "task", "payload", "schema", "traversal", "absolute", "duplicate", "budget", "permissions", "null_files"} {
		t.Run(mode, func(t *testing.T) {
			e := good
			e.Files = append([]restoreworkspace.File{}, good.Files...)
			switch mode {
			case "tenant":
				e.TenantID = "b"
			case "task":
				e.TaskID = "other"
			case "payload":
				e.PayloadSHA256 = restoreDigest(nil)
			case "schema":
				e.Schema = "v2"
			case "traversal":
				e.Files[0].Path = "../secret"
			case "absolute":
				e.Files[0].Path = "/secret"
			case "duplicate":
				e.Files = append(e.Files, e.Files[0])
			case "budget":
				e.Files[0].Size = restoreworkspace.MaxBytes + 1
			case "permissions":
				e.Files[0].Mode = 0644
			case "null_files":
				e.Files = nil
			}
			report, err := AssessPersistentTaskRestoreAdmissionWithWorkspace(t.Context(), dir, "restore-task", "a", nil, workspaceEvidenceJSON(t, e))
			if err == nil || report != nil {
				t.Fatal("unsafe baseline accepted", mode)
			}
		})
	}
	for _, invalid := range [][]byte{
		[]byte(strings.Replace(string(data), `"schema":`, `"workspace_directory":"/online","schema":`, 1)),
		[]byte(strings.Replace(string(data), `"schema":`, `"schema":"duplicate","schema":`, 1)),
		append(bytes.Clone(data), []byte(" {}")...),
	} {
		if report, err := AssessPersistentTaskRestoreAdmissionWithWorkspace(t.Context(), dir, "restore-task", "a", nil, invalid); err == nil || report != nil {
			t.Fatal("untrusted baseline fields accepted")
		}
	}
	if report, err := AssessPersistentTaskRestoreAdmissionWithWorkspace(t.Context(), dir, "restore-task", "a", nil, bytes.Repeat([]byte("x"), MaxTaskRestoreWorkspaceEvidenceBytes+1)); err == nil || report != nil {
		t.Fatal("oversized evidence accepted")
	}
}

type workspaceMutationReader struct {
	inner  *persistentRestoreAdmissionReader
	mutate func()
	after  bool
}

func (r *workspaceMutationReader) ReadState(ctx context.Context) (TaskRestoreAdmissionState, error) {
	return r.inner.ReadState(ctx)
}
func (r *workspaceMutationReader) CheckEvidence(ctx context.Context, c TaskRestoreEvidenceClaim) (TaskRestoreEvidenceObservation, error) {
	if c.Kind != "workspace" {
		return r.inner.CheckEvidence(ctx, c)
	}
	if !r.after {
		r.mutate()
	}
	result, err := r.inner.CheckEvidence(ctx, c)
	if r.after {
		r.mutate()
	}
	return result, err
}
func TestTaskRestoreWorkspaceInvalidatedDuringAssessment(t *testing.T) {
	for _, after := range []bool{false, true} {
		t.Run(map[bool]string{false: "before_check", true: "after_check"}[after], func(t *testing.T) {
			dir, data, root := restoreWorkspaceFixture(t)
			sandbox, err := OpenPersistentTaskRestoreSandbox(t.Context(), dir)
			if err != nil {
				t.Fatal(err)
			}
			defer sandbox.Close()
			path := filepath.Join(dir, taskRestoreDatabaseName)
			identity, _ := os.Lstat(path)
			dirIdentity, _ := os.Lstat(dir)
			e := TaskRestoreWorkspaceEvidence{}
			if err := json.Unmarshal(data, &e); err != nil {
				t.Fatal(err)
			}
			workspace, err := decodeRestoreWorkspaceEvidence(data, "restore-task", "a", e.PayloadSHA256)
			if err != nil {
				t.Fatal(err)
			}
			workspace.directory = dir
			reader := &persistentRestoreAdmissionReader{sandbox: sandbox, path: path, identity: identity, dirIdentity: dirIdentity, task: "restore-task", tenant: "a", workspace: workspace}
			initial, err := reader.ReadState(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			m := TaskRestoreDependencyManifest{Schema: TaskRestoreAdmissionSchema, Binding: initial.Binding, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(5 * time.Minute), Evidence: []TaskRestoreEvidenceClaim{{Kind: "workspace", SHA256: restoreDigest(data)}}}
			mutating := &workspaceMutationReader{inner: reader, after: after, mutate: func() {
				if err := os.WriteFile(filepath.Join(root, "sub", "file.txt"), []byte("private-changed"), 0600); err != nil {
					t.Fatal(err)
				}
			}}
			report, err := EvaluateTaskRestoreAdmission(t.Context(), restoreFixture(t), admissionJSON(t, m), "a", MaxTaskExportBytes, mutating, nil)
			if err != nil || len(report.VerifiedEvidence) != 0 || report.EvidenceComplete || !hasAdmissionBlocker(report, "target", "state_changed") {
				t.Fatal("workspace mutation retained evidence", report, err)
			}
		})
	}
}
