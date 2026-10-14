package store

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"time"
)

const TaskRestoreDependencyAuthoritySchema = "task-restore-dependency-authority-v1"
const MaxTaskRestoreDependencyAuthorityBytes = MaxTaskRestoreObservationBytes + (16 << 10)
const taskRestoreDependencyAuthorityDomain = "ai-agent/task-restore-dependency-authority/v1\n"

var ErrTaskRestoreDependencyAuthority = errors.New("restore dependency authority unavailable")

// This wire envelope contains no public key or connection information. Evidence
// is untrusted even when the reader was independently selected by trusted code.
type TaskRestoreDependencyAuthorityContent struct {
	Schema      string          `json:"schema"`
	Kind        string          `json:"kind"`
	KeyID       string          `json:"key_id"`
	Observation json.RawMessage `json:"observation"`
}
type TaskRestoreDependencyAuthorityEvidence struct {
	Content   TaskRestoreDependencyAuthorityContent `json:"content"`
	Signature []byte                                `json:"signature"`
}

// Pins must come from an independently authenticated current control plane, not
// from the evidence envelope. ObservationSHA256 binds the entire normalized
// observation, including historical source completeness and current isolation.
// Generation must change for rotation, revocation, permissions and every
// relevant observation change, including delete/recreate and ABA transitions.
// A signature alone proves neither completeness nor that the snapshot is current.
// Providers must honor cancellation and return owned immutable key bytes.
type TaskRestoreDependencyAuthorityTrustSnapshot struct {
	PublicKey                                                        ed25519.PublicKey
	KeyID, Kind, TaskID, TenantID, PayloadSHA256, SourceID, TargetID string
	ObservationSHA256, Revision, TrustRevision, Generation           string
	Status                                                           string
	CreatedAt, ExpiresAt                                             time.Time
}
type TaskRestoreDependencyAuthorityTrustProvider interface {
	ReadDependencyAuthorityTrust(context.Context, TaskRestoreDependencyRequest, string) (TaskRestoreDependencyAuthorityTrustSnapshot, error)
}
type TaskRestoreDependencyAuthorityEvidenceReader interface {
	ReadDependencyAuthorityEvidence(context.Context, TaskRestoreDependencyRequest, string) ([]byte, error)
}

// This adapter performs authentication only. It supplies no default network,
// environment trust or local-Store authority. Actual providers must enforce
// their read-only identity, URL policy, output bounds and context cancellation.
type TaskRestoreDependencyAuthorityReader struct {
	evidence TaskRestoreDependencyAuthorityEvidenceReader
	trust    TaskRestoreDependencyAuthorityTrustProvider
}

func NewTaskRestoreDependencyAuthorityReader(evidence TaskRestoreDependencyAuthorityEvidenceReader, trust TaskRestoreDependencyAuthorityTrustProvider) (*TaskRestoreDependencyAuthorityReader, error) {
	if evidence == nil || trust == nil {
		return nil, ErrTaskRestoreDependencyAuthority
	}
	return &TaskRestoreDependencyAuthorityReader{evidence: evidence, trust: trust}, nil
}

func normalizeDependencyAuthorityObservation(kind string, data []byte) (json.RawMessage, TaskRestoreObservationScope, error) {
	if len(data) > MaxTaskRestoreObservationBytes {
		return nil, TaskRestoreObservationScope{}, ErrTaskRestoreDependencyAuthority
	}
	var scope TaskRestoreObservationScope
	var normalized []byte
	var err error
	switch kind {
	case "session_dependencies":
		var o TaskRestoreSessionObservation
		if strictRestoreJSON(data, &o) != nil || o.Schema != TaskRestoreSessionObservationSchema || len(o.Source.Members) > MaxTaskRestoreObservationTasks || len(o.Target.Members) > MaxTaskRestoreObservationTasks || len(o.Source.Edges) > MaxTaskRestoreObservationEdges || len(o.Target.Edges) > MaxTaskRestoreObservationEdges {
			return nil, scope, ErrTaskRestoreDependencyAuthority
		}
		scope = o.Scope
		normalized, err = json.Marshal(o)
	case "execution_leases":
		var o TaskRestoreExecutionObservation
		if strictRestoreJSON(data, &o) != nil || o.Schema != TaskRestoreExecutionObservationSchema {
			return nil, scope, ErrTaskRestoreDependencyAuthority
		}
		scope = o.Scope
		normalized, err = json.Marshal(o)
	default:
		return nil, scope, ErrTaskRestoreDependencyAuthority
	}
	if err != nil || len(normalized) > MaxTaskRestoreObservationBytes || !validObservationID(scope.TaskID) || !validObservationID(scope.TenantID) || !validRestoreDigest(scope.PayloadSHA256) || !validRestoreDigest(scope.SourceID) || !validRestoreDigest(scope.TargetID) || !validObservationToken(scope.Revision) || !validObservationToken(scope.TrustRevision) || scope.CreatedAt.IsZero() || !scope.ExpiresAt.After(scope.CreatedAt) || scope.ExpiresAt.Sub(scope.CreatedAt) > maxTaskRestoreEvidenceAge {
		return nil, scope, ErrTaskRestoreDependencyAuthority
	}
	return normalized, scope, nil
}

// Exact signing contract: domain separator + encoding/json of this fixed typed
// content with its observation decoded and re-encoded using the S01 types.
// Signers must independently observe authoritative records, never endorse a
// caller's self-consistent artifact. This function only defines signed bytes.
func TaskRestoreDependencyAuthoritySigningBytes(content TaskRestoreDependencyAuthorityContent) ([]byte, error) {
	if content.Schema != TaskRestoreDependencyAuthoritySchema || !validObservationToken(content.KeyID) {
		return nil, ErrTaskRestoreDependencyAuthority
	}
	normalized, _, err := normalizeDependencyAuthorityObservation(content.Kind, content.Observation)
	if err != nil {
		return nil, err
	}
	content.Observation = normalized
	data, err := json.Marshal(content)
	if err != nil || len(data)+len(taskRestoreDependencyAuthorityDomain) > MaxTaskRestoreDependencyAuthorityBytes {
		return nil, ErrTaskRestoreDependencyAuthority
	}
	return append([]byte(taskRestoreDependencyAuthorityDomain), data...), nil
}

func cloneDependencyAuthorityPin(p TaskRestoreDependencyAuthorityTrustSnapshot) TaskRestoreDependencyAuthorityTrustSnapshot {
	p.PublicKey = bytes.Clone(p.PublicKey)
	return p
}
func sameDependencyAuthorityPin(a, b TaskRestoreDependencyAuthorityTrustSnapshot) bool {
	// Include every field, including the independently supplied key. Typed JSON
	// compares wall-clock instants without private monotonic clock components.
	first, firstErr := json.Marshal(a)
	second, secondErr := json.Marshal(b)
	return firstErr == nil && secondErr == nil && bytes.Equal(first, second)
}
func validDependencyAuthorityPin(p TaskRestoreDependencyAuthorityTrustSnapshot, q TaskRestoreDependencyRequest, kind string, now time.Time) bool {
	return p.Status == "active" && p.Kind == kind && p.TaskID == q.TaskID && p.TenantID == q.TenantID && p.PayloadSHA256 == q.PayloadSHA256 && p.SourceID == q.SourceID && p.TargetID == q.TargetID &&
		validObservationToken(p.KeyID) && len(p.PublicKey) == ed25519.PublicKeySize && validRestoreDigest(p.ObservationSHA256) && validObservationToken(p.Revision) && validObservationToken(p.TrustRevision) && validRestoreDigest(p.Generation) && validRestoreEvidenceTime(p.CreatedAt, p.ExpiresAt, now)
}
func validDependencyAuthorityRequest(q TaskRestoreDependencyRequest) bool {
	return validObservationID(q.TaskID) && validObservationID(q.TenantID) && validRestoreDigest(q.PayloadSHA256) && validRestoreDigest(q.SourceID) && validRestoreDigest(q.TargetID) &&
		validRestoreDigest(q.TaskSHA256) && validRestoreDigest(q.CheckpointSHA256) && q.MaxBytes >= 4096 && q.MaxBytes <= MaxTaskRestoreObservationBytes && q.MaxTasks >= 1 && q.MaxTasks <= MaxTaskRestoreObservationTasks && q.MaxEdges >= 0 && q.MaxEdges <= MaxTaskRestoreObservationEdges
}

func (r *TaskRestoreDependencyAuthorityReader) read(ctx context.Context, q TaskRestoreDependencyRequest, kind string) (result []byte, err error) {
	// Provider errors may contain secrets. Only caller cancellation is returned.
	defer func() {
		if ctx.Err() != nil {
			result = nil
			err = ctx.Err()
		} else if err != nil {
			result = nil
			err = ErrTaskRestoreDependencyAuthority
		}
	}()
	if !validDependencyAuthorityRequest(q) {
		return nil, ErrTaskRestoreDependencyAuthority
	}
	child, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	now := time.Now().UTC()
	pin, err := r.trust.ReadDependencyAuthorityTrust(child, q, kind)
	pinReadAt := time.Now().UTC()
	if err != nil || child.Err() != nil || pinReadAt.Before(now) || !validDependencyAuthorityPin(pin, q, kind, pinReadAt) {
		return nil, ErrTaskRestoreDependencyAuthority
	}
	pin = cloneDependencyAuthorityPin(pin)
	data, err := r.evidence.ReadDependencyAuthorityEvidence(child, q, kind)
	proofReadAt := time.Now().UTC()
	if err != nil || child.Err() != nil || proofReadAt.Before(pinReadAt) || len(data) > MaxTaskRestoreDependencyAuthorityBytes {
		return nil, ErrTaskRestoreDependencyAuthority
	}
	data = bytes.Clone(data)
	var envelope TaskRestoreDependencyAuthorityEvidence
	if strictRestoreJSON(data, &envelope) != nil || envelope.Content.Schema != TaskRestoreDependencyAuthoritySchema || envelope.Content.Kind != kind || envelope.Content.KeyID != pin.KeyID || len(envelope.Signature) != ed25519.SignatureSize {
		return nil, ErrTaskRestoreDependencyAuthority
	}
	normalized, scope, err := normalizeDependencyAuthorityObservation(kind, envelope.Content.Observation)
	if err != nil || len(normalized) > q.MaxBytes || !validObservationScope(scope, q, proofReadAt) || restoreDigest(normalized) != pin.ObservationSHA256 || scope.Revision != pin.Revision || scope.TrustRevision != pin.TrustRevision {
		return nil, ErrTaskRestoreDependencyAuthority
	}
	signingBytes, err := TaskRestoreDependencyAuthoritySigningBytes(envelope.Content)
	if err != nil || !ed25519.Verify(pin.PublicKey, signingBytes, envelope.Signature) {
		return nil, ErrTaskRestoreDependencyAuthority
	}
	after, err := r.trust.ReadDependencyAuthorityTrust(child, q, kind)
	finished := time.Now().UTC()
	if err != nil || child.Err() != nil || finished.Before(proofReadAt) || !validDependencyAuthorityPin(after, q, kind, finished) || !validRestoreEvidenceTime(scope.CreatedAt, scope.ExpiresAt, finished) {
		return nil, ErrTaskRestoreDependencyAuthority
	}
	after = cloneDependencyAuthorityPin(after)
	if !sameDependencyAuthorityPin(pin, after) {
		return nil, ErrTaskRestoreDependencyAuthority
	}
	// Include authenticated control-plane generation/key/permissions in the S01
	// state revision even if the signed record content stayed byte-for-byte equal.
	keyRevision, _ := json.Marshal(struct{ KeyID, KeySHA256, Generation, TrustRevision string }{pin.KeyID, restoreDigest(pin.PublicKey), pin.Generation, pin.TrustRevision})
	scope.TrustRevision = restoreDigest(keyRevision)
	if pin.CreatedAt.After(scope.CreatedAt) {
		scope.CreatedAt = pin.CreatedAt
	}
	if pin.ExpiresAt.Before(scope.ExpiresAt) {
		scope.ExpiresAt = pin.ExpiresAt
	}
	switch kind {
	case "session_dependencies":
		var o TaskRestoreSessionObservation
		if strictRestoreJSON(normalized, &o) != nil {
			return nil, ErrTaskRestoreDependencyAuthority
		}
		o.Scope = scope
		if validateSessionObservation(o, q, finished) != "verified" {
			return nil, ErrTaskRestoreDependencyAuthority
		}
		result, err = json.Marshal(o)
	case "execution_leases":
		var o TaskRestoreExecutionObservation
		if strictRestoreJSON(normalized, &o) != nil {
			return nil, ErrTaskRestoreDependencyAuthority
		}
		if o.IsolationExpiresAt.Before(scope.ExpiresAt) {
			scope.ExpiresAt = o.IsolationExpiresAt
		}
		o.Scope = scope
		if validateExecutionObservation(o, q, finished) != "verified" {
			return nil, ErrTaskRestoreDependencyAuthority
		}
		result, err = json.Marshal(o)
	default:
		return nil, ErrTaskRestoreDependencyAuthority
	}
	returnedAt := time.Now().UTC()
	if err != nil || child.Err() != nil || returnedAt.Before(finished) || !validRestoreEvidenceTime(scope.CreatedAt, scope.ExpiresAt, returnedAt) || len(result) > q.MaxBytes {
		return nil, ErrTaskRestoreDependencyAuthority
	}
	return result, nil
}
func (r *TaskRestoreDependencyAuthorityReader) ReadSessionDependencies(ctx context.Context, q TaskRestoreDependencyRequest) ([]byte, error) {
	return r.read(ctx, q, "session_dependencies")
}
func (r *TaskRestoreDependencyAuthorityReader) ReadExecutionObservation(ctx context.Context, q TaskRestoreDependencyRequest) ([]byte, error) {
	return r.read(ctx, q, "execution_leases")
}
