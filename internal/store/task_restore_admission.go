package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"
)

const TaskRestoreAdmissionSchema = "task-restore-dependencies-v1"
const MaxTaskRestoreManifestBytes = 64 << 10
const maxTaskRestoreEvidenceAge = 15 * time.Minute

var ErrTaskRestoreManifest = errors.New("invalid restore dependency manifest")

var restoreAdmissionKinds = [...]string{
	"workspace", "key_management", "external_audit", "session_dependencies",
	"execution_leases", "approvals_and_effects", "memory_index", "target_compatibility", "isolated_drill",
}

// Bind every observation to the same immutable export and current target state.
// TargetID and StateRevision are digests, never paths or connection strings.
type TaskRestoreAdmissionBinding struct {
	TaskID         string `json:"task_id"`
	TenantID       string `json:"tenant_id"`
	PayloadSHA256  string `json:"payload_sha256"`
	TargetID       string `json:"target_id"`
	TargetVersion  string `json:"target_version"`
	RuntimeVersion string `json:"runtime_version"`
	PolicyVersion  string `json:"policy_version"`
	StateRevision  string `json:"state_revision"`
}

type TaskRestoreEvidenceClaim struct {
	Kind   string `json:"kind"`
	SHA256 string `json:"sha256"`
}

// This is an untrusted list of evidence digests. It contains no keys, paths,
// approvals, evidence bodies, or assertions of verification success.
type TaskRestoreDependencyManifest struct {
	Schema    string                      `json:"schema"`
	Binding   TaskRestoreAdmissionBinding `json:"binding"`
	CreatedAt time.Time                   `json:"created_at"`
	ExpiresAt time.Time                   `json:"expires_at"`
	Evidence  []TaskRestoreEvidenceClaim  `json:"evidence"`
}

type TaskRestoreAdmissionState struct {
	Binding         TaskRestoreAdmissionBinding
	SandboxVerified bool
}

type TaskRestoreEvidenceObservation struct {
	Binding TaskRestoreAdmissionBinding
	SHA256  string
	// Only "verified", "missing", and "conflict" are recognized. Every other
	// value is unverified, including attempts to waive an allegedly unused kind.
	Status     string
	VerifiedAt time.Time
	ExpiresAt  time.Time
}

// Implementations are trusted, read-only adapters, never deserialized from a
// manifest. ReadState must revalidate persisted records and current versions;
// CheckEvidence must actually check the named dependency (including absence
// when not applicable), without decrypting into a report or executing effects.
// StateRevision must change for any relevant state/version/lease mutation.
// This diagnostic does not make evaluation atomic with future execution.
type TaskRestoreAdmissionReader interface {
	ReadState(context.Context) (TaskRestoreAdmissionState, error)
	CheckEvidence(context.Context, TaskRestoreEvidenceClaim) (TaskRestoreEvidenceObservation, error)
}

type TaskRestoreAdmissionBlocker struct {
	Kind string `json:"kind"`
	Code string `json:"code"`
}

type TaskRestoreAdmissionReport struct {
	Schema           string                        `json:"schema"`
	Binding          *TaskRestoreAdmissionBinding  `json:"binding,omitempty"`
	CheckedAt        time.Time                     `json:"checked_at"`
	PayloadSHA256    string                        `json:"payload_sha256"`
	ManifestSHA256   string                        `json:"manifest_sha256"`
	VerifiedEvidence []TaskRestoreEvidenceClaim    `json:"verified_evidence"`
	EvidenceComplete bool                          `json:"evidence_complete"`
	Decision         string                        `json:"decision"`
	RestoreReady     bool                          `json:"restore_ready"`
	SourcePreserved  bool                          `json:"source_preserved"`
	Blockers         []TaskRestoreAdmissionBlocker `json:"blockers"`
}

func restoreDigest(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}
func validRestoreDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	raw, err := hex.DecodeString(value)
	return err == nil && hex.EncodeToString(raw) == value
}

func validRestoreBinding(b TaskRestoreAdmissionBinding) bool {
	if b.TaskID == "" || len(b.TaskID) > 256 || b.TenantID == "" || len(b.TenantID) > 256 || !validRestoreDigest(b.PayloadSHA256) || !validRestoreDigest(b.TargetID) || !validRestoreDigest(b.StateRevision) {
		return false
	}
	for _, v := range []string{b.TargetVersion, b.RuntimeVersion, b.PolicyVersion} {
		if len(v) == 0 || len(v) > 128 {
			return false
		}
		for _, c := range v {
			if c < 33 || c > 126 {
				return false
			}
		}
	}
	return true
}

func validRestoreEvidenceTime(start, end, now time.Time) bool {
	return !start.IsZero() && !end.IsZero() && !start.After(now) && end.After(now) && end.After(start) && end.Sub(start) <= maxTaskRestoreEvidenceAge && now.Sub(start) <= maxTaskRestoreEvidenceAge
}

// EvaluateTaskRestoreAdmission validates a v2 export and a versioned manifest,
// then compares trusted observations. It never writes, connects to a Store,
// follows evidence hashes as URLs/paths, or grants restoration permission.
// clock must be a trusted local clock; nil uses time.Now. All nine categories
// are mandatory, even when the reader must prove a dependency is not applicable.
func EvaluateTaskRestoreAdmission(ctx context.Context, artifact, manifestData []byte, tenant string, budget int, reader TaskRestoreAdmissionReader, clock func() time.Time) (*TaskRestoreAdmissionReport, error) {
	a, snap, _, err := decodeJointRestore(ctx, artifact, tenant, budget)
	if err != nil {
		return nil, err
	}
	if len(manifestData) > MaxTaskRestoreManifestBytes {
		return nil, ErrTaskExportTooLarge
	}
	var manifest TaskRestoreDependencyManifest
	if strictRestoreJSON(manifestData, &manifest) != nil || manifest.Schema != TaskRestoreAdmissionSchema || !validRestoreBinding(manifest.Binding) || len(manifest.Evidence) > len(restoreAdmissionKinds) {
		return nil, ErrTaskRestoreManifest
	}
	claims := make(map[string]TaskRestoreEvidenceClaim)
	for _, claim := range manifest.Evidence {
		known := false
		for _, kind := range restoreAdmissionKinds {
			if kind == claim.Kind {
				known = true
				break
			}
		}
		if !known || !validRestoreDigest(claim.SHA256) {
			return nil, ErrTaskRestoreManifest
		}
		if _, duplicate := claims[claim.Kind]; duplicate {
			return nil, ErrTaskRestoreManifest
		}
		claims[claim.Kind] = claim
	}
	if clock == nil {
		clock = time.Now
	}
	now := clock()
	lastTime := now
	clockMovedBackwards := false
	observeTime := func() time.Time {
		current := clock()
		if current.Before(lastTime) {
			clockMovedBackwards = true
		}
		lastTime = current
		return current
	}
	report := &TaskRestoreAdmissionReport{Schema: "task-restore-admission-diagnostic-v1", CheckedAt: now, PayloadSHA256: a.PayloadSHA256, ManifestSHA256: restoreDigest(manifestData), Decision: "blocked", SourcePreserved: true, VerifiedEvidence: []TaskRestoreEvidenceClaim{}, Blockers: []TaskRestoreAdmissionBlocker{}}
	block := func(kind, code string) {
		report.Blockers = append(report.Blockers, TaskRestoreAdmissionBlocker{Kind: kind, Code: code})
	}
	artifactBindingValid := manifest.Binding.TaskID == snap.Task.ID && manifest.Binding.TenantID == tenant && manifest.Binding.PayloadSHA256 == a.PayloadSHA256
	if !artifactBindingValid {
		block("manifest", "artifact_binding_mismatch")
	}
	manifestTimeValid := validRestoreEvidenceTime(manifest.CreatedAt, manifest.ExpiresAt, now)
	if !manifestTimeValid {
		block("manifest", "expired_or_invalid_time")
	}
	var initial TaskRestoreAdmissionState
	stateValid := false
	if reader == nil {
		block("target", "unverified")
	} else {
		initial, err = reader.ReadState(ctx)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil {
			block("target", "unverified")
		} else if !initial.SandboxVerified || !validRestoreBinding(initial.Binding) {
			block("target", "sandbox_unverified")
		} else {
			report.Binding = &initial.Binding
			if initial.Binding != manifest.Binding {
				block("target", "state_binding_mismatch")
			} else {
				stateValid = true
			}
		}
	}
	observations := make(map[string]TaskRestoreEvidenceObservation)
	for _, kind := range restoreAdmissionKinds {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		claim, exists := claims[kind]
		if !exists {
			block(kind, "missing")
			continue
		}
		// Never consult external evidence if the artifact/target/time binding failed.
		if !stateValid || !manifestTimeValid || !artifactBindingValid {
			block(kind, "unverified")
			continue
		}
		observation, err := reader.CheckEvidence(ctx, claim)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil {
			block(kind, "unverified")
			continue
		}
		switch observation.Status {
		case "missing":
			block(kind, "missing")
		case "conflict":
			block(kind, "conflict")
		case "verified":
			if observation.Binding != manifest.Binding || observation.SHA256 != claim.SHA256 {
				block(kind, "evidence_binding_mismatch")
			} else if !validRestoreEvidenceTime(observation.VerifiedAt, observation.ExpiresAt, observeTime()) {
				block(kind, "expired_or_invalid_time")
			} else {
				observations[kind] = observation
			}
		default:
			block(kind, "unverified")
		}
	}
	stateUnchanged := stateValid
	if stateValid {
		final, err := reader.ReadState(ctx)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil {
			stateUnchanged = false
			block("target", "unverified")
		} else if final != initial {
			stateUnchanged = false
			block("target", "state_changed")
		}
	}
	finished := observeTime()
	if clockMovedBackwards {
		block("assessment", "clock_moved_backwards")
	}
	if !validRestoreEvidenceTime(manifest.CreatedAt, manifest.ExpiresAt, finished) {
		if validRestoreEvidenceTime(manifest.CreatedAt, manifest.ExpiresAt, now) {
			block("manifest", "expired_during_assessment")
		}
	}
	for _, kind := range restoreAdmissionKinds {
		if o, ok := observations[kind]; ok && !validRestoreEvidenceTime(o.VerifiedAt, o.ExpiresAt, finished) {
			block(kind, "expired_during_assessment")
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if stateUnchanged && artifactBindingValid && manifestTimeValid && validRestoreEvidenceTime(manifest.CreatedAt, manifest.ExpiresAt, finished) && !clockMovedBackwards {
		for _, kind := range restoreAdmissionKinds {
			if o, ok := observations[kind]; ok && validRestoreEvidenceTime(o.VerifiedAt, o.ExpiresAt, finished) {
				report.VerifiedEvidence = append(report.VerifiedEvidence, claims[kind])
			}
		}
	}
	report.EvidenceComplete = len(report.Blockers) == 0
	// Even complete evidence is diagnostic: online execution, dependency import,
	// independent admission review and atomic execution fencing are absent.
	block("execution", "online_restore_not_implemented")
	return report, nil
}
