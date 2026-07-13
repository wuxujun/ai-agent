package llm

import (
	"crypto/sha256"
	"encoding/hex"
)

func credentialFingerprint(credential string) string {
	sum := sha256.Sum256([]byte(credential))
	return hex.EncodeToString(sum[:8])
}
