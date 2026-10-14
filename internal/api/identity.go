package api

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode"
)

// Only verified provider metadata reaches this identity boundary. No raw
// subject or credential is persisted in the introspection cache or audit ID.
type bearerIdentity struct {
	TenantID string
	ActorID  string
}

func personalActorID(kind, tenant, subject string) string {
	if subject == "" || len(subject) > 256 || subject != strings.TrimSpace(subject) {
		return ""
	}
	for _, r := range subject {
		if unicode.IsControl(r) {
			return ""
		}
	}
	digest := sha256.Sum256([]byte(tenant + "\x00" + subject))
	return fmt.Sprintf("%s-sub:%x", kind, digest[:12])
}

func (identity bearerIdentity) personal() bool {
	for _, prefix := range []string{"jwt-sub:", "introspection-sub:"} {
		if !strings.HasPrefix(identity.ActorID, prefix) {
			continue
		}
		digest := strings.TrimPrefix(identity.ActorID, prefix)
		_, err := hex.DecodeString(digest)
		return err == nil && len(digest) == 24 && strings.ToLower(digest) == digest
	}
	return false
}
