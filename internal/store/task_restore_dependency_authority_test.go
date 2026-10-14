package store

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

type dependencyAuthorityControlFixture struct {
	pin                   TaskRestoreDependencyAuthorityTrustSnapshot
	evidence              []byte
	trustErr, evidenceErr error
	reads, evidenceReads  int
	onEvidence            func()
	onTrust               func(int)
}

func (f *dependencyAuthorityControlFixture) ReadDependencyAuthorityTrust(ctx context.Context, q TaskRestoreDependencyRequest, kind string) (TaskRestoreDependencyAuthorityTrustSnapshot, error) {
	f.reads++
	if f.onTrust != nil {
		f.onTrust(f.reads)
	}
	if err := ctx.Err(); err != nil {
		return TaskRestoreDependencyAuthorityTrustSnapshot{}, err
	}
	return f.pin, f.trustErr
}
func (f *dependencyAuthorityControlFixture) ReadDependencyAuthorityEvidence(ctx context.Context, q TaskRestoreDependencyRequest, kind string) ([]byte, error) {
	f.evidenceReads++
	if f.onEvidence != nil {
		f.onEvidence()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return f.evidence, f.evidenceErr
}

func signedDependencyAuthorityFixture(t *testing.T, kind string) (TaskRestoreDependencyRequest, *dependencyAuthorityControlFixture, *TaskRestoreDependencyAuthorityReader, ed25519.PrivateKey) {
	t.Helper()
	_, _, r, observations := dependencyFixture(t)
	q := r.request
	q.TargetID = observations.session.Scope.TargetID
	var raw []byte
	var err error
	if kind == "session_dependencies" {
		raw, err = json.Marshal(observations.session)
	} else {
		raw, err = json.Marshal(observations.execution)
	}
	if err != nil {
		t.Fatal(err)
	}
	content := TaskRestoreDependencyAuthorityContent{Schema: TaskRestoreDependencyAuthoritySchema, Kind: kind, KeyID: "fixture-key-1", Observation: raw}
	normalized, scope, err := normalizeDependencyAuthorityObservation(kind, raw)
	if err != nil {
		t.Fatal(err)
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := TaskRestoreDependencyAuthoritySigningBytes(content)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(TaskRestoreDependencyAuthorityEvidence{Content: content, Signature: ed25519.Sign(private, signed)})
	if err != nil {
		t.Fatal(err)
	}
	f := &dependencyAuthorityControlFixture{pin: TaskRestoreDependencyAuthorityTrustSnapshot{PublicKey: public, KeyID: content.KeyID, Kind: kind, TaskID: q.TaskID, TenantID: q.TenantID, PayloadSHA256: q.PayloadSHA256, SourceID: q.SourceID, TargetID: q.TargetID, ObservationSHA256: restoreDigest(normalized), Revision: scope.Revision, TrustRevision: scope.TrustRevision, Generation: restoreDigest([]byte("control-1")), Status: "active", CreatedAt: scope.CreatedAt, ExpiresAt: scope.ExpiresAt}, evidence: data}
	reader, err := NewTaskRestoreDependencyAuthorityReader(f, f)
	if err != nil {
		t.Fatal(err)
	}
	return q, f, reader, private
}
func readAuthorityKind(ctx context.Context, r *TaskRestoreDependencyAuthorityReader, q TaskRestoreDependencyRequest, kind string) ([]byte, error) {
	if kind == "session_dependencies" {
		return r.ReadSessionDependencies(ctx, q)
	}
	return r.ReadExecutionObservation(ctx, q)
}
func resignDependencyEvidence(t *testing.T, f *dependencyAuthorityControlFixture, key ed25519.PrivateKey, mutate func(*TaskRestoreDependencyAuthorityContent)) {
	t.Helper()
	var e TaskRestoreDependencyAuthorityEvidence
	if err := json.Unmarshal(f.evidence, &e); err != nil {
		t.Fatal(err)
	}
	mutate(&e.Content)
	signed, err := TaskRestoreDependencyAuthoritySigningBytes(e.Content)
	if err != nil {
		t.Fatal(err)
	}
	e.Signature = ed25519.Sign(key, signed)
	f.evidence, err = json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	normalized, scope, err := normalizeDependencyAuthorityObservation(e.Content.Kind, e.Content.Observation)
	if err != nil {
		t.Fatal(err)
	}
	f.pin.ObservationSHA256 = restoreDigest(normalized)
	f.pin.Revision = scope.Revision
	f.pin.TrustRevision = scope.TrustRevision
}

func TestTaskRestoreDependencyAuthorityVerifiedAndGenerationBinding(t *testing.T) {
	for _, kind := range []string{"session_dependencies", "execution_leases"} {
		t.Run(kind, func(t *testing.T) {
			q, f, r, _ := signedDependencyAuthorityFixture(t, kind)
			beforeEvidence := bytes.Clone(f.evidence)
			got, err := readAuthorityKind(t.Context(), r, q, kind)
			if err != nil || len(got) == 0 || f.reads != 2 || f.evidenceReads != 1 {
				t.Fatal("signed observation rejected", err)
			}
			_, scope, err := normalizeDependencyAuthorityObservation(kind, got)
			if err != nil {
				t.Fatal(err)
			}
			if scope.TrustRevision == f.pin.TrustRevision {
				t.Fatal("current generation missing from output binding")
			}
			f.pin.Generation = restoreDigest([]byte("control-2"))
			next, err := readAuthorityKind(t.Context(), r, q, kind)
			if err != nil || bytes.Equal(got, next) {
				t.Fatal("ABA/rotation generation did not invalidate output", err)
			}
			if !bytes.Equal(beforeEvidence, f.evidence) {
				t.Fatal("evidence mutated")
			}
		})
	}
}

func TestTaskRestoreDependencyAuthorityFailClosed(t *testing.T) {
	cases := []string{"bad_signature", "wrong_key", "wrong_key_id", "wrong_kind", "wrong_source", "wrong_target", "wrong_tenant", "wrong_payload", "wrong_task", "wrong_digest", "wrong_revision", "wrong_trust_revision", "missing_generation", "revoked", "expired_pin", "future_pin", "long_pin", "trust_error", "evidence_error", "unknown_envelope_field", "duplicate_key", "trailing_json", "oversize", "unsigned", "untrusted_key", "rotated_during_read", "revoked_during_read", "generation_during_read", "cancel"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			q, f, r, _ := signedDependencyAuthorityFixture(t, "session_dependencies")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch name {
			case "bad_signature":
				var e TaskRestoreDependencyAuthorityEvidence
				json.Unmarshal(f.evidence, &e)
				e.Signature[0] ^= 0xff
				f.evidence, _ = json.Marshal(e)
			case "wrong_key":
				f.pin.PublicKey = make([]byte, ed25519.PublicKeySize)
			case "wrong_key_id":
				f.pin.KeyID = "another-key"
			case "wrong_kind":
				f.pin.Kind = "execution_leases"
			case "wrong_source":
				f.pin.SourceID = restoreDigest(nil)
			case "wrong_target":
				f.pin.TargetID = restoreDigest(nil)
			case "wrong_tenant":
				f.pin.TenantID = "b"
			case "wrong_payload":
				f.pin.PayloadSHA256 = restoreDigest(nil)
			case "wrong_task":
				f.pin.TaskID = "other"
			case "wrong_digest":
				f.pin.ObservationSHA256 = restoreDigest(nil)
			case "wrong_revision":
				f.pin.Revision = "revision-2"
			case "wrong_trust_revision":
				f.pin.TrustRevision = "trust-2"
			case "missing_generation":
				f.pin.Generation = ""
			case "revoked":
				f.pin.Status = "revoked"
			case "expired_pin":
				f.pin.ExpiresAt = time.Now().Add(-time.Second)
			case "future_pin":
				f.pin.CreatedAt = time.Now().Add(time.Minute)
			case "long_pin":
				f.pin.ExpiresAt = time.Now().Add(time.Hour)
			case "trust_error":
				f.trustErr = errors.New("private-credential")
			case "evidence_error":
				f.evidenceErr = errors.New("private-credential")
			case "unknown_envelope_field":
				f.evidence = bytes.Replace(f.evidence, []byte(`"content":`), []byte(`"unknown":1,"content":`), 1)
			case "duplicate_key":
				f.evidence = bytes.Replace(f.evidence, []byte(`"key_id":`), []byte(`"key_id":"duplicate","key_id":`), 1)
			case "trailing_json":
				f.evidence = append(f.evidence, []byte(" {}")...)
			case "oversize":
				f.evidence = make([]byte, MaxTaskRestoreDependencyAuthorityBytes+1)
			case "unsigned":
				var e TaskRestoreDependencyAuthorityEvidence
				json.Unmarshal(f.evidence, &e)
				e.Signature = nil
				f.evidence, _ = json.Marshal(e)
			case "untrusted_key":
				f.evidence = bytes.Replace(f.evidence, []byte(`"content":`), []byte(`"public_key":"attacker","content":`), 1)
			case "rotated_during_read":
				f.onEvidence = func() { f.pin.PublicKey[0] ^= 0xff }
			case "revoked_during_read":
				f.onEvidence = func() { f.pin.Status = "revoked" }
			case "generation_during_read":
				f.onEvidence = func() { f.pin.Generation = restoreDigest([]byte("new-generation")) }
			case "cancel":
				f.onEvidence = cancel
			}
			result, err := r.ReadSessionDependencies(ctx, q)
			if result != nil || err == nil {
				t.Fatal("unsafe authority accepted")
			}
			if name == "cancel" {
				if !errors.Is(err, context.Canceled) {
					t.Fatal("cancellation lost", err)
				}
			} else if !errors.Is(err, ErrTaskRestoreDependencyAuthority) || bytes.Contains([]byte(err.Error()), []byte("private")) {
				t.Fatal("provider error leaked", err)
			}
		})
	}
}

func TestTaskRestoreDependencyAuthoritySignatureDoesNotWaiveContracts(t *testing.T) {
	for _, mode := range []string{"missing_member", "bad_baseline", "highwater", "active_lease", "no_stop", "target_lease", "nonterminal"} {
		t.Run(mode, func(t *testing.T) {
			kind := "session_dependencies"
			if mode == "active_lease" || mode == "no_stop" || mode == "target_lease" || mode == "nonterminal" {
				kind = "execution_leases"
			}
			q, f, r, key := signedDependencyAuthorityFixture(t, kind)
			resignDependencyEvidence(t, f, key, func(c *TaskRestoreDependencyAuthorityContent) {
				if kind == "session_dependencies" {
					var o TaskRestoreSessionObservation
					json.Unmarshal(c.Observation, &o)
					switch mode {
					case "missing_member":
						o.Target.Members = []TaskRestoreDependencyMember{}
					case "bad_baseline":
						o.BaselineRevision = "different"
					case "highwater":
						o.Source.HighWatermark--
					}
					c.Observation, _ = json.Marshal(o)
				} else {
					var o TaskRestoreExecutionObservation
					json.Unmarshal(c.Observation, &o)
					switch mode {
					case "active_lease":
						o.LeasePresent = true
						o.OwnerSHA256 = restoreDigest(nil)
						o.LeaseExpiresAt = time.Now().Add(time.Minute)
					case "no_stop":
						o.IsolationContract = ""
					case "target_lease":
						o.TargetLeasePresent = true
					case "nonterminal":
						q.Status = "running"
						o.SourceStatus = q.Status
						o.TargetStatus = q.Status
					}
					c.Observation, _ = json.Marshal(o)
				}
			})
			result, err := readAuthorityKind(t.Context(), r, q, kind)
			if result != nil || !errors.Is(err, ErrTaskRestoreDependencyAuthority) {
				t.Fatal("signature waived admission contract", err)
			}
		})
	}
}

func TestTaskRestoreDependencyAuthorityTrustWindowAndStrictSigning(t *testing.T) {
	q, f, r, private := signedDependencyAuthorityFixture(t, "execution_leases")
	f.pin.ExpiresAt = time.Now().Add(time.Minute)
	output, err := r.ReadExecutionObservation(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	var o TaskRestoreExecutionObservation
	if strictRestoreJSON(output, &o) != nil || !o.Scope.ExpiresAt.Equal(f.pin.ExpiresAt) {
		t.Fatal("trust window not bounded")
	}
	isolationEnd := time.Now().UTC().Add(30 * time.Second)
	resignDependencyEvidence(t, f, private, func(c *TaskRestoreDependencyAuthorityContent) {
		var observation TaskRestoreExecutionObservation
		json.Unmarshal(c.Observation, &observation)
		observation.IsolationExpiresAt = isolationEnd
		c.Observation, _ = json.Marshal(observation)
	})
	output, err = r.ReadExecutionObservation(t.Context(), q)
	if err != nil || strictRestoreJSON(output, &o) != nil || !o.Scope.ExpiresAt.Equal(isolationEnd) {
		t.Fatal("isolation window not bounded", err)
	}
	var e TaskRestoreDependencyAuthorityEvidence
	json.Unmarshal(f.evidence, &e)
	first, err := TaskRestoreDependencyAuthoritySigningBytes(e.Content)
	if err != nil {
		t.Fatal(err)
	}
	var formatted bytes.Buffer
	json.Indent(&formatted, e.Content.Observation, "", "  ")
	e.Content.Observation = formatted.Bytes()
	second, err := TaskRestoreDependencyAuthoritySigningBytes(e.Content)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatal("signing contract depends on whitespace", err)
	}
	e.Content.Observation = bytes.Replace(e.Content.Observation, []byte(`"scope":`), []byte(`"unexpected":1,"scope":`), 1)
	if _, err := TaskRestoreDependencyAuthoritySigningBytes(e.Content); err == nil {
		t.Fatal("unknown observation field signed")
	}
	// Cross-kind envelopes and nil providers are never accepted.
	if _, err := r.ReadSessionDependencies(t.Context(), q); !errors.Is(err, ErrTaskRestoreDependencyAuthority) {
		t.Fatal("kind substitution accepted", err)
	}
	if _, err := NewTaskRestoreDependencyAuthorityReader(nil, f); err == nil {
		t.Fatal("nil evidence accepted")
	}
	if _, err := NewTaskRestoreDependencyAuthorityReader(f, nil); err == nil {
		t.Fatal("nil trust accepted")
	}
	q.TenantID = ""
	before := f.reads
	if _, err := r.ReadExecutionObservation(t.Context(), q); err == nil || f.reads != before {
		t.Fatal("invalid request consulted trust")
	}
}

func TestTaskRestoreDependencyAuthorityCompositeAdmissionAndRevocation(t *testing.T) {
	artifact, base, _, _ := dependencyFixture(t)
	sessionQ, sessionControl, sessionReader, _ := signedDependencyAuthorityFixture(t, "session_dependencies")
	executionQ, _, executionReader, _ := signedDependencyAuthorityFixture(t, "execution_leases")
	reader, err := NewTaskRestoreDependencyAdmissionReader(t.Context(), base, artifact, "a", TaskRestoreDependencyReaders{SourceID: sessionQ.SourceID, Session: sessionReader, Execution: executionReader})
	if err != nil {
		t.Fatal(err)
	}
	state, err := reader.ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	sessionData, err := sessionReader.ReadSessionDependencies(t.Context(), sessionQ)
	if err != nil {
		t.Fatal(err)
	}
	executionData, err := executionReader.ReadExecutionObservation(t.Context(), executionQ)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	manifest := TaskRestoreDependencyManifest{Schema: TaskRestoreAdmissionSchema, Binding: state.Binding, CreatedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Minute), Evidence: []TaskRestoreEvidenceClaim{{Kind: "session_dependencies", SHA256: restoreDigest(sessionData)}, {Kind: "execution_leases", SHA256: restoreDigest(executionData)}}}
	report, err := EvaluateTaskRestoreAdmission(t.Context(), artifact, admissionJSON(t, manifest), "a", MaxTaskExportBytes, reader, nil)
	if err != nil || len(report.VerifiedEvidence) != 2 || report.RestoreReady || report.Decision != "blocked" || !report.SourcePreserved {
		t.Fatal(report, err)
	}
	sessionControl.pin.Status = "revoked"
	report, err = EvaluateTaskRestoreAdmission(t.Context(), artifact, admissionJSON(t, manifest), "a", MaxTaskExportBytes, reader, nil)
	if err != nil || len(report.VerifiedEvidence) != 0 || !hasAdmissionBlocker(report, "target", "state_binding_mismatch") {
		t.Fatal("revoked signature remained usable", report, err)
	}
	body, _ := json.Marshal(report)
	for _, secret := range []string{"fixture-key-1", "private-goal", "private-trace", "generation-1"} {
		if bytes.Contains(body, []byte(secret)) {
			t.Fatal("authority data leaked")
		}
	}
}

func TestTaskRestoreDependencyAuthorityRejectsSecondReadChanges(t *testing.T) {
	for _, mode := range []string{"expires", "permissions", "snapshot", "key_id", "generation", "error"} {
		t.Run(mode, func(t *testing.T) {
			q, f, r, _ := signedDependencyAuthorityFixture(t, "session_dependencies")
			f.onTrust = func(call int) {
				if call != 2 {
					return
				}
				switch mode {
				case "expires":
					f.pin.ExpiresAt = time.Now().Add(-time.Second)
				case "permissions":
					f.pin.TrustRevision = "permission-2"
				case "snapshot":
					f.pin.ObservationSHA256 = restoreDigest(nil)
				case "key_id":
					f.pin.KeyID = "key-2"
				case "generation":
					f.pin.Generation = restoreDigest([]byte("generation-2"))
				case "error":
					f.trustErr = errors.New("private-secret")
				}
			}
			if result, err := r.ReadSessionDependencies(t.Context(), q); result != nil || !errors.Is(err, ErrTaskRestoreDependencyAuthority) {
				t.Fatal("mid-read trust changed", err)
			}
		})
	}
}

func TestTaskRestoreDependencyAuthoritySignedMemberTraceMismatch(t *testing.T) {
	q, f, r, key := signedDependencyAuthorityFixture(t, "session_dependencies")
	resignDependencyEvidence(t, f, key, func(c *TaskRestoreDependencyAuthorityContent) {
		var o TaskRestoreSessionObservation
		json.Unmarshal(c.Observation, &o)
		member := TaskRestoreDependencyMember{ID: "restore-task-z", TenantID: q.TenantID, SessionID: q.SessionID, SequenceNo: q.SequenceNo + 1, TaskSHA256: restoreDigest([]byte("same-task")), TraceSHA256: restoreDigest([]byte("source-traces")), Revision: "other-1"}
		o.Source.Members = append(o.Source.Members, member)
		member.TraceSHA256 = restoreDigest([]byte("target-traces"))
		o.Target.Members = append(o.Target.Members, member)
		c.Observation, _ = json.Marshal(o)
	})
	// Even a valid signature and current pins cannot waive exact Trace equality.
	if data, err := r.ReadSessionDependencies(t.Context(), q); data != nil || !errors.Is(err, ErrTaskRestoreDependencyAuthority) {
		t.Fatal("signed checkpoint mismatch accepted", err)
	}
	var e TaskRestoreDependencyAuthorityEvidence
	json.Unmarshal(f.evidence, &e)
	e.Content.Observation = bytes.Replace(e.Content.Observation, []byte(TaskRestoreSessionObservationSchema), []byte("task-restore-session-observation-v1"), 1)
	if _, err := TaskRestoreDependencyAuthoritySigningBytes(e.Content); err == nil {
		t.Fatal("legacy unbound schema signed")
	}
}
