package llm

import "testing"

func TestLiteLLMHealthURL(t *testing.T) {
	got, err := liteLLMHealthURL("http://litellm:4000/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	if got != "http://litellm:4000/health" {
		t.Fatalf("health URL = %q", got)
	}
}
