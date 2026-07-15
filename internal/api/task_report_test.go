package api

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateTaskReportTextPreservesUTF8(t *testing.T) {
	value := strings.Repeat("陈园青", 101)
	got := truncateTaskReportText(value, 300)
	if !utf8.ValidString(got) {
		t.Fatal("truncated task report text is not valid UTF-8")
	}
	if !strings.HasSuffix(got, "... (truncated)") {
		t.Fatalf("expected truncation suffix, got %q", got)
	}
}
