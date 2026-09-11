package config

import (
	"errors"
	"os"
	"testing"
)

func TestRestartGuardCoversStartupBindingsAndAllowsDynamicSettings(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Config)
	}{
		{"wiki_directory", func(c *Config) { c.Wiki.Directory = "new-wiki" }},
		{"wiki_timeout", func(c *Config) { c.Wiki.TimeoutSeconds = 60 }},
		{"wiki_private_network", func(c *Config) { c.Wiki.AllowPrivateNetwork = true }},
		{"mcp", func(c *Config) { c.MCP.Servers = []MCPServerConfig{{Name: "new-server"}} }},
		{"store_dsn", func(c *Config) { c.Store.DSN = "new.db" }},
		{"store_type", func(c *Config) { c.Store.Type = "redis" }},
		{"http_address", func(c *Config) { c.API.Addr = ":8089" }},
		{"skill_root", func(c *Config) { c.Skill.Root = "new-skills" }},
		{"telemetry", func(c *Config) { c.Telemetry.Enabled = true }},
		{"engine_mode", func(c *Config) { c.Orchestrator.Mode = "legacy" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old, next := &Config{}, &Config{}
			tc.mutate(next)
			if err := validateRestartRequiredReload(old, next); !errors.Is(err, ErrRestartRequired) {
				t.Fatalf("startup change accepted: %v", err)
			}
		})
	}
	old, next := &Config{}, &Config{}
	next.Wiki.SearchTopK = 4
	next.Wiki.FetchMaxBytes = 1000
	next.Wiki.FetchMaxItems = 2
	next.Wiki.DefaultSpace = "dynamic-space"
	next.Wiki.CandidateCacheMaxTasks = 50
	next.Wiki.CandidateCacheTTLSeconds = 60
	next.Wiki.CircuitBreakerFailureThreshold = 3
	next.Wiki.CircuitBreakerCooldownSeconds = 10
	next.LLM.Model = "dynamic-model"
	next.Tool.TimeoutSeconds = 30
	next.Orchestrator.MaxConcurrentTasks = 7
	next.Log.Level = "debug"
	next.Store.MemoryCandidateLimit = 100
	next.Store.VectorSearch = "pgvector"
	next.Store.PGVectorDimensions = 16
	if err := validateRestartRequiredReload(old, next); err != nil {
		t.Fatalf("dynamic settings rejected: %v", err)
	}
}
func TestReloadRejectsStartupBindingWithoutChangingRevision(t *testing.T) {
	path, before := loadBrainReloadFixture(t)
	previousRevision := configRevision
	candidate := append(brainReloadConfig("./data/brain"), []byte("\nwiki:\n  directory: new-wiki\n")...)
	if err := os.WriteFile(path, candidate, 0644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Reload(); !errors.Is(err, ErrRestartRequired) {
		t.Fatalf("reload accepted startup binding: %v", err)
	}
	if Get() != before || configRevision != previousRevision {
		t.Fatal("rejected reload replaced config or revision")
	}
	candidate = append(brainReloadConfig("./data/brain"), []byte("\nwiki:\n  search_top_k: 9\n")...)
	if err := os.WriteFile(path, candidate, 0644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Reload(); err != nil {
		t.Fatal(err)
	}
	if Get().Wiki.SearchTopK != 9 || configRevision == previousRevision {
		t.Fatal("dynamic reload did not apply")
	}
}

func TestRestartGuardTreatsEmptyMCPListsAsEquivalent(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		old, next := &Config{}, &Config{}
		next.MCP.Servers = []MCPServerConfig{}
		if reverse {
			old, next = next, old
		}
		if err := validateRestartRequiredReload(old, next); err != nil {
			t.Fatalf("empty MCP list changed startup bindings: %v", err)
		}
	}
}
