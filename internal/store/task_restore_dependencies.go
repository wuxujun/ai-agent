package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/wuxujun/ai-agent/internal/types"
)

const (
	TaskRestoreSessionObservationSchema   = "task-restore-session-observation-v1"
	TaskRestoreExecutionObservationSchema = "task-restore-execution-observation-v1"
	MaxTaskRestoreObservationBytes        = 8 << 20
	MaxTaskRestoreObservationTasks        = 1000
	MaxTaskRestoreObservationEdges        = 4000
)

// Readers are selected by trusted code. They must authenticate authority, prove
// complete snapshots and monotonic revisions (including deletion/recreation),
// recheck revocation, and perform no writes. JSON is bounded, copied immediately
// and strictly decoded; it is never accepted as evidence from a manifest.
// Returning an empty/incomplete snapshot does not prove absence.
type TaskRestoreSessionDependencyReader interface {
	ReadSessionDependencies(context.Context, TaskRestoreDependencyRequest) ([]byte, error)
}
type TaskRestoreExecutionObservationReader interface {
	ReadExecutionObservation(context.Context, TaskRestoreDependencyRequest) ([]byte, error)
}

type TaskRestoreDependencyRequest struct {
	TaskID, TenantID, SessionID, PayloadSHA256, SourceID, TargetID string
	TaskSHA256, CheckpointSHA256                                   string
	SessionSHA256                                                  string
	HighWatermark                                                  int64
	SequenceNo                                                     int64
	Status                                                         types.TaskStatus
	MaxBytes, MaxTasks, MaxEdges                                   int
}

type TaskRestoreDependencyReaders struct {
	SourceID  string
	Session   TaskRestoreSessionDependencyReader
	Execution TaskRestoreExecutionObservationReader
}

// Revision is a trusted monotonic change generation, not a content fingerprint.
// TrustRevision includes permission and revocation changes. Providers must bind
// these revisions to their authenticated source/target identities.
type TaskRestoreObservationScope struct {
	TaskID        string    `json:"task_id"`
	TenantID      string    `json:"tenant_id"`
	PayloadSHA256 string    `json:"payload_sha256"`
	SourceID      string    `json:"source_id"`
	TargetID      string    `json:"target_id"`
	Revision      string    `json:"revision"`
	TrustRevision string    `json:"trust_revision"`
	CreatedAt     time.Time `json:"created_at"`
	ExpiresAt     time.Time `json:"expires_at"`
}

type TaskRestoreDependencyMember struct {
	ID         string `json:"id"`
	TenantID   string `json:"tenant_id"`
	SessionID  string `json:"session_id"`
	SequenceNo int64  `json:"sequence_no,string"`
	TaskSHA256 string `json:"task_sha256"`
	Revision   string `json:"revision"`
}
type TaskRestoreDependencyEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
}
type TaskRestoreSessionSnapshot struct {
	SessionID     string                        `json:"session_id"`
	SessionSHA256 string                        `json:"session_sha256"`
	Status        types.SessionStatus           `json:"status"`
	HighWatermark int64                         `json:"high_watermark,string"`
	Revision      string                        `json:"revision"`
	Members       []TaskRestoreDependencyMember `json:"members"`
	Edges         []TaskRestoreDependencyEdge   `json:"edges"`
}

type TaskRestoreSessionObservation struct {
	Schema string                      `json:"schema"`
	Scope  TaskRestoreObservationScope `json:"scope"`
	// v1 only supports a complete same-session closure with an acyclic graph.
	// Cross-session dependencies need a future contract and remain unverified.
	DependencyContract    string                     `json:"dependency_contract"`
	BaselinePayloadSHA256 string                     `json:"baseline_payload_sha256"`
	BaselineRevision      string                     `json:"baseline_revision"`
	Source                TaskRestoreSessionSnapshot `json:"source"`
	Target                TaskRestoreSessionSnapshot `json:"target"`
}

type TaskRestoreExecutionObservation struct {
	Schema              string                      `json:"schema"`
	Scope               TaskRestoreObservationScope `json:"scope"`
	Policy              string                      `json:"policy"`
	SourceStatus        types.TaskStatus            `json:"source_status"`
	TargetStatus        types.TaskStatus            `json:"target_status"`
	TaskSHA256          string                      `json:"task_sha256"`
	CheckpointSHA256    string                      `json:"checkpoint_sha256"`
	RuntimeVersion      string                      `json:"runtime_version"`
	LeaseRevision       string                      `json:"lease_revision"`
	TargetLeaseRevision string                      `json:"target_lease_revision"`
	TargetLeasePresent  bool                        `json:"target_lease_present"`
	LeasePresent        bool                        `json:"lease_present"`
	OwnerSHA256         string                      `json:"owner_sha256"`
	LeaseExpiresAt      time.Time                   `json:"lease_expires_at"`
	// Authority must prove dispatch is disabled and previous executors stopped.
	// v1 does not accept fencing-only or nonterminal recovery claims.
	IsolationContract  string    `json:"isolation_contract"`
	IsolationRevision  string    `json:"isolation_revision"`
	IsolationExpiresAt time.Time `json:"isolation_expires_at"`
}

type restoreDependencyAdmissionReader struct {
	base    TaskRestoreAdmissionReader
	readers TaskRestoreDependencyReaders
	request TaskRestoreDependencyRequest
}

// NewTaskRestoreDependencyAdmissionReader composes trusted observations with an
// existing reader. It validates the artifact before calling any provider. No
// default provider, runtime config, network connection or writable Store exists.
func NewTaskRestoreDependencyAdmissionReader(ctx context.Context, base TaskRestoreAdmissionReader, artifact []byte, tenant string, readers TaskRestoreDependencyReaders) (TaskRestoreAdmissionReader, error) {
	if base == nil || ((readers.Session != nil || readers.Execution != nil) && !validRestoreDigest(readers.SourceID)) {
		return nil, ErrTaskRestoreManifest
	}
	a, snap, _, err := decodeJointRestore(ctx, artifact, tenant, MaxTaskExportBytes)
	if err != nil {
		return nil, err
	}
	taskBytes, err := json.Marshal(snap.Task)
	if err != nil {
		return nil, ErrTaskRestoreManifest
	}
	traces, err := json.Marshal(snap.Traces)
	if err != nil {
		return nil, ErrTaskRestoreManifest
	}
	sessionDigest := ""
	var highWatermark int64
	if snap.Dependencies.Session != nil {
		data, err := json.Marshal(snap.Dependencies.Session)
		if err != nil {
			return nil, ErrTaskRestoreManifest
		}
		sessionDigest = restoreDigest(data)
		highWatermark = snap.Dependencies.Session.NextSequence
	}
	return &restoreDependencyAdmissionReader{base: base, readers: readers, request: TaskRestoreDependencyRequest{
		TaskID: snap.Task.ID, TenantID: tenant, SessionID: snap.Task.SessionID, SequenceNo: snap.Task.SequenceNo, Status: snap.Task.Status,
		PayloadSHA256: a.PayloadSHA256, SourceID: readers.SourceID, TaskSHA256: restoreDigest(taskBytes), CheckpointSHA256: restoreDigest(traces),
		SessionSHA256: sessionDigest, HighWatermark: highWatermark,
		MaxBytes: MaxTaskRestoreObservationBytes, MaxTasks: MaxTaskRestoreObservationTasks, MaxEdges: MaxTaskRestoreObservationEdges,
	}}, nil
}

func validObservationToken(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for _, c := range s {
		if c < 33 || c > 126 {
			return false
		}
	}
	return true
}
func validObservationID(s string) bool {
	if len(s) == 0 || len(s) > 256 {
		return false
	}
	for _, c := range s {
		if c < 32 || c == 127 {
			return false
		}
	}
	return true
}
func validObservationScope(s TaskRestoreObservationScope, q TaskRestoreDependencyRequest, now time.Time) bool {
	return s.TaskID == q.TaskID && s.TenantID == q.TenantID && s.PayloadSHA256 == q.PayloadSHA256 && s.SourceID == q.SourceID && s.TargetID == q.TargetID &&
		validObservationToken(s.Revision) && validObservationToken(s.TrustRevision) && validRestoreEvidenceTime(s.CreatedAt, s.ExpiresAt, now)
}

func validateSessionSnapshot(s TaskRestoreSessionSnapshot, q TaskRestoreDependencyRequest) string {
	if !validObservationToken(s.Revision) || s.Members == nil || s.Edges == nil || len(s.Members) > q.MaxTasks || len(s.Edges) > q.MaxEdges {
		return "unverified"
	}
	if s.SessionID != q.SessionID || s.HighWatermark < 0 {
		return "conflict"
	}
	if s.SessionID != "" {
		if !validRestoreDigest(s.SessionSHA256) || (s.Status != types.SessionStatusActive && s.Status != types.SessionStatusArchived) {
			return "unverified"
		}
	} else if s.SessionSHA256 != "" || s.Status != "" || s.HighWatermark != 0 {
		return "conflict"
	}
	members := make(map[string]TaskRestoreDependencyMember, len(s.Members))
	sequences := map[int64]bool{}
	previous := ""
	for _, m := range s.Members {
		if !validObservationID(m.ID) || m.ID <= previous || !validRestoreDigest(m.TaskSHA256) || !validObservationToken(m.Revision) {
			return "unverified"
		}
		previous = m.ID
		if m.TenantID != q.TenantID || m.SessionID != q.SessionID {
			return "conflict"
		}
		if q.SessionID != "" {
			if m.SequenceNo <= 0 || m.SequenceNo > s.HighWatermark || sequences[m.SequenceNo] {
				return "conflict"
			}
			sequences[m.SequenceNo] = true
		} else if m.SequenceNo != 0 {
			return "conflict"
		}
		members[m.ID] = m
	}
	root, ok := members[q.TaskID]
	if !ok {
		return "missing"
	}
	if root.TaskSHA256 != q.TaskSHA256 || root.SequenceNo != q.SequenceNo {
		return "conflict"
	}
	// Same-session v1 cannot prove a no-session task has cross-task dependencies.
	if q.SessionID == "" && (len(members) != 1 || len(s.Edges) != 0) {
		return "unverified"
	}
	adjacency := map[string][]string{}
	indegree := map[string]int{}
	lastFrom, lastTo := "", ""
	for _, e := range s.Edges {
		if e.From < lastFrom || (e.From == lastFrom && e.To <= lastTo) {
			return "unverified"
		}
		lastFrom, lastTo = e.From, e.To
		if _, ok := members[e.From]; !ok {
			return "conflict"
		}
		if _, ok := members[e.To]; !ok {
			return "conflict"
		}
		adjacency[e.From] = append(adjacency[e.From], e.To)
		indegree[e.To]++
	}
	queue := []string{}
	for id := range members {
		if indegree[id] == 0 {
			queue = append(queue, id)
		}
	}
	visited := 0
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		visited++
		for _, to := range adjacency[id] {
			indegree[to]--
			if indegree[to] == 0 {
				queue = append(queue, to)
			}
		}
	}
	if visited != len(members) {
		return "conflict"
	}
	return "verified"
}

func validateSessionObservation(o TaskRestoreSessionObservation, q TaskRestoreDependencyRequest, now time.Time) string {
	if o.Schema != TaskRestoreSessionObservationSchema || !validObservationScope(o.Scope, q, now) || o.DependencyContract != "complete-same-session-dag-v1" ||
		o.BaselinePayloadSHA256 != q.PayloadSHA256 || !validObservationToken(o.BaselineRevision) {
		return "unverified"
	}
	// The authenticated reader must compare its complete historical snapshot to
	// current source; this pinned generation must be unchanged, not just its hash.
	if o.Source.Revision != o.BaselineRevision {
		return "conflict"
	}
	if o.Source.SessionSHA256 != q.SessionSHA256 || o.Source.HighWatermark != q.HighWatermark {
		return "conflict"
	}
	if status := validateSessionSnapshot(o.Source, q); status != "verified" {
		return status
	}
	if status := validateSessionSnapshot(o.Target, q); status != "verified" {
		return status
	}
	if o.Source.SessionSHA256 != o.Target.SessionSHA256 || o.Source.Status != o.Target.Status || o.Source.HighWatermark != o.Target.HighWatermark ||
		len(o.Source.Members) != len(o.Target.Members) || len(o.Source.Edges) != len(o.Target.Edges) {
		return "conflict"
	}
	for i, m := range o.Source.Members {
		target := o.Target.Members[i]
		m.Revision = target.Revision // independent target generations may differ
		if m != target {
			return "conflict"
		}
	}
	for i, e := range o.Source.Edges {
		if e != o.Target.Edges[i] {
			return "conflict"
		}
	}
	return "verified"
}
func validateExecutionObservation(o TaskRestoreExecutionObservation, q TaskRestoreDependencyRequest, now time.Time) string {
	if o.Schema != TaskRestoreExecutionObservationSchema || !validObservationScope(o.Scope, q, now) || o.Policy != "terminal-diagnostic-only-v1" ||
		o.RuntimeVersion != "task-restore-diagnostic-v1" || !validObservationToken(o.LeaseRevision) || !validObservationToken(o.TargetLeaseRevision) || !validObservationToken(o.IsolationRevision) {
		return "unverified"
	}
	if o.SourceStatus != q.Status || o.TargetStatus != q.Status || o.TaskSHA256 != q.TaskSHA256 || o.CheckpointSHA256 != q.CheckpointSHA256 {
		return "conflict"
	}
	if !types.IsTerminalTaskStatus(q.Status) {
		return "unverified"
	}
	if o.TargetLeasePresent {
		return "conflict"
	}
	if o.LeasePresent {
		if !validRestoreDigest(o.OwnerSHA256) || o.LeaseExpiresAt.IsZero() {
			return "unverified"
		}
		// Presence includes expired rows: no usable clock/stop proof is inferred.
		if o.LeaseExpiresAt.After(now) {
			return "conflict"
		}
	} else if o.OwnerSHA256 != "" || !o.LeaseExpiresAt.IsZero() {
		return "conflict"
	}
	if o.IsolationContract != "dispatch-disabled-executors-stopped-v1" || !validRestoreEvidenceTime(o.Scope.CreatedAt, o.IsolationExpiresAt, now) {
		return "unverified"
	}
	return "verified"
}

type restoreDependencyScan struct {
	digest, status string
	start, end     time.Time
}

func (r *restoreDependencyAdmissionReader) scan(ctx context.Context, kind string, binding TaskRestoreAdmissionBinding) (restoreDependencyScan, error) {
	result := restoreDependencyScan{status: "unverified"}
	q := r.request
	q.TargetID = binding.TargetID
	if binding.TaskID != q.TaskID || binding.TenantID != q.TenantID || binding.PayloadSHA256 != q.PayloadSHA256 {
		return result, nil
	}
	child, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var data []byte
	var err error
	switch kind {
	case "session_dependencies":
		if r.readers.Session == nil {
			return result, nil
		}
		data, err = r.readers.Session.ReadSessionDependencies(child, q)
	case "execution_leases":
		if r.readers.Execution == nil {
			return result, nil
		}
		data, err = r.readers.Execution.ReadExecutionObservation(child, q)
	default:
		return result, nil
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if err != nil || child.Err() != nil || len(data) > q.MaxBytes {
		return result, nil
	}
	// Provider must not mutate returned bytes concurrently; own the retained copy.
	data = append([]byte(nil), data...)
	result.digest = restoreDigest(data)
	now := time.Now().UTC()
	if kind == "session_dependencies" {
		var o TaskRestoreSessionObservation
		if strictRestoreJSON(data, &o) != nil {
			return result, nil
		}
		result.status = validateSessionObservation(o, q, now)
		result.start, result.end = o.Scope.CreatedAt, o.Scope.ExpiresAt
	} else {
		var o TaskRestoreExecutionObservation
		if strictRestoreJSON(data, &o) != nil {
			return result, nil
		}
		result.status = validateExecutionObservation(o, q, now)
		result.start, result.end = o.Scope.CreatedAt, o.Scope.ExpiresAt
		if o.IsolationExpiresAt.Before(result.end) {
			result.end = o.IsolationExpiresAt
		}
	}
	return result, nil
}

func (r *restoreDependencyAdmissionReader) observe(ctx context.Context) (TaskRestoreAdmissionState, map[string]restoreDependencyScan, error) {
	scans := map[string]restoreDependencyScan{}
	before, err := r.base.ReadState(ctx)
	if err != nil {
		return TaskRestoreAdmissionState{}, nil, err
	}
	if r.readers.Session == nil && r.readers.Execution == nil {
		return before, scans, nil
	}
	revisions := []string{before.Binding.StateRevision}
	for _, kind := range []string{"session_dependencies", "execution_leases"} {
		scan, err := r.scan(ctx, kind, before.Binding)
		if err != nil {
			return TaskRestoreAdmissionState{}, nil, err
		}
		scans[kind] = scan
		revisions = append(revisions, kind, scan.digest, scan.status)
	}
	after, err := r.base.ReadState(ctx)
	if err != nil {
		return TaskRestoreAdmissionState{}, nil, err
	}
	if after != before {
		return TaskRestoreAdmissionState{}, nil, ErrTaskRestoreMismatch
	}
	encoded, _ := json.Marshal(revisions)
	before.Binding.StateRevision = restoreDigest(encoded)
	before.Binding.PolicyVersion = "read-only-dependencies-v1-" + restoreDigest([]byte(before.Binding.PolicyVersion))
	return before, scans, nil
}

func (r *restoreDependencyAdmissionReader) ReadState(ctx context.Context) (TaskRestoreAdmissionState, error) {
	state, _, err := r.observe(ctx)
	return state, err
}

func (r *restoreDependencyAdmissionReader) CheckEvidence(ctx context.Context, claim TaskRestoreEvidenceClaim) (TaskRestoreEvidenceObservation, error) {
	state, scans, err := r.observe(ctx)
	if err != nil {
		return TaskRestoreEvidenceObservation{}, err
	}
	var observation TaskRestoreEvidenceObservation
	if claim.Kind != "session_dependencies" && claim.Kind != "execution_leases" {
		observation, err = r.base.CheckEvidence(ctx, claim)
		if err != nil {
			return TaskRestoreEvidenceObservation{}, err
		}
		base, err := r.base.ReadState(ctx)
		if err != nil {
			return TaskRestoreEvidenceObservation{}, err
		}
		if observation.Binding != base.Binding {
			return TaskRestoreEvidenceObservation{}, errors.New("dependency base changed")
		}
		observation.Binding = state.Binding
	} else {
		scan := scans[claim.Kind]
		observation = TaskRestoreEvidenceObservation{Binding: state.Binding, SHA256: scan.digest, Status: scan.status, VerifiedAt: scan.start, ExpiresAt: scan.end}
		if scan.digest != "" && scan.digest != claim.SHA256 {
			observation.Status = "conflict"
		}
	}
	final, err := r.ReadState(ctx)
	if ctx.Err() != nil {
		return TaskRestoreEvidenceObservation{}, ctx.Err()
	}
	if err != nil || final != state {
		return TaskRestoreEvidenceObservation{}, ErrTaskRestoreMismatch
	}
	return observation, nil
}
