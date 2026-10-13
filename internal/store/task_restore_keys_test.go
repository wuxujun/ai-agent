package store

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/approvalcrypto"
	"github.com/wuxujun/ai-agent/internal/types"
)

type fixtureRestoreKeys struct {
	codec    *approvalcrypto.Codec
	revision string
	err      error
	calls    int
	changeAt int
}

func (p *fixtureRestoreKeys) Snapshot(ctx context.Context) (approvalcrypto.ReadOnlyKeySnapshot, error) {
	if err := ctx.Err(); err != nil {
		return approvalcrypto.ReadOnlyKeySnapshot{}, err
	}
	p.calls++
	if p.calls == p.changeAt {
		p.revision = restoreDigest([]byte("test-rotation"))
	}
	return approvalcrypto.ReadOnlyKeySnapshot{Codec: p.codec, Generation: p.revision}, p.err
}
func fixtureKeyProvider(t *testing.T) *fixtureRestoreKeys {
	t.Helper()
	codec, err := approvalcrypto.New(bytes.Repeat([]byte{0x31}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return &fixtureRestoreKeys{codec: codec, revision: restoreDigest([]byte("trusted-test-revision"))}
}
func fixtureSeal(t *testing.T, version int, plaintext string) []byte {
	t.Helper()
	key := bytes.Repeat([]byte{0x31}, 32)
	if version == 2 {
		c, err := approvalcrypto.New(key)
		if err != nil {
			t.Fatal(err)
		}
		b, err := c.Encrypt([]byte(plaintext))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	nonce := bytes.Repeat([]byte{0x41}, aead.NonceSize()) // local deterministic fixture only
	return aead.Seal(append([]byte{1}, nonce...), nonce, []byte(plaintext), nil)
}
func keyRestoreFixture(t *testing.T, version int, edit func(*types.DurableApproval), empty bool) ([]byte, string) {
	t.Helper()
	_, snap, _, err := decodeJointRestore(t.Context(), restoreFixture(t), "a", MaxTaskExportBytes)
	if err != nil {
		t.Fatal(err)
	}
	a := snap.Dependencies.Approvals[0]
	a.ActionPayload = fixtureSeal(t, version, `{"action":"write","parameters":{"large":9007199254740993,"nested":{"secret":"private-body"}}}`)
	a.ResolutionPayload = fixtureSeal(t, version, `{"approved":true,"parameters":{"large":9007199254740993}}`)
	if edit != nil {
		edit(a)
	}
	if empty {
		snap.Dependencies.Approvals = []*types.DurableApproval{}
	}
	data, err := EncodeTaskExport(t.Context(), snap, "fixture", snap.Task.ID, "a", MaxTaskExportBytes)
	if err != nil {
		t.Fatal(err)
	}
	_, dir, err := VerifyPersistentTaskRestore(t.Context(), data, "a", t.TempDir(), MaxTaskExportBytes)
	if err != nil {
		t.Fatal(err)
	}
	return data, dir
}
func TestTaskRestoreKeyManagementActualDecryptReadOnly(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(string(rune('0'+version)), func(t *testing.T) {
			data, dir := keyRestoreFixture(t, version, nil, false)
			before := bytes.Clone(data)
			path := filepath.Join(dir, taskRestoreDatabaseName)
			db, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			provider := fixtureKeyProvider(t)
			// Read old ciphertext after rotating the primary.
			provider.codec, err = approvalcrypto.NewKeyring(bytes.Repeat([]byte{0x32}, 32), bytes.Repeat([]byte{0x31}, 32))
			if err != nil {
				t.Fatal(err)
			}
			report, err := AssessPersistentTaskRestoreAdmissionWithKeys(t.Context(), dir, "restore-task", "a", nil, nil, provider)
			if err != nil || report.KeyManagement == nil || report.KeyManagement.Decryption != "verified" || report.KeyManagement.SourceBinding != "unverified" || len(report.VerifiedEvidence) != 0 || !hasAdmissionBlocker(report, "key_management", "unverified") || report.RestoreReady || report.Decision != "blocked" || !report.SourcePreserved {
				t.Fatal(report, err)
			}
			out, _ := json.Marshal(report)
			if bytes.Contains(out, []byte("private-")) || bytes.Contains(out, []byte("9007199254740993")) || bytes.Contains(out, []byte(dir)) {
				t.Fatal("sensitive report")
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(after, db) || !bytes.Equal(before, data) {
				t.Fatal("source/target modified", err)
			}
			afterEntries, err := os.ReadDir(dir)
			if err != nil || len(entries) != len(afterEntries) {
				t.Fatal("directory modified", err)
			}
		})
	}
}
func TestTaskRestoreKeyManagementNoApprovalsAndUnavailableProvider(t *testing.T) {
	_, dir := keyRestoreFixture(t, 2, nil, true)
	provider := fixtureKeyProvider(t)
	report, err := AssessPersistentTaskRestoreAdmissionWithKeys(t.Context(), dir, "restore-task", "a", nil, nil, provider)
	if err != nil || len(report.VerifiedEvidence) != 1 || report.VerifiedEvidence[0].Kind != "key_management" || report.KeyManagement.ApprovalCount != 0 || report.KeyManagement.SourceBinding != "not_applicable" || report.EvidenceComplete || report.RestoreReady {
		t.Fatal(report, err)
	}
	provider.err = errors.New("private-secret-provider-error")
	report, err = AssessPersistentTaskRestoreAdmissionWithKeys(t.Context(), dir, "restore-task", "a", nil, nil, provider)
	if err != nil || len(report.VerifiedEvidence) != 0 || !hasAdmissionBlocker(report, "key_management", "unverified") {
		t.Fatal(report, err)
	}
	out, _ := json.Marshal(report)
	if bytes.Contains(out, []byte("private-")) {
		t.Fatal("provider error leaked")
	}
}
func TestTaskRestoreKeyManagementRejections(t *testing.T) {
	for _, mode := range []string{"truncated", "unknown", "tampered", "missing_history", "action", "missing_resolution", "wrong_resolution", "resolution_null", "resolution_missing_bool", "pending_resolution", "expired_rejected", "consumed_missing", "duplicate_json", "unknown_field"} {
		t.Run(mode, func(t *testing.T) {
			_, dir := keyRestoreFixture(t, 2, func(a *types.DurableApproval) {
				switch mode {
				case "truncated":
					a.ActionPayload = []byte{2}
				case "unknown":
					a.ActionPayload[0] = 99
				case "tampered":
					a.ActionPayload[len(a.ActionPayload)-1] ^= 1
				case "action":
					a.ActionPayload = fixtureSeal(t, 2, `{"action":"other","parameters":{}}`)
				case "missing_resolution":
					a.ResolutionPayload = nil
				case "wrong_resolution":
					a.ResolutionPayload = fixtureSeal(t, 2, `{"approved":false}`)
				case "resolution_null":
					a.ResolutionPayload = fixtureSeal(t, 2, `null`)
				case "resolution_missing_bool":
					a.ResolutionPayload = fixtureSeal(t, 2, `{}`)
				case "pending_resolution":
					a.Status = types.ApprovalPending
				case "expired_rejected":
					a.Status = types.ApprovalExpired
					a.ResolutionPayload = fixtureSeal(t, 2, `{"approved":false}`)
				case "consumed_missing":
					a.Status = types.ApprovalConsumed
					a.ResolutionPayload = nil
				case "duplicate_json":
					a.ActionPayload = fixtureSeal(t, 2, `{"action":"other","action":"write"}`)
				case "unknown_field":
					a.ActionPayload = fixtureSeal(t, 2, `{"action":"write","tenant":"a"}`)
				}
			}, false)
			provider := fixtureKeyProvider(t)
			if mode == "missing_history" {
				var err error
				provider.codec, err = approvalcrypto.New(bytes.Repeat([]byte{0x32}, 32))
				if err != nil {
					t.Fatal(err)
				}
			}
			report, err := AssessPersistentTaskRestoreAdmissionWithKeys(t.Context(), dir, "restore-task", "a", nil, nil, provider)
			if err != nil || report.KeyManagement.Decryption == "verified" || len(report.VerifiedEvidence) != 0 || !hasAdmissionBlocker(report, "key_management", "conflict") {
				t.Fatal(report, err)
			}
		})
	}
}
func TestRestoreApprovalRelationshipsAndNumberPrecision(t *testing.T) {
	provider := fixtureKeyProvider(t)
	_, snap, _, err := decodeJointRestore(t.Context(), restoreFixture(t), "a", MaxTaskExportBytes)
	if err != nil {
		t.Fatal(err)
	}
	a := snap.Dependencies.Approvals[0]
	a.ActionPayload = fixtureSeal(t, 2, `{"action":"write","parameters":{"large":9007199254740993}}`)
	a.ResolutionPayload = fixtureSeal(t, 2, `{"approved":true}`)
	for _, mode := range []string{"tenant", "task", "request", "id", "version", "empty", "status"} {
		copy := *a
		switch mode {
		case "tenant":
			copy.TenantID = "b"
		case "task":
			copy.TaskID = "other"
		case "request":
			copy.Request.TaskID = "other"
		case "id":
			copy.Request.ID = "other"
		case "version":
			copy.Version = 0
		case "empty":
			copy.ActionPayload = nil
		case "status":
			copy.Status = "other"
		}
		if checkRestoreApproval(t.Context(), provider.codec, &copy, "restore-task", "a") {
			t.Fatal("relationship accepted", mode)
		}
	}
	plain, err := provider.codec.Decrypt(a.ActionPayload)
	if err != nil {
		t.Fatal(err)
	}
	var action struct {
		Action     string
		Parameters map[string]any
	}
	if err := strictRestoreJSON(plain, &action); err != nil {
		t.Fatal(err)
	}
	if n, ok := action.Parameters["large"].(json.Number); !ok || n.String() != "9007199254740993" {
		t.Fatal("integer rounded")
	}
	if !checkRestoreApproval(t.Context(), provider.codec, a, "restore-task", "a") {
		t.Fatal("valid rejected")
	}
}

type keyMutationReader struct {
	inner  *persistentRestoreAdmissionReader
	mutate func()
}

func (r *keyMutationReader) ReadState(ctx context.Context) (TaskRestoreAdmissionState, error) {
	return r.inner.ReadState(ctx)
}
func (r *keyMutationReader) CheckEvidence(ctx context.Context, claim TaskRestoreEvidenceClaim) (TaskRestoreEvidenceObservation, error) {
	o, err := r.inner.CheckEvidence(ctx, claim)
	r.mutate()
	return o, err
}
func TestTaskRestoreKeysInvalidatedDuringAssessment(t *testing.T) {
	for _, mode := range []string{"rotation", "revocation", "ciphertext"} {
		t.Run(mode, func(t *testing.T) {
			artifact, dir := keyRestoreFixture(t, 2, nil, true)
			sandbox, err := OpenPersistentTaskRestoreSandbox(t.Context(), dir)
			if err != nil {
				t.Fatal(err)
			}
			defer sandbox.Close()
			path := filepath.Join(dir, taskRestoreDatabaseName)
			identity, _ := os.Lstat(path)
			dirIdentity, _ := os.Lstat(dir)
			provider := fixtureKeyProvider(t)
			reader := &persistentRestoreAdmissionReader{sandbox: sandbox, path: path, identity: identity, dirIdentity: dirIdentity, task: "restore-task", tenant: "a", keys: &restoreKeyReader{provider: provider}}
			state, err := reader.ReadState(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			m := TaskRestoreDependencyManifest{Schema: TaskRestoreAdmissionSchema, Binding: state.Binding, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().Add(5 * time.Minute), Evidence: []TaskRestoreEvidenceClaim{{Kind: "key_management", SHA256: reader.keys.digest}}}
			mutating := &keyMutationReader{inner: reader, mutate: func() {
				if mode == "ciphertext" {
					f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
					if err != nil {
						t.Fatal(err)
					}
					_, err = f.Write([]byte("changed"))
					closeErr := f.Close()
					if err != nil || closeErr != nil {
						t.Fatal(err, closeErr)
					}
				} else {
					provider.revision = restoreDigest([]byte("trusted-new-version"))
					if mode == "revocation" {
						provider.err = errors.New("private-revocation")
					}
				}
			}}
			report, err := EvaluateTaskRestoreAdmission(t.Context(), artifact, admissionJSON(t, m), "a", MaxTaskExportBytes, mutating, nil)
			if err != nil || len(report.VerifiedEvidence) != 0 || report.EvidenceComplete || !(hasAdmissionBlocker(report, "target", "state_changed") || hasAdmissionBlocker(report, "target", "unverified")) {
				t.Fatal(report, err)
			}
		})
	}
}
func TestTaskRestoreKeysCancellationAndDuringScanRotation(t *testing.T) {
	_, dir := keyRestoreFixture(t, 2, nil, false)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := AssessPersistentTaskRestoreAdmissionWithKeys(ctx, dir, "restore-task", "a", nil, nil, fixtureKeyProvider(t)); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	provider := fixtureKeyProvider(t)
	provider.changeAt = 2
	if report, err := AssessPersistentTaskRestoreAdmissionWithKeys(t.Context(), dir, "restore-task", "a", nil, nil, provider); err == nil || report != nil || strings.Contains(err.Error(), "private") {
		t.Fatal("mid scan rotation accepted")
	}
}

func TestTaskRestoreKeysPreserveWorkspaceCombination(t *testing.T) {
	dir, baseline, root := restoreWorkspaceFixture(t)
	file := filepath.Join(root, "sub", "file.txt")
	before, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	report, err := AssessPersistentTaskRestoreAdmissionWithKeys(t.Context(), dir, "restore-task", "a", nil, baseline, fixtureKeyProvider(t))
	if err != nil || report.Binding.PolicyVersion != "read-only-workspace-keys-v1" || len(report.VerifiedEvidence) != 1 || report.VerifiedEvidence[0].Kind != "workspace" || !hasAdmissionBlocker(report, "key_management", "conflict") {
		t.Fatal(report, err)
	}
	after, err := os.ReadFile(file)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("workspace changed", err)
	}
}

func TestTaskRestoreKeysRejectRecordAndByteBudgets(t *testing.T) {
	for _, mode := range []string{"count", "bytes"} {
		t.Run(mode, func(t *testing.T) {
			sandbox, err := NewTaskRestoreSandbox(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer sandbox.Close()
			if mode == "count" {
				_, err = sandbox.db.ExecContext(t.Context(), "WITH RECURSIVE n(x) AS (SELECT 0 UNION ALL SELECT x+1 FROM n WHERE x<?) INSERT INTO restore_parts(task_id,kind,position,body) SELECT 'restore-task','approval',x,'{}' FROM n", MaxTaskExportDependencies)
			} else {
				_, err = sandbox.db.ExecContext(t.Context(), "INSERT INTO restore_parts(task_id,kind,position,body) VALUES('restore-task','approval',0,zeroblob(?))", MaxTaskExportBytes+1)
			}
			if err != nil {
				t.Fatal(err)
			}
			reader := &persistentRestoreAdmissionReader{sandbox: sandbox, task: "restore-task", tenant: "a"}
			keys := &restoreKeyReader{provider: fixtureKeyProvider(t)}
			if _, err := keys.read(t.Context(), reader); !errors.Is(err, ErrTaskRestoreMismatch) {
				t.Fatal("unbounded record read", err)
			}
		})
	}
}

func TestTaskRestoreKeysExpiredManifestAndOldProviderRevision(t *testing.T) {
	_, dir := keyRestoreFixture(t, 2, nil, true)
	provider := fixtureKeyProvider(t)
	initial, err := AssessPersistentTaskRestoreAdmissionWithKeys(t.Context(), dir, "restore-task", "a", nil, nil, provider)
	if err != nil {
		t.Fatal(err)
	}
	m := TaskRestoreDependencyManifest{Schema: TaskRestoreAdmissionSchema, Binding: *initial.Binding, CreatedAt: time.Now().Add(-2 * time.Minute), ExpiresAt: time.Now().Add(-time.Minute), Evidence: initial.VerifiedEvidence}
	report, err := AssessPersistentTaskRestoreAdmissionWithKeys(t.Context(), dir, "restore-task", "a", admissionJSON(t, m), nil, provider)
	if err != nil || len(report.VerifiedEvidence) != 0 || !hasAdmissionBlocker(report, "manifest", "expired_or_invalid_time") || report.KeyManagement.Decryption != "unverified" {
		t.Fatal(report, err)
	}
	m.CreatedAt, m.ExpiresAt = time.Now().UTC(), time.Now().Add(5*time.Minute)
	provider.revision = restoreDigest([]byte("new-trusted-provider-revision"))
	report, err = AssessPersistentTaskRestoreAdmissionWithKeys(t.Context(), dir, "restore-task", "a", admissionJSON(t, m), nil, provider)
	if err != nil || len(report.VerifiedEvidence) != 0 || !hasAdmissionBlocker(report, "target", "state_binding_mismatch") {
		t.Fatal(report, err)
	}
}

func TestRestoreApprovalResolutionStateContract(t *testing.T) {
	provider := fixtureKeyProvider(t)
	for _, fixture := range []struct {
		status     types.DurableApprovalStatus
		resolution string
		valid      bool
	}{
		{types.ApprovalPending, "", true}, {types.ApprovalExpired, "", true},
		{types.ApprovalExpired, `{"approved":true}`, true}, {types.ApprovalRejected, `{"approved":false}`, true},
		{types.ApprovalConsumed, `{"approved":true,"parameters":{}}`, true}, {types.ApprovalConsumed, `{"approved":false}`, true},
		{types.ApprovalRejected, `{"approved":true}`, false}, {types.ApprovalPending, `{"approved":true}`, false},
	} {
		a := &types.DurableApproval{ID: "approval", TaskID: "restore-task", TenantID: "a", Version: 1, Status: fixture.status, Request: types.ApprovalRequest{ID: "approval", TaskID: "restore-task", Action: "write", RiskLevel: types.RiskLevelHigh}, ActionPayload: fixtureSeal(t, 2, `{"action":"write","parameters":{}}`)}
		if fixture.resolution != "" {
			a.ResolutionPayload = fixtureSeal(t, 2, fixture.resolution)
		}
		if checkRestoreApproval(t.Context(), provider.codec, a, "restore-task", "a") != fixture.valid {
			t.Fatal("wrong state contract", fixture.status)
		}
	}
}
