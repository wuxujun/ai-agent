package approvalcrypto

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"testing"
)

func TestEnvironmentReadOnlyKeysRotationAndRevocation(t *testing.T) {
	oldKey, newKey := bytes.Repeat([]byte{0x55}, 32), bytes.Repeat([]byte{0x66}, 32)
	old := base64.StdEncoding.EncodeToString(oldKey)
	newValue := base64.StdEncoding.EncodeToString(newKey)
	t.Setenv("AI_AGENT_APPROVAL_ENCRYPTION_KEY", old)
	t.Setenv("AI_AGENT_APPROVAL_ENCRYPTION_PREVIOUS_KEYS", "")
	p := NewEnvironmentReadOnlyKeyProvider()
	first, err := p.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := first.Codec.Encrypt([]byte("private-body"))
	if err != nil {
		t.Fatal(err)
	}
	again, err := p.Snapshot(t.Context())
	if err != nil || again.Generation != first.Generation {
		t.Fatal("unstable revision")
	}
	t.Setenv("AI_AGENT_APPROVAL_ENCRYPTION_KEY", newValue)
	t.Setenv("AI_AGENT_APPROVAL_ENCRYPTION_PREVIOUS_KEYS", old)
	rotated, err := p.Snapshot(t.Context())
	if err != nil || rotated.Generation == first.Generation {
		t.Fatal("rotation not observed")
	}
	if _, err := rotated.Codec.Decrypt(ciphertext); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AI_AGENT_APPROVAL_ENCRYPTION_PREVIOUS_KEYS", "")
	revoked, err := p.Snapshot(t.Context())
	if err != nil || revoked.Generation == rotated.Generation {
		t.Fatal("revocation not observed")
	}
	if _, err := revoked.Codec.Decrypt(ciphertext); err == nil {
		t.Fatal("revoked historical key accepted")
	}
	fresh, err := NewEnvironmentReadOnlyKeyProvider().Snapshot(t.Context())
	if err != nil || fresh.Generation == revoked.Generation {
		t.Fatal("new provider reused revision")
	}
}

func TestEnvironmentReadOnlyKeysRejectsInvalidAndCancelled(t *testing.T) {
	good := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x44}, 32))
	for _, fixture := range [][2]string{{"", ""}, {"private-invalid", ""}, {base64.StdEncoding.EncodeToString([]byte("short")), ""}, {good, "private-invalid"}, {good, good + ","}, {good, strings.Repeat(good+",", 33)}, {strings.Repeat("x", 129), ""}} {
		t.Setenv("AI_AGENT_APPROVAL_ENCRYPTION_KEY", fixture[0])
		t.Setenv("AI_AGENT_APPROVAL_ENCRYPTION_PREVIOUS_KEYS", fixture[1])
		s, err := NewEnvironmentReadOnlyKeyProvider().Snapshot(t.Context())
		if !errors.Is(err, ErrReadOnlyKeys) || s.Codec != nil || strings.Contains(err.Error(), "private") {
			t.Fatal("invalid provider result")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := NewEnvironmentReadOnlyKeyProvider().Snapshot(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestEnvironmentReadOnlyKeysConcurrentSnapshots(t *testing.T) {
	t.Setenv("AI_AGENT_APPROVAL_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x44}, 32)))
	t.Setenv("AI_AGENT_APPROVAL_ENCRYPTION_PREVIOUS_KEYS", "")
	p := NewEnvironmentReadOnlyKeyProvider()
	s, err := p.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 10 {
				current, err := p.Snapshot(t.Context())
				if err != nil || current.Generation != s.Generation {
					t.Error("concurrent snapshot changed")
				}
			}
		})
	}
	wg.Wait()
}
