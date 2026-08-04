package buildinfo

import (
	"os"
	"testing"
)

func TestCurrentUsesEmbeddedVersion(t *testing.T) {
	original := Version
	Version = "  v1.2.3  "
	t.Cleanup(func() { Version = original })
	if got := Current(); got != "v1.2.3" {
		t.Fatalf("Current() = %q", got)
	}
}

func TestCurrentMatchesInjectedVersion(t *testing.T) {
	expected := os.Getenv("EXPECTED_APP_VERSION")
	if expected == "" {
		t.Skip("linker injection check is opt-in")
	}
	if got := Current(); got != expected {
		t.Fatalf("Current() = %q, want linker-injected %q", got, expected)
	}
}
