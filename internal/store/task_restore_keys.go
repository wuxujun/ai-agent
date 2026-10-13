package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/wuxujun/ai-agent/internal/approvalcrypto"
	"github.com/wuxujun/ai-agent/internal/types"
)

const TaskRestoreKeyManagementSchema = "task-restore-key-management-v1"
const TaskRestoreKeyManagementSourceSchema = "task-restore-key-management-v2"

// A generated diagnostic, never an operator assertion of key availability.
// Decryption verifies format and outer associations only. Provenance requires
// a separately signed source proof checked against independently pinned trust;
// existing ciphertext has no authenticated tenant/task/request binding.
type TaskRestoreKeyManagementSummary struct {
	Schema        string `json:"schema"`
	ApprovalCount int    `json:"approval_count"`
	Decryption    string `json:"decryption"`
	SourceBinding string `json:"source_binding"`
	Reason        string `json:"reason"`
}

type restoreKeyReader struct {
	provider       approvalcrypto.ReadOnlyKeyProvider
	source         *restoreApprovalSourceReader
	summary        TaskRestoreKeyManagementSummary
	digest, status string
	checkedAt      time.Time
	expiresAt      time.Time
}

func (k *restoreKeyReader) read(ctx context.Context, r *persistentRestoreAdmissionReader) (string, error) {
	k.summary = TaskRestoreKeyManagementSummary{Schema: TaskRestoreKeyManagementSchema, Decryption: "unverified", SourceBinding: "unverified", Reason: "key_provider_unavailable"}
	k.status = "unverified"
	snapshot, keyErr := k.provider.Snapshot(ctx)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	// Never accept an unversioned provider, including on an unavailable result.
	if !validRestoreDigest(snapshot.Generation) {
		return "", approvalcrypto.ErrReadOnlyKeys
	}
	var sourcePin TaskRestoreApprovalSourceTrustSnapshot
	sourceStatus := ""
	if k.source != nil {
		k.summary.Schema = TaskRestoreKeyManagementSourceSchema
		var err error
		sourcePin, sourceStatus, err = k.source.read(ctx)
		if err != nil {
			return "", err
		}
	}
	tx, err := r.sandbox.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return "", ErrTaskRestoreMismatch
	}
	defer tx.Rollback()
	var size, count int
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(SUM(length(body)),0),COUNT(*) FROM restore_parts WHERE task_id=? AND kind='approval'", r.task).Scan(&size, &count); err != nil || size > MaxTaskExportBytes || count > MaxTaskExportDependencies {
		return "", ErrTaskRestoreMismatch
	}
	k.summary.ApprovalCount = count
	sourceMatches := k.source != nil && len(k.source.evidence.Content.Approvals) == count
	if keyErr == nil && snapshot.Codec != nil {
		k.summary.Decryption = "verified"
		k.summary.Reason = "ciphertext_source_unverified"
		rows, err := tx.QueryContext(ctx, "SELECT position,body FROM restore_parts WHERE task_id=? AND kind='approval' ORDER BY position", r.task)
		if err != nil {
			return "", ErrTaskRestoreMismatch
		}
		seen, remaining := 0, MaxTaskExportBytes
		for rows.Next() {
			if err := ctx.Err(); err != nil {
				rows.Close()
				return "", err
			}
			var position int
			var body []byte
			if err := rows.Scan(&position, &body); err != nil {
				rows.Close()
				return "", ErrTaskRestoreMismatch
			}
			remaining -= len(body)
			if position != seen || remaining < 0 || seen >= count {
				rows.Close()
				return "", ErrTaskRestoreMismatch
			}
			seen++
			var a types.DurableApproval
			if strictRestoreJSON(body, &a) != nil || !checkRestoreApproval(ctx, snapshot.Codec, &a, r.task, r.tenant) {
				k.summary.Decryption = "unverified"
				k.summary.Reason = "payload_or_relationship_invalid"
				k.status = "conflict"
			}
			if k.source != nil && !k.source.matches(&a) {
				sourceMatches = false
			}
		}
		rowsErr := rows.Err()
		rows.Close()
		if rowsErr != nil || seen != count {
			return "", ErrTaskRestoreMismatch
		}
		if count == 0 {
			k.summary.SourceBinding = "not_applicable"
			k.summary.Reason = "no_approvals"
			k.status = "verified"
		}
	}
	if k.source != nil {
		// A selected proof cannot be waived, including for an empty database.
		k.summary.SourceBinding = "unverified"
		switch {
		case sourceStatus == "conflict" || !sourceMatches:
			k.status = "conflict"
			k.summary.Reason = "source_evidence_conflict"
		case sourceStatus != "verified":
			if k.status != "conflict" {
				k.status = "unverified"
			}
			k.summary.Reason = "source_evidence_unverified"
		case k.summary.Decryption == "verified":
			k.summary.SourceBinding = "verified"
			k.status = "verified"
			k.summary.Reason = "source_and_decryption_verified"
		}
	}
	if err := tx.Commit(); err != nil {
		return "", ErrTaskRestoreMismatch
	}
	// Detect a provider change during the scan, before publishing the revision.
	final, finalErr := k.provider.Snapshot(ctx)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if final.Generation != snapshot.Generation || (finalErr == nil) != (keyErr == nil) || (final.Codec == nil) != (snapshot.Codec == nil) {
		return "", ErrTaskRestoreMismatch
	}
	sourceRevision := ""
	if k.source != nil {
		finalPin, finalStatus, err := k.source.read(ctx)
		if err != nil {
			return "", err
		}
		if !sameRestoreApprovalSourceTrust(sourcePin, finalPin) || finalStatus != sourceStatus {
			return "", ErrTaskRestoreMismatch
		}
		sourceRevision = restoreDigest([]byte(sourcePin.Generation + ":" + k.source.digest + ":" + sourceStatus))
	}
	k.checkedAt = time.Now().UTC()
	k.expiresAt = k.checkedAt.Add(5 * time.Minute)
	if k.source != nil && k.source.evidence.Content.ExpiresAt.Before(k.expiresAt) {
		k.expiresAt = k.source.evidence.Content.ExpiresAt
	}
	data, _ := json.Marshal(struct {
		Summary            TaskRestoreKeyManagementSummary
		Generation, Status string
		SourceRevision     string `json:",omitempty"`
	}{k.summary, snapshot.Generation, k.status, sourceRevision})
	k.digest = restoreDigest(data)
	return k.digest, nil
}

func checkRestoreApproval(ctx context.Context, codec *approvalcrypto.Codec, a *types.DurableApproval, task, tenant string) bool {
	if ctx.Err() != nil || a.TaskID != task || a.TenantID != tenant || a.Version < 1 || a.Request.ID != a.ID || a.Request.TaskID != task || a.ID == "" || a.Request.Action == "" || a.Request.RiskLevel != types.RiskLevelHigh {
		return false
	}
	plain, err := codec.Decrypt(a.ActionPayload)
	if err != nil {
		return false
	}
	var action struct {
		Action     string         `json:"action"`
		Parameters map[string]any `json:"parameters"`
	}
	if strictRestoreJSON(plain, &action) != nil || action.Action != a.Request.Action {
		return false
	}
	// UseNumber in strictRestoreJSON keeps integers in both payloads exact.
	// Request.Parameters is a redacted preview, not the execution arguments.
	required := false
	switch a.Status {
	case types.ApprovalPending:
		return len(a.ResolutionPayload) == 0
	case types.ApprovalApproved, types.ApprovalRejected, types.ApprovalConsumed:
		required = true
	case types.ApprovalExpired:
		// Pending may expire without a resolution; approved may expire with one.
	default:
		return false
	}
	if len(a.ResolutionPayload) == 0 {
		return !required
	}
	plain, err = codec.Decrypt(a.ResolutionPayload)
	if err != nil {
		return false
	}
	var resolution struct {
		Approved   *bool          `json:"approved"`
		Message    string         `json:"message,omitempty"`
		Parameters map[string]any `json:"parameters,omitempty"`
		ActorID    string         `json:"actor_id,omitempty"`
	}
	if strictRestoreJSON(plain, &resolution) != nil || resolution.Approved == nil {
		return false
	}
	if a.Status == types.ApprovalApproved && !*resolution.Approved || a.Status == types.ApprovalRejected && *resolution.Approved || a.Status == types.ApprovalExpired && !*resolution.Approved {
		return false
	}
	// Consumed may originate from approved or rejected; it is never permission
	// for replay. Neither outcome proves effects or current authorization.
	return ctx.Err() == nil
}

func (k *restoreKeyReader) check(claim TaskRestoreEvidenceClaim, binding TaskRestoreAdmissionBinding) TaskRestoreEvidenceObservation {
	status := k.status
	if claim.SHA256 != k.digest {
		status = "conflict"
	}
	return TaskRestoreEvidenceObservation{Binding: binding, SHA256: k.digest, Status: status, VerifiedAt: k.checkedAt, ExpiresAt: k.expiresAt}
}
