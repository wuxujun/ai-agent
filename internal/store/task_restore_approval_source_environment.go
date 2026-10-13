package store

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"os"
	"sync"
)

type environmentApprovalSourceTrust struct {
	mu          sync.Mutex
	values      [4]string
	generation  string
	initialized bool
}

// Only explicit callers activate this trust source. Pins must be independently
// provisioned by the source authority, never copied from uploaded evidence.
// There is no private signing key, signing operation, file or network access.
func NewEnvironmentTaskRestoreApprovalSourceTrust() TaskRestoreApprovalSourceTrustProvider {
	return &environmentApprovalSourceTrust{}
}

func (p *environmentApprovalSourceTrust) Snapshot(ctx context.Context) (TaskRestoreApprovalSourceTrustSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return TaskRestoreApprovalSourceTrustSnapshot{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	v := [4]string{os.Getenv("AI_AGENT_RESTORE_APPROVAL_SOURCE_PUBLIC_KEY"), os.Getenv("AI_AGENT_RESTORE_APPROVAL_SOURCE_ID"), os.Getenv("AI_AGENT_RESTORE_APPROVAL_SOURCE_TENANT"), os.Getenv("AI_AGENT_RESTORE_APPROVAL_SOURCE_SNAPSHOT")}
	if !p.initialized || v != p.values {
		var nonce [32]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return TaskRestoreApprovalSourceTrustSnapshot{}, ErrTaskRestoreApprovalSourceTrust
		}
		p.values, p.generation, p.initialized = v, hex.EncodeToString(nonce[:]), true
	}
	s := TaskRestoreApprovalSourceTrustSnapshot{SourceID: v[1], TenantID: v[2], SnapshotID: v[3], Generation: p.generation}
	if len(v[0]) > 128 || !validApprovalSourceID(v[1]) || v[2] == "" || len(v[2]) > 256 || !validApprovalSourceID(v[3]) {
		return s, ErrTaskRestoreApprovalSourceTrust
	}
	key, err := base64.StdEncoding.DecodeString(v[0])
	if err != nil || len(key) != ed25519.PublicKeySize {
		return s, ErrTaskRestoreApprovalSourceTrust
	}
	if err := ctx.Err(); err != nil {
		return TaskRestoreApprovalSourceTrustSnapshot{}, err
	}
	s.PublicKey = ed25519.PublicKey(key)
	return s, nil
}
