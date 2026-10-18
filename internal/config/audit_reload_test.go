package config

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/wuxujun/ai-agent/internal/logger"
)

func TestLoggingReloadAppliesAcrossEntrypointsAndRollsBackFailure(t *testing.T) {
	t.Setenv("TEST_NO_CONFIG", "")
	t.Setenv("AI_AGENT_LOG_LEVEL", "")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	write := func(level, directory string, file bool) {
		t.Helper()
		data := fmt.Sprintf("log:\n  level: %q\n  console: true\n  file_enabled: %t\n  access_enabled: false\n  directory: %q\n", level, file, directory)
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("info", dir, false)
	t.Setenv("AI_AGENT_CONFIG_FILE", path)
	resetConfig()
	defer resetConfig()
	initial := LoadConfig()
	if err := ConfigureLogging(initial); err != nil {
		t.Fatal(err)
	}
	defer ConfigureLogging(initial)
	component := logger.Component("reload-test")
	if component.Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("debug enabled initially")
	}
	write("debug", dir, false)
	reloadWatchedConfig(nil)
	if !component.Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("file watcher did not update logger")
	}
	write("error", dir, false)
	if _, _, err := Reload(); err != nil {
		t.Fatal(err)
	}
	if component.Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("manual reload did not update logger")
	}
	previous := Get()
	revision := Revision()
	blocked := filepath.Join(dir, "regular-file")
	if err := os.WriteFile(blocked, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	write("debug", blocked, true)
	if _, _, err := Reload(); err == nil {
		t.Fatal("unopenable log directory accepted")
	}
	if Get() != previous || Revision() != revision || component.Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("failed logging reload changed snapshot, revision or logger")
	}
}
func TestJWTConfigRejectsPlaintextJWKS(t *testing.T) {
	cfg := &Config{}
	cfg.LLM.Provider = "openai"
	cfg.LLM.TimeoutSeconds = 30
	cfg.API.Auth = APIAuthConfig{Mode: "jwt", JWT: APIJWTConfig{Issuer: "https://issuer.example", Audience: "agent", JWKSURL: "http://issuer.example/jwks", TenantClaim: "code", AllowedAlgorithms: []string{"RS256"}, ClockSkewSeconds: 30, JWKSCacheTTLSeconds: 300, JWKSRequestTimeoutSeconds: 5}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("plaintext JWKS accepted")
	}
	cfg.API.Auth.JWT.JWKSURL = "https://issuer.example/jwks"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}
