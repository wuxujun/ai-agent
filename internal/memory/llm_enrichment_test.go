package memory

import "testing"

func TestTruncateLLMText(t *testing.T) {
	if got := truncateLLMText("  query  ", 10); got != "query" {
		t.Fatalf("trimmed value = %q", got)
	}
	if got := truncateLLMText("123456", 4); got != "1234" {
		t.Fatalf("truncated value = %q", got)
	}
}
