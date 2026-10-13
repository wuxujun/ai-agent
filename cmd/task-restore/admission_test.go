package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/approvalcrypto"
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

func TestTaskRestoreSignedApprovalSourceCommand(t *testing.T) {
	source := store.NewMemoryStore()
	defer source.Close()
	task := &types.Task{ID: "source-task", TenantID: "a", Status: types.StatusFailed, Goal: "private-goal"}
	if err := source.SaveFullTask(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{0x6a}, 32)
	codec, err := approvalcrypto.New(key)
	if err != nil {
		t.Fatal(err)
	}
	action, err := codec.Encrypt([]byte(`{"action":"write","parameters":{"private":"body","large":9007199254740993}}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := source.CreateApproval(t.Context(), &types.DurableApproval{ID: "private-approval", TaskID: task.ID, TenantID: "a", Status: types.ApprovalPending, Request: types.ApprovalRequest{ID: "private-approval", TaskID: task.ID, Action: "write", RiskLevel: types.RiskLevelHigh}, ActionPayload: action}); err != nil {
		t.Fatal(err)
	}
	snap, err := source.ReadJointTaskExport(t.Context(), task.ID, "a", store.MaxTaskExportBytes)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := store.EncodeTaskExport(t.Context(), snap, "memory", task.ID, "a", store.MaxTaskExportBytes)
	if err != nil {
		t.Fatal(err)
	}
	_, dir, err := store.VerifyPersistentTaskRestore(t.Context(), artifact, "a", t.TempDir(), store.MaxTaskExportBytes)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := store.AssessPersistentTaskRestoreAdmission(t.Context(), dir, task.ID, "a", nil)
	if err != nil {
		t.Fatal(err)
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.TaskRestoreApprovalSourceRecordFor(snap.Dependencies.Approvals[0])
	if err != nil {
		t.Fatal(err)
	}
	content := store.TaskRestoreApprovalSourceContent{Schema: store.TaskRestoreApprovalSourceSchema, SourceID: "private-source", SnapshotID: "private-snapshot", TaskID: task.ID, TenantID: "a", PayloadSHA256: plain.PayloadSHA256, CreatedAt: time.Now().UTC().Add(-time.Second), ExpiresAt: time.Now().Add(5 * time.Minute), Approvals: []store.TaskRestoreApprovalSourceRecord{record}}
	signingBytes, err := store.TaskRestoreApprovalSourceSigningBytes(content)
	if err != nil {
		t.Fatal(err)
	}
	e := store.TaskRestoreApprovalSourceEvidence{Content: content, Signature: ed25519.Sign(private, signingBytes)}
	proof, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "private-proof.json")
	if err := os.WriteFile(file, proof, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AI_AGENT_APPROVAL_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(key))
	t.Setenv("AI_AGENT_APPROVAL_ENCRYPTION_PREVIOUS_KEYS", "")
	t.Setenv("AI_AGENT_RESTORE_APPROVAL_SOURCE_PUBLIC_KEY", base64.StdEncoding.EncodeToString(public))
	t.Setenv("AI_AGENT_RESTORE_APPROVAL_SOURCE_ID", content.SourceID)
	t.Setenv("AI_AGENT_RESTORE_APPROVAL_SOURCE_TENANT", "a")
	t.Setenv("AI_AGENT_RESTORE_APPROVAL_SOURCE_SNAPSHOT", content.SnapshotID)
	base := []string{"assess", "--directory", dir, "--task", task.ID, "--tenant", "a", "--key-source", "environment"}
	args := append(append([]string{}, base...), "--approval-source-evidence", file, "--approval-source-trust", "environment")
	for _, mode := range []string{"unselected", "verified", "forged", "revoked"} {
		t.Run(mode, func(t *testing.T) {
			selected := args
			if mode == "unselected" {
				selected = base
			}
			if mode == "forged" {
				copy := e
				copy.Signature = bytes.Clone(e.Signature)
				copy.Signature[0] ^= 1
				b, _ := json.Marshal(copy)
				if err := os.WriteFile(file, b, 0600); err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := os.WriteFile(file, proof, 0600); err != nil {
						t.Fatal(err)
					}
				}()
			}
			if mode == "revoked" {
				t.Setenv("AI_AGENT_RESTORE_APPROVAL_SOURCE_PUBLIC_KEY", "")
			}
			var out, errOut bytes.Buffer
			if code := run(t.Context(), selected, &out, &errOut); code != 3 {
				t.Fatal(code, errOut.String())
			}
			var report store.TaskRestoreAdmissionReport
			if err := json.Unmarshal(out.Bytes(), &report); err != nil {
				t.Fatal(err)
			}
			verified := mode == "verified"
			if (len(report.VerifiedEvidence) == 1) != verified || (report.KeyManagement.SourceBinding == "verified") != verified || report.RestoreReady || report.Decision != "blocked" {
				t.Fatal(report)
			}
			for _, secret := range []string{"private-", dir, base64.StdEncoding.EncodeToString(key), base64.StdEncoding.EncodeToString(public), "9007199254740993"} {
				if bytes.Contains(out.Bytes(), []byte(secret)) || bytes.Contains(errOut.Bytes(), []byte(secret)) {
					t.Fatal("source details exposed")
				}
			}
		})
	}
	after, err := os.ReadFile(file)
	if err != nil || !bytes.Equal(after, proof) {
		t.Fatal("proof overwritten", err)
	}
	for _, extra := range [][]string{{"--approval-source-evidence", file}, {"--approval-source-trust", "environment"}, {"--approval-source-evidence", file, "--approval-source-trust", "file"}} {
		var out, errOut bytes.Buffer
		if code := run(t.Context(), append(append([]string{}, base...), extra...), &out, &errOut); code != 2 || out.Len() != 0 {
			t.Fatal("invalid source options accepted", code)
		}
	}
}

func TestTaskRestoreExplicitEnvironmentKeysCommand(t *testing.T) {
	dir := admissionCommandFixture(t)
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x6a}, 32))
	t.Setenv("AI_AGENT_APPROVAL_ENCRYPTION_KEY", key)
	t.Setenv("AI_AGENT_APPROVAL_ENCRYPTION_PREVIOUS_KEYS", "")
	args := []string{"assess", "--directory", dir, "--task", "admission-task", "--tenant", "a"}
	for _, mode := range []string{"unselected", "environment", "unavailable", "invalid_source"} {
		t.Run(mode, func(t *testing.T) {
			selected := append([]string{}, args...)
			if mode != "unselected" {
				selected = append(selected, "--key-source", "environment")
			}
			if mode == "invalid_source" {
				selected[len(selected)-1] = "file"
			}
			if mode == "unavailable" {
				t.Setenv("AI_AGENT_APPROVAL_ENCRYPTION_KEY", "private-invalid-secret")
			}
			var out, errOut bytes.Buffer
			code := run(t.Context(), selected, &out, &errOut)
			if mode == "invalid_source" {
				if code != 2 || out.Len() != 0 {
					t.Fatal(code)
				}
				return
			}
			var report store.TaskRestoreAdmissionReport
			if code != 3 || json.Unmarshal(out.Bytes(), &report) != nil || report.RestoreReady || report.Decision != "blocked" {
				t.Fatal(code, errOut.String())
			}
			if mode == "unselected" && report.KeyManagement != nil {
				t.Fatal("implicit secret access")
			}
			if mode == "environment" && (report.KeyManagement == nil || report.KeyManagement.Decryption != "verified" || len(report.VerifiedEvidence) != 1 || report.VerifiedEvidence[0].Kind != "key_management") {
				t.Fatal(report)
			}
			if mode == "unavailable" && (report.KeyManagement == nil || report.KeyManagement.Decryption != "unverified" || len(report.VerifiedEvidence) != 0) {
				t.Fatal(report)
			}
			for _, secret := range []string{key, "private-", "secret-task-body", dir} {
				if bytes.Contains(out.Bytes(), []byte(secret)) || bytes.Contains(errOut.Bytes(), []byte(secret)) {
					t.Fatal("secret leaked")
				}
			}
		})
	}
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
