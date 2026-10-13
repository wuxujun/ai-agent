package store

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/wuxujun/ai-agent/internal/types"
)

const TaskRestoreApprovalSourceSchema = "task-restore-approval-source-v1"
const MaxTaskRestoreApprovalSourceBytes = 1 << 20
const taskRestoreApprovalSourceDomain = "ai-agent/task-restore-approval-source/v1\n"

var ErrTaskRestoreApprovalSource = errors.New("invalid restore approval source evidence")
var ErrTaskRestoreApprovalSourceTrust = errors.New("restore approval source trust unavailable")

// These pins are provisioned independently of uploaded artifacts and evidence.
// SnapshotID must come from the authoritative source/control plane, not the
// proof being checked. Generation is opaque lowercase hex, not a key hash.
type TaskRestoreApprovalSourceTrustSnapshot struct {
	PublicKey                                  ed25519.PublicKey `json:"-"`
	SourceID, TenantID, SnapshotID, Generation string
}

// A trusted implementation must bound reads, honor cancellation, and change
// Generation for key rotation, source snapshot updates, scope or revocation.
// Implementations must not return mutable key slices shared with callers.
// Errors may be secret and must never be emitted in diagnostic reports.
type TaskRestoreApprovalSourceTrustProvider interface {
	Snapshot(context.Context) (TaskRestoreApprovalSourceTrustSnapshot, error)
}

type TaskRestoreApprovalSourceRecord struct {
	ApprovalID       string                      `json:"approval_id"`
	TaskID           string                      `json:"task_id"`
	TenantID         string                      `json:"tenant_id"`
	RequestID        string                      `json:"request_id"`
	Version          int64                       `json:"version"`
	Status           types.DurableApprovalStatus `json:"status"`
	Action           string                      `json:"action"`
	ActionSHA256     string                      `json:"action_sha256"`
	ResolutionSHA256 string                      `json:"resolution_sha256"`
	RecordSHA256     string                      `json:"record_sha256"`
}

// Approvals is the complete source list, sorted by approval ID; an empty list
// is signed absence. The authority must read its source independently, never
// endorse an uploaded artifact merely because it is self-consistent.
type TaskRestoreApprovalSourceContent struct {
	Schema        string                            `json:"schema"`
	SourceID      string                            `json:"source_id"`
	SnapshotID    string                            `json:"snapshot_id"`
	TenantID      string                            `json:"tenant_id"`
	TaskID        string                            `json:"task_id"`
	PayloadSHA256 string                            `json:"payload_sha256"`
	CreatedAt     time.Time                         `json:"created_at"`
	ExpiresAt     time.Time                         `json:"expires_at"`
	Approvals     []TaskRestoreApprovalSourceRecord `json:"approvals"`
}

type TaskRestoreApprovalSourceEvidence struct {
	Content   TaskRestoreApprovalSourceContent `json:"content"`
	Signature []byte                           `json:"signature"`
}

func validApprovalSourceID(s string) bool {
	if len(s) == 0 || len(s) > 256 {
		return false
	}
	for _, c := range s {
		if c < 33 || c > 126 {
			return false
		}
	}
	return true
}

// TaskRestoreApprovalSourceSigningBytes is the exact signed wire contract:
// domain separator followed by encoding/json of this fixed typed structure.
// Arbitrary JSON canonicalizers are not interchangeable with this format.
// This function neither obtains source records nor grants them provenance.
func TaskRestoreApprovalSourceSigningBytes(c TaskRestoreApprovalSourceContent) ([]byte, error) {
	if c.Schema != TaskRestoreApprovalSourceSchema || !validApprovalSourceID(c.SourceID) || !validApprovalSourceID(c.SnapshotID) || c.TaskID == "" || len(c.TaskID) > 256 || c.TenantID == "" || len(c.TenantID) > 256 || !validRestoreDigest(c.PayloadSHA256) || c.Approvals == nil || len(c.Approvals) > MaxTaskExportDependencies || c.CreatedAt.IsZero() || !c.ExpiresAt.After(c.CreatedAt) || c.ExpiresAt.Sub(c.CreatedAt) > maxTaskRestoreEvidenceAge {
		return nil, ErrTaskRestoreApprovalSource
	}
	previous := ""
	for _, a := range c.Approvals {
		if a.ApprovalID <= previous || len(a.ApprovalID) > 256 || a.TaskID != c.TaskID || a.TenantID != c.TenantID || a.RequestID != a.ApprovalID || a.Version < 1 || a.Action == "" || len(a.Action) > 256 || !validRestoreDigest(a.ActionSHA256) || !validRestoreDigest(a.ResolutionSHA256) || !validRestoreDigest(a.RecordSHA256) {
			return nil, ErrTaskRestoreApprovalSource
		}
		switch a.Status {
		case types.ApprovalPending, types.ApprovalApproved, types.ApprovalRejected, types.ApprovalExpired, types.ApprovalConsumed:
		default:
			return nil, ErrTaskRestoreApprovalSource
		}
		previous = a.ApprovalID
	}
	b, err := json.Marshal(c)
	if err != nil || len(b)+len(taskRestoreApprovalSourceDomain) > MaxTaskRestoreApprovalSourceBytes {
		return nil, ErrTaskRestoreApprovalSource
	}
	return append([]byte(taskRestoreApprovalSourceDomain), b...), nil
}

// RecordSHA256 uses canonicalRestoreJSON on encoding/json(DurableApproval),
// preserving JSON numbers. It binds preview, timestamps, owner and lease as
// well as identity, status, version and both ciphertexts. Nothing is decrypted.
func TaskRestoreApprovalSourceRecordFor(a *types.DurableApproval) (TaskRestoreApprovalSourceRecord, error) {
	if a == nil {
		return TaskRestoreApprovalSourceRecord{}, ErrTaskRestoreApprovalSource
	}
	b, err := json.Marshal(a)
	if err != nil || len(b) > MaxTaskExportBytes {
		return TaskRestoreApprovalSourceRecord{}, ErrTaskRestoreApprovalSource
	}
	b, err = canonicalRestoreJSON(b)
	if err != nil {
		return TaskRestoreApprovalSourceRecord{}, ErrTaskRestoreApprovalSource
	}
	return TaskRestoreApprovalSourceRecord{ApprovalID: a.ID, TaskID: a.TaskID, TenantID: a.TenantID, RequestID: a.Request.ID, Version: a.Version, Status: a.Status, Action: a.Request.Action, ActionSHA256: restoreDigest(a.ActionPayload), ResolutionSHA256: restoreDigest(a.ResolutionPayload), RecordSHA256: restoreDigest(b)}, nil
}

type restoreApprovalSourceReader struct {
	provider     TaskRestoreApprovalSourceTrustProvider
	evidence     TaskRestoreApprovalSourceEvidence
	signingBytes []byte
	digest       string
}

func decodeRestoreApprovalSource(data []byte, provider TaskRestoreApprovalSourceTrustProvider, task, tenant, payload string) (*restoreApprovalSourceReader, error) {
	if len(data) > MaxTaskRestoreApprovalSourceBytes {
		return nil, ErrTaskExportTooLarge
	}
	var e TaskRestoreApprovalSourceEvidence
	if strictRestoreJSON(data, &e) != nil || len(e.Signature) != ed25519.SignatureSize || e.Content.TaskID != task || e.Content.TenantID != tenant || e.Content.PayloadSHA256 != payload {
		return nil, ErrTaskRestoreApprovalSource
	}
	b, err := TaskRestoreApprovalSourceSigningBytes(e.Content)
	if err != nil {
		return nil, err
	}
	return &restoreApprovalSourceReader{provider: provider, evidence: e, signingBytes: b, digest: restoreDigest(data)}, nil
}

func (s *restoreApprovalSourceReader) read(ctx context.Context) (TaskRestoreApprovalSourceTrustSnapshot, string, error) {
	pin, err := s.provider.Snapshot(ctx)
	if ctx.Err() != nil {
		return pin, "", ctx.Err()
	}
	if !validRestoreDigest(pin.Generation) {
		return pin, "", ErrTaskRestoreApprovalSourceTrust
	}
	if err != nil || len(pin.PublicKey) != ed25519.PublicKeySize || !validApprovalSourceID(pin.SourceID) || !validApprovalSourceID(pin.SnapshotID) || pin.TenantID == "" || len(pin.TenantID) > 256 {
		return pin, "unverified", nil
	}
	c := s.evidence.Content
	if pin.SourceID != c.SourceID || pin.SnapshotID != c.SnapshotID || pin.TenantID != c.TenantID || !ed25519.Verify(pin.PublicKey, s.signingBytes, s.evidence.Signature) {
		return pin, "conflict", nil
	}
	if !validRestoreEvidenceTime(c.CreatedAt, c.ExpiresAt, time.Now().UTC()) {
		return pin, "unverified", nil
	}
	return pin, "verified", nil
}

func sameRestoreApprovalSourceTrust(a, b TaskRestoreApprovalSourceTrustSnapshot) bool {
	return a.Generation == b.Generation && a.SourceID == b.SourceID && a.TenantID == b.TenantID && a.SnapshotID == b.SnapshotID && bytes.Equal(a.PublicKey, b.PublicKey)
}

func (s *restoreApprovalSourceReader) matches(a *types.DurableApproval) bool {
	if a == nil {
		return false
	}
	list := s.evidence.Content.Approvals
	i := sort.Search(len(list), func(i int) bool { return list[i].ApprovalID >= a.ID })
	if i == len(list) || list[i].ApprovalID != a.ID {
		return false
	}
	actual, err := TaskRestoreApprovalSourceRecordFor(a)
	return err == nil && actual == list[i]
}
