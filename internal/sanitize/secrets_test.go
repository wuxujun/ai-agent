package sanitize

import (
	"strings"
	"testing"
)

func TestSecrets(t *testing.T) {
	input := "OPENAI_API_KEY=sk-abcdefghijklmnopqrstuvwxyz\nAuthorization: Bearer token.secret.value"
	result := Secrets(input)
	if strings.Contains(result, "abcdefghijklmnopqrstuvwxyz") || strings.Contains(result, "token.secret.value") {
		t.Fatalf("secret was not redacted: %s", result)
	}
}
