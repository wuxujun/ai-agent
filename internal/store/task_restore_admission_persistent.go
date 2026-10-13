package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/wuxujun/ai-agent/internal/approvalcrypto"
)

const maxTaskRestoreAdmissionDatabaseBytes = 64 << 20

// The offline reader only proves the current isolated database's integrity.
// Optional trusted readers check workspace and keys. It has no executor, audit
// client, online Store or authority to restore approvals or execute effects.
type persistentRestoreAdmissionReader struct {
	sandbox      *TaskRestoreSandbox
	path         string
	identity     os.FileInfo
	dirIdentity  os.FileInfo
	task, tenant string
	workspace    *restoreWorkspaceReader
	keys         *restoreKeyReader
}

func (r *persistentRestoreAdmissionReader) fingerprint(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	directory := filepath.Dir(r.path)
	canonical, err := filepath.EvalSymlinks(directory)
	if err != nil || canonical != directory {
		return "", ErrTaskRestoreTarget
	}
	dirInfo, err := os.Lstat(directory)
	if err != nil || !os.SameFile(r.dirIdentity, dirInfo) || !dirInfo.IsDir() || (runtime.GOOS != "windows" && dirInfo.Mode().Perm()&0077 != 0) {
		return "", ErrTaskRestoreTarget
	}
	info, err := os.Lstat(r.path)
	if err != nil || !info.Mode().IsRegular() || !os.SameFile(r.identity, info) || (runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0) {
		return "", ErrTaskRestoreTarget
	}
	file, err := os.Open(r.path)
	if err != nil {
		return "", ErrTaskRestoreTarget
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || opened.Size() > maxTaskRestoreAdmissionDatabaseBytes {
		return "", ErrTaskRestoreTarget
	}
	hash := sha256.New()
	buffer := make([]byte, 32<<10)
	total := 0
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, err := file.Read(buffer)
		total += n
		if total > maxTaskRestoreAdmissionDatabaseBytes {
			return "", ErrTaskRestoreTarget
		}
		hash.Write(buffer[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", ErrTaskRestoreTarget
		}
	}
	after, err := os.Lstat(r.path)
	if err != nil || !os.SameFile(info, after) || opened.Size() != after.Size() || !opened.ModTime().Equal(after.ModTime()) {
		return "", ErrTaskRestoreTarget
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func (r *persistentRestoreAdmissionReader) ReadState(ctx context.Context) (TaskRestoreAdmissionState, error) {
	before, err := r.fingerprint(ctx)
	if err != nil {
		return TaskRestoreAdmissionState{}, err
	}
	report, err := r.sandbox.Verify(ctx, r.task, r.tenant)
	if err != nil {
		return TaskRestoreAdmissionState{}, err
	}
	after, err := r.fingerprint(ctx)
	if err != nil {
		return TaskRestoreAdmissionState{}, err
	}
	if before != after {
		return TaskRestoreAdmissionState{}, ErrTaskRestoreMismatch
	}
	revision := after
	policyVersion := "read-only-admission-v1"
	if r.workspace != nil {
		workspaceRevision, err := r.workspace.read(ctx)
		if err != nil {
			return TaskRestoreAdmissionState{}, err
		}
		revision = restoreDigest([]byte(after + ":" + workspaceRevision))
		policyVersion = "read-only-workspace-v1"
	}
	if r.keys != nil {
		keyRevision, err := r.keys.read(ctx, r)
		if err != nil {
			return TaskRestoreAdmissionState{}, err
		}
		revision = restoreDigest([]byte(revision + ":" + keyRevision))
		policyVersion = "read-only-keys-v1"
		if r.workspace != nil {
			policyVersion = "read-only-workspace-keys-v1"
		}
		if r.keys.source != nil {
			policyVersion = "read-only-source-keys-v1"
			if r.workspace != nil {
				policyVersion = "read-only-workspace-source-keys-v1"
			}
		}
	}
	// Cover mutations during dependency scans, not just sandbox.Verify.
	final, err := r.fingerprint(ctx)
	if err != nil {
		return TaskRestoreAdmissionState{}, err
	}
	if final != after {
		return TaskRestoreAdmissionState{}, ErrTaskRestoreMismatch
	}
	return TaskRestoreAdmissionState{SandboxVerified: report.Verified && report.Reopened, Binding: TaskRestoreAdmissionBinding{
		TaskID: r.task, TenantID: r.tenant, PayloadSHA256: report.PayloadSHA256,
		TargetID: restoreDigest([]byte(filepath.Dir(r.path))), StateRevision: revision,
		TargetVersion: "isolated-sqlite-v1", RuntimeVersion: "task-restore-diagnostic-v1", PolicyVersion: policyVersion,
	}}, nil
}

func (r *persistentRestoreAdmissionReader) CheckEvidence(ctx context.Context, claim TaskRestoreEvidenceClaim) (TaskRestoreEvidenceObservation, error) {
	if claim.Kind == "key_management" && r.keys != nil {
		state, err := r.ReadState(ctx)
		if err != nil {
			return TaskRestoreEvidenceObservation{}, err
		}
		return r.keys.check(claim, state.Binding), nil
	}
	if claim.Kind != "workspace" || r.workspace == nil {
		return TaskRestoreEvidenceObservation{}, nil
	}
	state, err := r.ReadState(ctx)
	if err != nil {
		return TaskRestoreEvidenceObservation{}, err
	}
	return r.workspace.check(ctx, claim, state.Binding)
}

// AssessPersistentTaskRestoreAdmission only opens an existing dedicated sandbox
// read-only. A nil manifest generates an empty current-state checklist, so the
// caller can discover the binding and missing categories without any writes.
// Supplied evidence digests remain unverified by this offline-only adapter.
func AssessPersistentTaskRestoreAdmission(ctx context.Context, directory, task, tenant string, manifestData []byte) (*TaskRestoreAdmissionReport, error) {
	return AssessPersistentTaskRestoreAdmissionWithWorkspace(ctx, directory, task, tenant, manifestData, nil)
}

// Workspace evidence must be independently selected by the operator, never
// extracted from the uploaded dependency manifest. Only the fixed isolated
// tenant/task root is read. Remaining evidence categories remain unverified.
func AssessPersistentTaskRestoreAdmissionWithWorkspace(ctx context.Context, directory, task, tenant string, manifestData, workspaceData []byte) (*TaskRestoreAdmissionReport, error) {
	return AssessPersistentTaskRestoreAdmissionWithKeys(ctx, directory, task, tenant, manifestData, workspaceData, nil)
}

// The provider is selected explicitly by trusted code, never by uploaded JSON.
// A nil provider preserves the database/workspace-only diagnostic behavior.
func AssessPersistentTaskRestoreAdmissionWithKeys(ctx context.Context, directory, task, tenant string, manifestData, workspaceData []byte, provider approvalcrypto.ReadOnlyKeyProvider) (*TaskRestoreAdmissionReport, error) {
	return AssessPersistentTaskRestoreAdmissionWithOptions(ctx, directory, task, tenant, manifestData, TaskRestoreAdmissionOptions{WorkspaceEvidence: workspaceData, KeyProvider: provider})
}

type TaskRestoreAdmissionOptions struct {
	WorkspaceEvidence      []byte
	KeyProvider            approvalcrypto.ReadOnlyKeyProvider
	ApprovalSourceEvidence []byte
	ApprovalSourceTrust    TaskRestoreApprovalSourceTrustProvider
}

// Only trusted application code selects providers. Signature evidence is
// untrusted input; neither a public key nor source pins are accepted from it.
func AssessPersistentTaskRestoreAdmissionWithOptions(ctx context.Context, directory, task, tenant string, manifestData []byte, options TaskRestoreAdmissionOptions) (*TaskRestoreAdmissionReport, error) {
	workspaceData, provider := options.WorkspaceEvidence, options.KeyProvider
	if len(options.ApprovalSourceEvidence) > MaxTaskRestoreApprovalSourceBytes {
		return nil, ErrTaskExportTooLarge
	}
	if (options.ApprovalSourceEvidence != nil) != (options.ApprovalSourceTrust != nil) || (options.ApprovalSourceTrust != nil && provider == nil) {
		return nil, ErrTaskRestoreApprovalSource
	}
	if len(workspaceData) > MaxTaskRestoreWorkspaceEvidenceBytes {
		return nil, ErrTaskExportTooLarge
	}
	if len(manifestData) > MaxTaskRestoreManifestBytes {
		return nil, ErrTaskExportTooLarge
	}
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return nil, ErrTaskRestoreTarget
	}
	path := filepath.Join(absolute, taskRestoreDatabaseName)
	identity, err := os.Lstat(path)
	if err != nil || !identity.Mode().IsRegular() || identity.Size() > maxTaskRestoreAdmissionDatabaseBytes {
		return nil, ErrTaskRestoreTarget
	}
	dirIdentity, err := os.Lstat(absolute)
	if err != nil {
		return nil, ErrTaskRestoreTarget
	}
	sandbox, err := OpenPersistentTaskRestoreSandbox(ctx, directory)
	if err != nil {
		return nil, err
	}
	defer sandbox.Close()
	reader := &persistentRestoreAdmissionReader{sandbox: sandbox, path: path, identity: identity, dirIdentity: dirIdentity, task: task, tenant: tenant}
	state, err := reader.ReadState(ctx)
	if err != nil {
		return nil, err
	}
	if workspaceData != nil {
		workspace, err := decodeRestoreWorkspaceEvidence(workspaceData, task, tenant, state.Binding.PayloadSHA256)
		if err != nil {
			return nil, err
		}
		workspace.directory = absolute
		reader.workspace = workspace
		state, err = reader.ReadState(ctx)
		if err != nil {
			return nil, err
		}
	}
	if provider != nil {
		reader.keys = &restoreKeyReader{provider: provider}
		if options.ApprovalSourceTrust != nil {
			source, err := decodeRestoreApprovalSource(options.ApprovalSourceEvidence, options.ApprovalSourceTrust, task, tenant, state.Binding.PayloadSHA256)
			if err != nil {
				return nil, err
			}
			reader.keys.source = source
		}
		state, err = reader.ReadState(ctx)
		if err != nil {
			return nil, err
		}
	}
	// Keep the size check and blob read in the same SQLite snapshot.
	tx, err := sandbox.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var size int
	if err := tx.QueryRowContext(ctx, "SELECT length(artifact) FROM restore_runs WHERE task_id=? AND tenant_id=?", task, tenant).Scan(&size); err != nil {
		return nil, ErrTaskRestoreMismatch
	}
	if size <= 0 || size > MaxTaskExportBytes {
		return nil, ErrTaskRestoreMismatch
	}
	var artifact []byte
	if err := tx.QueryRowContext(ctx, "SELECT artifact FROM restore_runs WHERE task_id=? AND tenant_id=?", task, tenant).Scan(&artifact); err != nil {
		return nil, ErrTaskRestoreMismatch
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if manifestData == nil {
		now := time.Now().UTC()
		manifestData, err = json.Marshal(TaskRestoreDependencyManifest{Schema: TaskRestoreAdmissionSchema, Binding: state.Binding, CreatedAt: now, ExpiresAt: now.Add(5 * time.Minute), Evidence: func() []TaskRestoreEvidenceClaim {
			claims := []TaskRestoreEvidenceClaim{}
			if reader.workspace != nil {
				claims = append(claims, TaskRestoreEvidenceClaim{Kind: "workspace", SHA256: reader.workspace.digest})
			}
			if reader.keys != nil {
				claims = append(claims, TaskRestoreEvidenceClaim{Kind: "key_management", SHA256: reader.keys.digest})
			}
			return claims
		}()})
		if err != nil {
			return nil, err
		}
	}
	report, err := EvaluateTaskRestoreAdmission(ctx, artifact, manifestData, tenant, MaxTaskExportBytes, reader, nil)
	if err == nil && reader.keys != nil {
		summary := reader.keys.summary
		// A summary is current verified data only when target binding stayed valid.
		for _, b := range report.Blockers {
			if b.Kind == "target" || b.Kind == "manifest" || b.Kind == "assessment" || (b.Kind == "key_management" && (b.Code == "expired_or_invalid_time" || b.Code == "expired_during_assessment")) {
				summary.Decryption, summary.SourceBinding = "unverified", "unverified"
				summary.Reason = "stale_or_unbound_evidence"
			}
		}
		report.KeyManagement = &summary
	}
	return report, err
}
