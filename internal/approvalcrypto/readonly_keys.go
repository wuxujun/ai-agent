package approvalcrypto

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"sync"
)

var ErrReadOnlyKeys = errors.New("read-only approval keys unavailable")

// ReadOnlyKeySnapshot contains no raw keys or key-derived public fingerprints.
// Generation is an opaque 64-character lowercase hex provider-owned revision,
// including revocations. It must not be a key-derived fingerprint.
type ReadOnlyKeySnapshot struct {
	Codec      *Codec `json:"-"`
	Generation string
}

// ReadOnlyKeyProvider is a trusted injection boundary, never manifest data.
// Implementations must bound reads, honor cancellation and change Generation
// whenever keys, availability or revocation state change. Errors are secret.
type ReadOnlyKeyProvider interface {
	Snapshot(context.Context) (ReadOnlyKeySnapshot, error)
}

type environmentKeyProvider struct {
	mu                            sync.Mutex
	primary, previous, generation string
	initialized                   bool
}

// NewEnvironmentReadOnlyKeyProvider reads only explicitly selected environment
// secrets. It does not read runtime config, files or operator version labels.
// Random generations are stable within this provider's lifetime; a new process
// deliberately invalidates old manifests rather than publishing key hashes.
func NewEnvironmentReadOnlyKeyProvider() ReadOnlyKeyProvider {
	return &environmentKeyProvider{}
}

func (p *environmentKeyProvider) Snapshot(ctx context.Context) (ReadOnlyKeySnapshot, error) {
	if err := ctx.Err(); err != nil {
		return ReadOnlyKeySnapshot{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	primary := os.Getenv("AI_AGENT_APPROVAL_ENCRYPTION_KEY")
	previous := os.Getenv("AI_AGENT_APPROVAL_ENCRYPTION_PREVIOUS_KEYS")
	if !p.initialized || primary != p.primary || previous != p.previous {
		var nonce [32]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return ReadOnlyKeySnapshot{}, ErrReadOnlyKeys
		}
		p.generation = hex.EncodeToString(nonce[:])
		p.primary, p.previous, p.initialized = primary, previous, true
	}
	snapshot := ReadOnlyKeySnapshot{Generation: p.generation}
	if len(primary) > 128 || len(previous) > 4096 || strings.TrimSpace(primary) == "" {
		return snapshot, ErrReadOnlyKeys
	}
	var history []string
	if previous != "" {
		history = strings.Split(previous, ",")
		if len(history) > 32 {
			return snapshot, ErrReadOnlyKeys
		}
		for _, key := range history {
			if strings.TrimSpace(key) == "" || len(key) > 128 {
				return snapshot, ErrReadOnlyKeys
			}
		}
	}
	codec, err := NewFromBase64Keyring(primary, history)
	if err != nil {
		return snapshot, ErrReadOnlyKeys
	}
	if err := ctx.Err(); err != nil {
		return ReadOnlyKeySnapshot{}, err
	}
	snapshot.Codec = codec
	return snapshot, nil
}
