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

func TestSecretsRedactsInlineApprovalValues(t *testing.T) {
	input := "options=map[api_key: deep-secret] --token=cli-secret --password more-secret"
	result := Secrets(input)
	for _, secret := range []string{"deep-secret", "cli-secret", "more-secret"} {
		if strings.Contains(result, secret) {
			t.Fatalf("inline secret %q was not redacted: %s", secret, result)
		}
	}
}
