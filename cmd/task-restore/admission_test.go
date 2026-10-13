package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/policy/restoreworkspace"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

func admissionCommandFixture(t *testing.T) string {
	t.Helper()
	source := store.NewMemoryStore()
	defer source.Close()
	task := &types.Task{ID: "admission-task", TenantID: "a", Status: types.StatusFailed, Goal: "secret-task-body"}
	if err := source.SaveFullTask(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	snapshot, err := source.ReadJointTaskExport(t.Context(), task.ID, "a", store.MaxTaskExportBytes)
	if err != nil {
		t.Fatal(err)
	}
	data, err := store.EncodeTaskExport(t.Context(), snapshot, "memory", task.ID, "a", store.MaxTaskExportBytes)
	if err != nil {
		t.Fatal(err)
	}
	_, dir, err := store.VerifyPersistentTaskRestore(t.Context(), data, "a", t.TempDir(), store.MaxTaskExportBytes)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestTaskRestoreAdmissionCommand(t *testing.T) {
	dir := admissionCommandFixture(t)
	var out, errOut bytes.Buffer
	args := []string{"assess", "--directory", dir, "--task", "admission-task", "--tenant", "a"}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	before := map[string][]byte{}
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		before[entry.Name()] = data
	}
	if code := run(t.Context(), args, &out, &errOut); code != 3 {
		t.Fatal("blocked diagnostic exit code", code, errOut.String())
	}
	var report store.TaskRestoreAdmissionReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Decision != "blocked" || report.EvidenceComplete || report.RestoreReady || !report.SourcePreserved || report.Binding == nil || len(report.Blockers) != 10 {
		t.Fatal(report)
	}
	if bytes.Contains(out.Bytes(), []byte("secret-")) || bytes.Contains(out.Bytes(), []byte(dir)) {
		t.Fatal("report exposed body/path")
	}
	m := store.TaskRestoreDependencyManifest{Schema: store.TaskRestoreAdmissionSchema, Binding: *report.Binding, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(5 * time.Minute), Evidence: []store.TaskRestoreEvidenceClaim{{Kind: "workspace", SHA256: report.PayloadSHA256}}}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(manifest, data, 0600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	if code := run(t.Context(), append(args, "--manifest", manifest), &out, &errOut); code != 3 {
		t.Fatal(code, errOut.String())
	}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, b := range report.Blockers {
		if b.Kind == "workspace" && b.Code == "unverified" {
			found = true
		}
	}
	if !found || report.EvidenceComplete || report.RestoreReady {
		t.Fatal("claim granted evidence", report)
	}
	after, err := os.ReadFile(manifest)
	if err != nil || !bytes.Equal(after, data) {
		t.Fatal("manifest changed", err)
	}
	afterEntries, err := os.ReadDir(dir)
	if err != nil || len(afterEntries) != len(entries) {
		t.Fatal("new target files", err)
	}
	for name, data := range before {
		after, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || !bytes.Equal(data, after) {
			t.Fatal("target changed", name, err)
		}
	}
}

func TestTaskRestoreAdmissionCommandInvalidInput(t *testing.T) {
	dir := admissionCommandFixture(t)
	file := filepath.Join(t.TempDir(), "manifest.json")
	for _, body := range [][]byte{nil, []byte(`{"schema":"private-error"}`), bytes.Repeat([]byte("x"), store.MaxTaskRestoreManifestBytes+1)} {
		if err := os.WriteFile(file, body, 0600); err != nil {
			t.Fatal(err)
		}
		var out, errOut bytes.Buffer
		if code := run(t.Context(), []string{"assess", "--directory", dir, "--task", "admission-task", "--tenant", "a", "--manifest", file}, &out, &errOut); code != 1 || out.Len() != 0 || bytes.Contains(errOut.Bytes(), []byte("private-")) {
			t.Fatal(code, out.String(), errOut.String())
		}
	}
	for _, args := range [][]string{{"assess"}, {"assess", "--directory", dir, "--task", "admission-task", "--tenant", "a", "--unexpected"}, {"assess", "--directory", dir, "--task", "admission-task", "--tenant", "b"}, {"assess", "--directory", dir, "--task", "admission-task", "--tenant", "a", "--manifest", dir}} {
		var out, errOut bytes.Buffer
		if code := run(t.Context(), args, &out, &errOut); code == 0 || code == 3 || out.Len() != 0 {
			t.Fatal("invalid assessment returned diagnostic", args, code)
		}
	}
}

func TestTaskRestoreWorkspaceCommand(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("workspace checker unsupported")
	}
	dir := admissionCommandFixture(t)
	root := filepath.Join(dir, restoreworkspace.RelativeRoot("admission-task", "a"))
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	content := []byte("private-file-body")
	leaf := filepath.Join(root, "private-name.txt")
	if err := os.WriteFile(leaf, content, 0600); err != nil {
		t.Fatal(err)
	}
	report, err := store.AssessPersistentTaskRestoreAdmission(t.Context(), dir, "admission-task", "a", nil)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(content)
	baseline := store.TaskRestoreWorkspaceEvidence{Schema: store.TaskRestoreWorkspaceSchema, TaskID: "admission-task", TenantID: "a", PayloadSHA256: report.PayloadSHA256, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(5 * time.Minute), Files: []restoreworkspace.File{{Path: "private-name.txt", Size: int64(len(content)), SHA256: hex.EncodeToString(hash[:]), Mode: 0600}}}
	data, err := json.Marshal(baseline)
	if err != nil {
		t.Fatal(err)
	}
	evidenceFile := filepath.Join(t.TempDir(), "evidence.json")
	if err := os.WriteFile(evidenceFile, data, 0600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	args := []string{"assess", "--directory", dir, "--task", "admission-task", "--tenant", "a", "--workspace-evidence", evidenceFile}
	if code := run(t.Context(), args, &out, &errOut); code != 3 {
		t.Fatal(code, errOut.String())
	}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.EvidenceComplete || report.RestoreReady || len(report.VerifiedEvidence) != 1 || report.VerifiedEvidence[0].Kind != "workspace" {
		t.Fatal(report)
	}
	if bytes.Contains(out.Bytes(), []byte("private-")) || bytes.Contains(out.Bytes(), []byte(dir)) {
		t.Fatal("workspace detail leaked")
	}
	after, err := os.ReadFile(evidenceFile)
	if err != nil || !bytes.Equal(after, data) {
		t.Fatal("baseline changed", err)
	}
	after, err = os.ReadFile(leaf)
	if err != nil || !bytes.Equal(after, content) {
		t.Fatal("workspace changed", err)
	}
	// Old database-only bindings are unusable once workspace state is included.
	plain, err := store.AssessPersistentTaskRestoreAdmission(t.Context(), dir, "admission-task", "a", nil)
	if err != nil {
		t.Fatal(err)
	}
	dependency := store.TaskRestoreDependencyManifest{Schema: store.TaskRestoreAdmissionSchema, Binding: *plain.Binding, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(5 * time.Minute), Evidence: report.VerifiedEvidence}
	manifestData, err := json.Marshal(dependency)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(t.TempDir(), "dependencies.json")
	if err := os.WriteFile(manifestPath, manifestData, 0600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	if code := run(t.Context(), append(args, "--manifest", manifestPath), &out, &errOut); code != 3 {
		t.Fatal(code, errOut.String())
	}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, b := range report.Blockers {
		if b.Kind == "target" && b.Code == "state_binding_mismatch" {
			found = true
		}
	}
	if !found || len(report.VerifiedEvidence) != 0 {
		t.Fatal("database-only state admitted workspace", report)
	}
}
