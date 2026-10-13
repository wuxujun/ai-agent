package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/wuxujun/ai-agent/internal/policy/restoreworkspace"
)

const TaskRestoreWorkspaceSchema = "task-restore-workspace-files-v1"
const MaxTaskRestoreWorkspaceEvidenceBytes = 256 << 10

var ErrTaskRestoreWorkspaceEvidence = errors.New("invalid restore workspace evidence")

// This operator-selected baseline is separate from the untrusted dependency
// manifest. Its provenance/completeness must be reviewed externally: matching
// listed files does not prove the original source had no omitted dependencies.
type TaskRestoreWorkspaceEvidence struct {
	Schema        string                  `json:"schema"`
	TaskID        string                  `json:"task_id"`
	TenantID      string                  `json:"tenant_id"`
	PayloadSHA256 string                  `json:"payload_sha256"`
	CreatedAt     time.Time               `json:"created_at"`
	ExpiresAt     time.Time               `json:"expires_at"`
	Files         []restoreworkspace.File `json:"files"`
}

type restoreWorkspaceReader struct {
	evidence                TaskRestoreWorkspaceEvidence
	digest                  string
	directory, task, tenant string
	snapshot                restoreworkspace.Snapshot
	status                  string
}

func decodeRestoreWorkspaceEvidence(data []byte, task, tenant, hash string) (*restoreWorkspaceReader, error) {
	if len(data) > MaxTaskRestoreWorkspaceEvidenceBytes {
		return nil, ErrTaskExportTooLarge
	}
	var e TaskRestoreWorkspaceEvidence
	if strictRestoreJSON(data, &e) != nil || e.Schema != TaskRestoreWorkspaceSchema || e.TaskID != task || e.TenantID != tenant || e.PayloadSHA256 != hash || !validRestoreDigest(hash) || e.Files == nil || len(e.Files) > restoreworkspace.MaxFiles {
		return nil, ErrTaskRestoreWorkspaceEvidence
	}
	var total int64
	previous := ""
	for _, file := range e.Files {
		if !restoreworkspace.ValidPath(file.Path) || file.Path <= previous || file.Size < 0 || file.Size > restoreworkspace.MaxBytes-total || !validRestoreDigest(file.SHA256) || file.Mode > 0700 || file.Mode&0077 != 0 || file.Mode&0400 == 0 {
			return nil, ErrTaskRestoreWorkspaceEvidence
		}
		total += file.Size
		previous = file.Path
	}
	return &restoreWorkspaceReader{evidence: e, digest: restoreDigest(data), task: task, tenant: tenant}, nil
}

func (w *restoreWorkspaceReader) read(ctx context.Context) (string, error) {
	snapshot, err := restoreworkspace.Scan(ctx, w.directory, w.task, w.tenant)
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	w.snapshot = snapshot
	switch {
	case err == nil:
		w.status = "verified"
	case errors.Is(err, restoreworkspace.ErrMissing):
		w.status = "missing"
	case errors.Is(err, restoreworkspace.ErrUnsafe), errors.Is(err, restoreworkspace.ErrChanged), errors.Is(err, restoreworkspace.ErrBudget):
		w.status = "conflict"
	default:
		w.status = "unverified"
	}
	// Denials also have a stable fingerprint; a transition to readable/changed
	// data invalidates the previous target binding rather than silently passing.
	data, _ := json.Marshal(struct{ Digest, Revision, Status string }{w.digest, snapshot.Revision, w.status})
	return restoreDigest(data), nil
}

func (w *restoreWorkspaceReader) check(ctx context.Context, claim TaskRestoreEvidenceClaim, binding TaskRestoreAdmissionBinding) (TaskRestoreEvidenceObservation, error) {
	if err := ctx.Err(); err != nil {
		return TaskRestoreEvidenceObservation{}, err
	}
	observation := TaskRestoreEvidenceObservation{Binding: binding, SHA256: w.digest, Status: w.status, VerifiedAt: time.Now().UTC(), ExpiresAt: w.evidence.ExpiresAt}
	if claim.SHA256 != w.digest {
		observation.Status = "conflict"
		return observation, nil
	}
	if w.status != "verified" {
		return observation, nil
	}
	actual := make(map[string]restoreworkspace.File, len(w.snapshot.Files))
	for _, file := range w.snapshot.Files {
		actual[file.Path] = file
	}
	for _, expected := range w.evidence.Files {
		file, exists := actual[expected.Path]
		if !exists {
			observation.Status = "missing"
			return observation, nil
		}
		if file != expected {
			observation.Status = "conflict"
			return observation, nil
		}
	}
	if len(actual) != len(w.evidence.Files) {
		observation.Status = "conflict"
		return observation, nil
	}
	if !validRestoreEvidenceTime(w.evidence.CreatedAt, w.evidence.ExpiresAt, observation.VerifiedAt) {
		observation.Status = "unverified"
	}
	return observation, nil
}
