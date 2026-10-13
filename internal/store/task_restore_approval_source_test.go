package store

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/policy/restoreworkspace"
	"github.com/wuxujun/ai-agent/internal/types"
)

type fixtureSourceTrust struct {
	pin             TaskRestoreApprovalSourceTrustSnapshot
	err             error
	calls, changeAt int
	cancel          context.CancelFunc
}

func (p *fixtureSourceTrust) Snapshot(ctx context.Context) (TaskRestoreApprovalSourceTrustSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return TaskRestoreApprovalSourceTrustSnapshot{}, err
	}
	p.calls++
	if p.calls == p.changeAt {
		p.pin.Generation = restoreDigest([]byte("source-revoked"))
		p.err = errors.New("private-trust-revoked")
	}
	if p.cancel != nil {
		p.cancel()
	}
	copy := p.pin
	copy.PublicKey = bytes.Clone(p.pin.PublicKey)
	return copy, p.err
}

func sourceProofJSON(t *testing.T, e TaskRestoreApprovalSourceEvidence) []byte {
	t.Helper()
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func signSourceProof(t *testing.T, c TaskRestoreApprovalSourceContent, key ed25519.PrivateKey) TaskRestoreApprovalSourceEvidence {
	t.Helper()
	b, err := TaskRestoreApprovalSourceSigningBytes(c)
	if err != nil {
		t.Fatal(err)
	}
	return TaskRestoreApprovalSourceEvidence{Content: c, Signature: ed25519.Sign(key, b)}
}
func sourceFixture(t *testing.T, artifact []byte) (TaskRestoreApprovalSourceEvidence, *fixtureSourceTrust, ed25519.PrivateKey) {
	t.Helper()
	a, snap, _, err := decodeJointRestore(t.Context(), artifact, "a", MaxTaskExportBytes)
	if err != nil {
		t.Fatal(err)
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	c := TaskRestoreApprovalSourceContent{Schema: TaskRestoreApprovalSourceSchema, SourceID: "private-authority", SnapshotID: "private-snapshot", TaskID: snap.Task.ID, TenantID: "a", PayloadSHA256: a.PayloadSHA256, CreatedAt: time.Now().UTC().Add(-time.Second), ExpiresAt: time.Now().UTC().Add(5 * time.Minute), Approvals: []TaskRestoreApprovalSourceRecord{}}
	for _, a := range snap.Dependencies.Approvals {
		record, err := TaskRestoreApprovalSourceRecordFor(a)
		if err != nil {
			t.Fatal(err)
		}
		c.Approvals = append(c.Approvals, record)
	}
	trust := &fixtureSourceTrust{pin: TaskRestoreApprovalSourceTrustSnapshot{PublicKey: public, SourceID: c.SourceID, SnapshotID: c.SnapshotID, TenantID: "a", Generation: restoreDigest([]byte("trusted-source-generation"))}}
	return signSourceProof(t, c, private), trust, private
}
func sourceOptions(t *testing.T, e TaskRestoreApprovalSourceEvidence, p TaskRestoreApprovalSourceTrustProvider) TaskRestoreAdmissionOptions {
	t.Helper()
	return TaskRestoreAdmissionOptions{KeyProvider: fixtureKeyProvider(t), ApprovalSourceEvidence: sourceProofJSON(t, e), ApprovalSourceTrust: p}
}

func TestTaskRestoreApprovalSourceVerifiedReadOnly(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(string(rune('0'+version)), func(t *testing.T) {
			artifact, dir := keyRestoreFixture(t, version, nil, false)
			proof, trust, _ := sourceFixture(t, artifact)
			dbPath := filepath.Join(dir, taskRestoreDatabaseName)
			before, err := os.ReadFile(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			input := sourceProofJSON(t, proof)
			original := bytes.Clone(input)
			source := bytes.Clone(artifact)
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			opts := sourceOptions(t, proof, trust)
			opts.ApprovalSourceEvidence = input
			report, err := AssessPersistentTaskRestoreAdmissionWithOptions(t.Context(), dir, "restore-task", "a", nil, opts)
			if err != nil || report.KeyManagement == nil || report.KeyManagement.Schema != "task-restore-key-management-v2" || report.KeyManagement.SourceBinding != "verified" || report.KeyManagement.Decryption != "verified" || len(report.VerifiedEvidence) != 1 || report.VerifiedEvidence[0].Kind != "key_management" || report.Binding.PolicyVersion != "read-only-source-keys-v1" || report.EvidenceComplete || report.RestoreReady || report.Decision != "blocked" || !report.SourcePreserved {
				t.Fatal(report, err)
			}
			if hasAdmissionBlocker(report, "key_management", "unverified") || !hasAdmissionBlocker(report, "approvals_and_effects", "missing") {
				t.Fatal(report)
			}
			out, _ := json.Marshal(report)
			for _, secret := range []string{"private-", "9007199254740993", base64.StdEncoding.EncodeToString(trust.pin.PublicKey), dir} {
				if bytes.Contains(out, []byte(secret)) {
					t.Fatal("source detail leaked")
				}
			}
			after, err := os.ReadFile(dbPath)
			if err != nil || !bytes.Equal(before, after) || !bytes.Equal(input, original) || !bytes.Equal(source, artifact) {
				t.Fatal("input/target changed", err)
			}
			afterEntries, err := os.ReadDir(dir)
			if err != nil || len(entries) != len(afterEntries) {
				t.Fatal("directory changed", err)
			}
		})
	}
}

func TestTaskRestoreApprovalSourceRejectsForgeryScopeAndRecords(t *testing.T) {
	for _, mode := range []string{"signature", "wrong_key", "source", "snapshot", "tenant_pin", "unavailable", "action_ciphertext", "resolution_ciphertext", "record", "request_action", "version", "status", "missing", "extra", "expired", "future", "missing_key"} {
		t.Run(mode, func(t *testing.T) {
			artifact, dir := keyRestoreFixture(t, 2, nil, false)
			e, p, private := sourceFixture(t, artifact)
			resign := false
			expected := "conflict"
			switch mode {
			case "signature":
				e.Signature[0] ^= 1
			case "wrong_key":
				p.pin.PublicKey, _, _ = ed25519.GenerateKey(rand.Reader)
			case "source":
				p.pin.SourceID = "other-authority"
			case "snapshot":
				p.pin.SnapshotID = "other-snapshot"
			case "tenant_pin":
				p.pin.TenantID = "b"
			case "unavailable":
				p.err = errors.New("private-unavailable")
				expected = "unverified"
			case "action_ciphertext":
				e.Content.Approvals[0].ActionSHA256 = restoreDigest([]byte("swapped"))
				resign = true
			case "resolution_ciphertext":
				e.Content.Approvals[0].ResolutionSHA256 = restoreDigest([]byte("swapped"))
				resign = true
			case "record":
				e.Content.Approvals[0].RecordSHA256 = restoreDigest([]byte("swapped"))
				resign = true
			case "request_action":
				e.Content.Approvals[0].Action = "other"
				resign = true
			case "version":
				e.Content.Approvals[0].Version++
				resign = true
			case "status":
				e.Content.Approvals[0].Status = "consumed"
				resign = true
			case "missing":
				e.Content.Approvals = []TaskRestoreApprovalSourceRecord{}
				resign = true
			case "extra":
				copy := e.Content.Approvals[0]
				copy.ApprovalID = "zz-extra"
				copy.RequestID = copy.ApprovalID
				e.Content.Approvals = append(e.Content.Approvals, copy)
				resign = true
			case "expired":
				e.Content.CreatedAt = time.Now().Add(-2 * time.Minute)
				e.Content.ExpiresAt = time.Now().Add(-time.Minute)
				resign = true
				expected = "unverified"
			case "future":
				e.Content.CreatedAt = time.Now().Add(time.Minute)
				e.Content.ExpiresAt = time.Now().Add(2 * time.Minute)
				resign = true
				expected = "unverified"
			case "missing_key":
				expected = "unverified"
			}
			if resign {
				e = signSourceProof(t, e.Content, private)
			}
			opts := sourceOptions(t, e, p)
			if mode == "missing_key" {
				key := fixtureKeyProvider(t)
				key.err = errors.New("private-key-error")
				opts.KeyProvider = key
			}
			report, err := AssessPersistentTaskRestoreAdmissionWithOptions(t.Context(), dir, "restore-task", "a", nil, opts)
			if err != nil || report.KeyManagement.SourceBinding == "verified" || len(report.VerifiedEvidence) != 0 || !hasAdmissionBlocker(report, "key_management", expected) {
				t.Fatal(report, err)
			}
			out, _ := json.Marshal(report)
			if bytes.Contains(out, []byte("private-")) {
				t.Fatal("source error leaked")
			}
		})
	}
}

func TestTaskRestoreApprovalSourceStrictInputAndOptions(t *testing.T) {
	artifact, dir := keyRestoreFixture(t, 2, nil, false)
	e, p, private := sourceFixture(t, artifact)
	for _, mode := range []string{"tenant", "task", "payload", "request", "duplicate", "null", "schema", "budget", "signature_length", "unknown", "duplicate_json", "trailing", "self_key", "no_trust", "no_proof", "no_keys"} {
		t.Run(mode, func(t *testing.T) {
			copy := e
			copy.Content.Approvals = append([]TaskRestoreApprovalSourceRecord{}, e.Content.Approvals...)
			switch mode {
			case "tenant":
				copy.Content.TenantID = "b"
				copy.Content.Approvals[0].TenantID = "b"
				copy = signSourceProof(t, copy.Content, private)
			case "task":
				copy.Content.TaskID = "other"
				copy.Content.Approvals[0].TaskID = "other"
				copy = signSourceProof(t, copy.Content, private)
			case "payload":
				copy.Content.PayloadSHA256 = restoreDigest(nil)
				copy = signSourceProof(t, copy.Content, private)
			case "request":
				copy.Content.Approvals[0].RequestID = "other"
			case "duplicate":
				copy.Content.Approvals = append(copy.Content.Approvals, copy.Content.Approvals[0])
			case "null":
				copy.Content.Approvals = nil
			case "schema":
				copy.Content.Schema = "unknown"
			case "signature_length":
				copy.Signature = []byte{1}
			}
			opts := sourceOptions(t, copy, p)
			switch mode {
			case "budget":
				opts.ApprovalSourceEvidence = bytes.Repeat([]byte("x"), MaxTaskRestoreApprovalSourceBytes+1)
			case "unknown":
				opts.ApprovalSourceEvidence = []byte(strings.Replace(string(opts.ApprovalSourceEvidence), `"content":`, `"unknown":true,"content":`, 1))
			case "duplicate_json":
				opts.ApprovalSourceEvidence = []byte(strings.Replace(string(opts.ApprovalSourceEvidence), `"schema":`, `"schema":"unknown","schema":`, 1))
			case "trailing":
				opts.ApprovalSourceEvidence = append(opts.ApprovalSourceEvidence, []byte(` {}`)...)
			case "self_key":
				opts.ApprovalSourceEvidence = []byte(strings.Replace(string(opts.ApprovalSourceEvidence), `"content":`, `"public_key":"self","content":`, 1))
			case "no_trust":
				opts.ApprovalSourceTrust = nil
			case "no_proof":
				opts.ApprovalSourceEvidence = nil
			case "no_keys":
				opts.KeyProvider = nil
			}
			if report, err := AssessPersistentTaskRestoreAdmissionWithOptions(t.Context(), dir, "restore-task", "a", nil, opts); err == nil || report != nil {
				t.Fatal("invalid source accepted", mode)
			}
		})
	}
}

func TestTaskRestoreApprovalSourceAbsenceDoesNotWaiveSelectedProof(t *testing.T) {
	artifact, dir := keyRestoreFixture(t, 2, nil, true)
	e, p, _ := sourceFixture(t, artifact)
	report, err := AssessPersistentTaskRestoreAdmissionWithOptions(t.Context(), dir, "restore-task", "a", nil, sourceOptions(t, e, p))
	if err != nil || len(report.VerifiedEvidence) != 1 || report.KeyManagement.SourceBinding != "verified" {
		t.Fatal(report, err)
	}
	e.Signature[0] ^= 1
	report, err = AssessPersistentTaskRestoreAdmissionWithOptions(t.Context(), dir, "restore-task", "a", nil, sourceOptions(t, e, p))
	if err != nil || len(report.VerifiedEvidence) != 0 || !hasAdmissionBlocker(report, "key_management", "conflict") {
		t.Fatal("forged absence waived", report, err)
	}
}

func TestTaskRestoreApprovalSourceTrustChangeInvalidatesEvidence(t *testing.T) {
	for _, mode := range []string{"rotation", "revocation", "snapshot", "expiry", "during_scan", "cancellation"} {
		t.Run(mode, func(t *testing.T) {
			artifact, dir := keyRestoreFixture(t, 2, nil, false)
			e, p, private := sourceFixture(t, artifact)
			if mode == "during_scan" {
				p.changeAt = 2
				if report, err := AssessPersistentTaskRestoreAdmissionWithOptions(t.Context(), dir, "restore-task", "a", nil, sourceOptions(t, e, p)); err == nil || report != nil {
					t.Fatal("scan change accepted")
				}
				return
			}
			if mode == "cancellation" {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				p.cancel = cancel
				if _, err := AssessPersistentTaskRestoreAdmissionWithOptions(ctx, dir, "restore-task", "a", nil, sourceOptions(t, e, p)); !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
				return
			}
			sandbox, err := OpenPersistentTaskRestoreSandbox(t.Context(), dir)
			if err != nil {
				t.Fatal(err)
			}
			defer sandbox.Close()
			path := filepath.Join(dir, taskRestoreDatabaseName)
			identity, _ := os.Lstat(path)
			dirIdentity, _ := os.Lstat(dir)
			source, err := decodeRestoreApprovalSource(sourceProofJSON(t, e), p, "restore-task", "a", e.Content.PayloadSHA256)
			if err != nil {
				t.Fatal(err)
			}
			reader := &persistentRestoreAdmissionReader{sandbox: sandbox, path: path, identity: identity, dirIdentity: dirIdentity, task: "restore-task", tenant: "a", keys: &restoreKeyReader{provider: fixtureKeyProvider(t), source: source}}
			state, err := reader.ReadState(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			m := TaskRestoreDependencyManifest{Schema: TaskRestoreAdmissionSchema, Binding: state.Binding, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().Add(5 * time.Minute), Evidence: []TaskRestoreEvidenceClaim{{Kind: "key_management", SHA256: reader.keys.digest}}}
			mutating := &keyMutationReader{inner: reader, mutate: func() {
				p.pin.Generation = restoreDigest([]byte("trusted-source-update"))
				switch mode {
				case "rotation":
					p.pin.PublicKey, _, _ = ed25519.GenerateKey(rand.Reader)
				case "revocation":
					p.err = errors.New("private-revocation")
				case "snapshot":
					p.pin.SnapshotID = "different-snapshot"
				case "expiry":
					e.Content.CreatedAt = time.Now().Add(-2 * time.Minute)
					e.Content.ExpiresAt = time.Now().Add(-time.Minute)
					changed, err := decodeRestoreApprovalSource(sourceProofJSON(t, signSourceProof(t, e.Content, private)), p, "restore-task", "a", e.Content.PayloadSHA256)
					if err != nil {
						t.Fatal(err)
					}
					reader.keys.source = changed
				}
			}}
			report, err := EvaluateTaskRestoreAdmission(t.Context(), artifact, admissionJSON(t, m), "a", MaxTaskExportBytes, mutating, nil)
			if err != nil || len(report.VerifiedEvidence) != 0 || !hasAdmissionBlocker(report, "target", "state_changed") {
				t.Fatal("stale source retained", report, err)
			}
		})
	}
}

func TestEnvironmentApprovalSourceTrustRevisionAndInvalidInput(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AI_AGENT_RESTORE_APPROVAL_SOURCE_PUBLIC_KEY", base64.StdEncoding.EncodeToString(pub))
	t.Setenv("AI_AGENT_RESTORE_APPROVAL_SOURCE_ID", "private-authority")
	t.Setenv("AI_AGENT_RESTORE_APPROVAL_SOURCE_TENANT", "a")
	t.Setenv("AI_AGENT_RESTORE_APPROVAL_SOURCE_SNAPSHOT", "private-snapshot")
	p := NewEnvironmentTaskRestoreApprovalSourceTrust()
	first, err := p.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// Public key arrays belong to the snapshot, not provider state.
	first.PublicKey[0] ^= 1
	second, err := p.Snapshot(t.Context())
	if err != nil || second.Generation != first.Generation || !bytes.Equal(second.PublicKey, pub) {
		t.Fatal("unstable/shared trust")
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			s, err := p.Snapshot(t.Context())
			if err != nil || s.Generation != second.Generation {
				t.Error("concurrent trust changed")
			}
		})
	}
	wg.Wait()
	for _, env := range []string{"AI_AGENT_RESTORE_APPROVAL_SOURCE_PUBLIC_KEY", "AI_AGENT_RESTORE_APPROVAL_SOURCE_ID", "AI_AGENT_RESTORE_APPROVAL_SOURCE_TENANT", "AI_AGENT_RESTORE_APPROVAL_SOURCE_SNAPSHOT"} {
		t.Run(env, func(t *testing.T) {
			t.Setenv(env, "")
			s, err := p.Snapshot(t.Context())
			if !errors.Is(err, ErrTaskRestoreApprovalSourceTrust) || s.Generation == second.Generation || !validRestoreDigest(s.Generation) {
				t.Fatal("revocation not observed", err)
			}
		})
	}
	t.Setenv("AI_AGENT_RESTORE_APPROVAL_SOURCE_PUBLIC_KEY", "private-invalid")
	if _, err := p.Snapshot(t.Context()); !errors.Is(err, ErrTaskRestoreApprovalSourceTrust) || strings.Contains(err.Error(), "private") {
		t.Fatal("invalid public key accepted/leaked")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := p.Snapshot(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestTaskRestoreApprovalSourceCiphertextSwapCannotBeResignedByUploader(t *testing.T) {
	for _, field := range []string{"action", "resolution", "preview"} {
		t.Run(field, func(t *testing.T) {
			artifact, _ := keyRestoreFixture(t, 2, nil, false)
			e, p, _ := sourceFixture(t, artifact)
			_, snap, _, err := decodeJointRestore(t.Context(), artifact, "a", MaxTaskExportBytes)
			if err != nil {
				t.Fatal(err)
			}
			a := snap.Dependencies.Approvals[0]
			switch field {
			case "action":
				a.ActionPayload = fixtureSeal(t, 2, `{"action":"write","parameters":{"different":true}}`)
			case "resolution":
				a.ResolutionPayload = fixtureSeal(t, 2, `{"approved":true,"parameters":{"different":true}}`)
			case "preview":
				a.Request.Parameters["large"] = json.Number("9007199254740992")
			}
			malicious, err := EncodeTaskExport(t.Context(), snap, "fixture", "restore-task", "a", MaxTaskExportBytes)
			if err != nil {
				t.Fatal(err)
			}
			_, dir, err := VerifyPersistentTaskRestore(t.Context(), malicious, "a", t.TempDir(), MaxTaskExportBytes)
			if err != nil {
				t.Fatal(err)
			}
			if report, err := AssessPersistentTaskRestoreAdmissionWithOptions(t.Context(), dir, "restore-task", "a", nil, sourceOptions(t, e, p)); err == nil || report != nil {
				t.Fatal("old source proof accepted new package")
			}
			// Attacker can recompute every ordinary hash but has no authority key.
			joint, _, _, err := decodeJointRestore(t.Context(), malicious, "a", MaxTaskExportBytes)
			if err != nil {
				t.Fatal(err)
			}
			e.Content.PayloadSHA256 = joint.PayloadSHA256
			e.Content.Approvals[0], err = TaskRestoreApprovalSourceRecordFor(a)
			if err != nil {
				t.Fatal(err)
			}
			report, err := AssessPersistentTaskRestoreAdmissionWithOptions(t.Context(), dir, "restore-task", "a", nil, sourceOptions(t, e, p))
			if err != nil || report.KeyManagement.Decryption != "verified" || report.KeyManagement.SourceBinding == "verified" || len(report.VerifiedEvidence) != 0 || !hasAdmissionBlocker(report, "key_management", "conflict") {
				t.Fatal("self-consistent swap granted provenance", report, err)
			}
		})
	}
}

func TestTaskRestoreApprovalSourceObservationExpiresDuringEvaluation(t *testing.T) {
	artifact, dir := keyRestoreFixture(t, 2, nil, false)
	e, p, private := sourceFixture(t, artifact)
	now := time.Now().UTC()
	e.Content.CreatedAt = now.Add(-time.Second)
	e.Content.ExpiresAt = now.Add(time.Minute)
	e = signSourceProof(t, e.Content, private)
	sandbox, err := OpenPersistentTaskRestoreSandbox(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer sandbox.Close()
	path := filepath.Join(dir, taskRestoreDatabaseName)
	identity, _ := os.Lstat(path)
	dirIdentity, _ := os.Lstat(dir)
	source, err := decodeRestoreApprovalSource(sourceProofJSON(t, e), p, "restore-task", "a", e.Content.PayloadSHA256)
	if err != nil {
		t.Fatal(err)
	}
	reader := &persistentRestoreAdmissionReader{sandbox: sandbox, path: path, identity: identity, dirIdentity: dirIdentity, task: "restore-task", tenant: "a", keys: &restoreKeyReader{provider: fixtureKeyProvider(t), source: source}}
	state, err := reader.ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	m := TaskRestoreDependencyManifest{Schema: TaskRestoreAdmissionSchema, Binding: state.Binding, CreatedAt: now, ExpiresAt: now.Add(5 * time.Minute), Evidence: []TaskRestoreEvidenceClaim{{Kind: "key_management", SHA256: reader.keys.digest}}}
	// Initial/observation times are real; the final trusted clock advances past
	// source expiry while the dependency manifest is still valid.
	calls := 0
	clock := func() time.Time {
		calls++
		if calls >= 3 {
			return now.Add(2 * time.Minute)
		}
		return time.Now().UTC()
	}
	report, err := EvaluateTaskRestoreAdmission(t.Context(), artifact, admissionJSON(t, m), "a", MaxTaskExportBytes, reader, clock)
	if err != nil || len(report.VerifiedEvidence) != 0 || !hasAdmissionBlocker(report, "key_management", "expired_during_assessment") {
		t.Fatal("source expiry outlived observation", report, err)
	}
}

func TestTaskRestoreApprovalSourceBoundsAndRecordNumbers(t *testing.T) {
	artifact, _ := keyRestoreFixture(t, 2, nil, false)
	e, _, _ := sourceFixture(t, artifact)
	c := e.Content
	c.Approvals = make([]TaskRestoreApprovalSourceRecord, MaxTaskExportDependencies+1)
	if _, err := TaskRestoreApprovalSourceSigningBytes(c); err == nil {
		t.Fatal("unbounded records accepted")
	}
	_, snap, _, err := decodeJointRestore(t.Context(), artifact, "a", MaxTaskExportBytes)
	if err != nil {
		t.Fatal(err)
	}
	a := snap.Dependencies.Approvals[0]
	before, err := TaskRestoreApprovalSourceRecordFor(a)
	if err != nil {
		t.Fatal(err)
	}
	a.Request.Parameters["large"] = json.Number("9007199254740992")
	after, err := TaskRestoreApprovalSourceRecordFor(a)
	if err != nil || before.RecordSHA256 == after.RecordSHA256 {
		t.Fatal("rounded source record hash", err)
	}
	for _, edit := range []func(*types.DurableApproval){func(a *types.DurableApproval) { a.Owner = "other" }, func(a *types.DurableApproval) { a.LeaseExpiresAt = a.LeaseExpiresAt.Add(time.Second) }, func(a *types.DurableApproval) { a.Version = 9007199254740993 }} {
		copy := *a
		edit(&copy)
		changed, err := TaskRestoreApprovalSourceRecordFor(&copy)
		if err != nil || changed.RecordSHA256 == after.RecordSHA256 {
			t.Fatal("unbound full record", err)
		}
	}
}

func TestTaskRestoreApprovalSourceCombinesWorkspaceState(t *testing.T) {
	artifact, dir := keyRestoreFixture(t, 2, nil, false)
	e, p, _ := sourceFixture(t, artifact)
	root := filepath.Join(dir, restoreworkspace.RelativeRoot("restore-task", "a"))
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	content := []byte("private-file-body")
	path := filepath.Join(root, "file.txt")
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	w := TaskRestoreWorkspaceEvidence{Schema: TaskRestoreWorkspaceSchema, TaskID: "restore-task", TenantID: "a", PayloadSHA256: e.Content.PayloadSHA256, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().Add(5 * time.Minute), Files: []restoreworkspace.File{{Path: "file.txt", Size: int64(len(content)), Mode: 0600, SHA256: restoreDigest(content)}}}
	opts := sourceOptions(t, e, p)
	opts.WorkspaceEvidence = workspaceEvidenceJSON(t, w)
	report, err := AssessPersistentTaskRestoreAdmissionWithOptions(t.Context(), dir, "restore-task", "a", nil, opts)
	if err != nil || len(report.VerifiedEvidence) != 2 || report.VerifiedEvidence[0].Kind != "workspace" || report.VerifiedEvidence[1].Kind != "key_management" || report.Binding.PolicyVersion != "read-only-workspace-source-keys-v1" || report.RestoreReady || report.EvidenceComplete {
		t.Fatal(report, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, content) {
		t.Fatal("workspace modified", err)
	}
}
