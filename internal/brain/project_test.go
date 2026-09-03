package brain_test

import (
	"strings"
	"testing"

	"github.com/wuxujun/ai-agent/internal/brain"
	"github.com/wuxujun/ai-agent/internal/config"
)

func brainConfigFixture() *config.Config {
	cfg := &config.Config{}
	cfg.Brain = config.BrainConfig{Enabled: true, Root: "./data/brain"}
	cfg.API.Tenants = map[string]config.APITenantConfig{
		"tenant-a": {BrainProjects: map[string]config.BrainProjectConfig{"atlas": {WikiSpace: "brain-atlas"}}},
	}
	return cfg
}

func TestResolveProjectRejectsCrossTenantAndInvalidSlug(t *testing.T) {
	cfg := brainConfigFixture()
	if _, err := brain.ResolveProject(cfg, "tenant-b", "atlas"); err == nil {
		t.Fatal("expected tenant isolation error")
	}
	if _, err := brain.ResolveProject(cfg, "tenant-a", "../atlas"); err == nil {
		t.Fatal("expected slug error")
	}
}

func TestResolveProjectReturnsExplicitTenantProjectScope(t *testing.T) {
	ref, err := brain.ResolveProject(brainConfigFixture(), "tenant-a", "atlas")
	if err != nil {
		t.Fatal(err)
	}
	if ref.TenantID != "tenant-a" || ref.ProjectID != "atlas" || ref.WikiSpace != "brain-atlas" {
		t.Fatalf("project ref = %+v", ref)
	}
	if !strings.HasPrefix(ref.StorageKey, "sha256:") || len(ref.StorageKey) != len("sha256:")+64 {
		t.Fatalf("storage key = %q", ref.StorageKey)
	}
}

func TestProjectConfigDigestIsStableAndScopeBound(t *testing.T) {
	ref, err := brain.ResolveProject(brainConfigFixture(), "tenant-a", "atlas")
	if err != nil {
		t.Fatal(err)
	}
	got := brain.ProjectConfigDigest(ref)
	if !strings.HasPrefix(got, "sha256:") || len(got) != len("sha256:")+64 {
		t.Fatalf("digest = %q", got)
	}
	if got != brain.ProjectConfigDigest(ref) {
		t.Fatal("digest is not stable")
	}
	ref.ProjectID = "orbit"
	if got == brain.ProjectConfigDigest(ref) {
		t.Fatal("digest did not bind project scope")
	}
}
