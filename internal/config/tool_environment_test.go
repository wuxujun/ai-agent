package config

import (
	"os"
	"strings"
	"testing"

	"github.com/spf13/viper"
)

func TestToolEnvironmentReloadAndSnapshotIsolation(t *testing.T) {
	path, _ := loadBrainReloadFixture(t)
	write := func(name string) {
		t.Helper()
		body := append(brainReloadConfig("./data/brain"), []byte("\ntool:\n  environment_allowlist: ["+name+"]\n")...)
		if err := os.WriteFile(path, body, 0644); err != nil {
			t.Fatal(err)
		}
	}
	write("GOCACHE")
	if _, changes, err := Reload(); err != nil || !strings.Contains(strings.Join(changes, "\n"), "tool.environment_allowlist") {
		t.Fatalf("valid policy reload failed: %v", err)
	}
	before, revision := Get(), Revision()
	cloned := cloneConfig(before)
	cloned.Tool.EnvironmentAllowlist[0] = "changed"
	if before.Tool.EnvironmentAllowlist[0] != "GOCACHE" {
		t.Fatal("clone shared environment policy backing array")
	}
	write("OPENAI_API_KEY")
	if _, _, err := Reload(); err == nil {
		t.Fatal("secret inheritance candidate accepted")
	}
	if Get() != before || Revision() != revision {
		t.Fatal("invalid policy replaced active configuration")
	}
	write("GOMODCACHE")
	if _, _, err := Reload(); err != nil || Get().Tool.EnvironmentAllowlist[0] != "GOMODCACHE" {
		t.Fatal("valid policy revocation/replacement failed")
	}
}

func TestToolEnvironmentEnvBinding(t *testing.T) {
	resetConfig()
	t.Cleanup(resetConfig)
	setupViper()
	t.Setenv("AI_AGENT_TOOL_ENVIRONMENT_ALLOWLIST", "GOCACHE,GOMODCACHE")
	cfg, err := unmarshalConfig()
	if err != nil || len(cfg.Tool.EnvironmentAllowlist) != 2 || cfg.Tool.EnvironmentAllowlist[0] != "GOCACHE" || cfg.Tool.EnvironmentAllowlist[1] != "GOMODCACHE" {
		t.Fatalf("allowlist binding failed: %v", err)
	}
	viper.Set("tool.environment_allowlist", []string{"CUSTOM_TOKEN"})
	cfg, err = unmarshalConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err = cfg.Validate(); err == nil {
		t.Fatal("sensitive variable enabled by config")
	}
}
