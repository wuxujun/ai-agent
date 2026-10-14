package tools

import (
	"strings"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
)

func TestRunCommandOmitsServiceCredentialsAndStartupHooks(t *testing.T) {
	if _, err := exec_LookPath("sh"); err != nil {
		t.Skip("sh is unavailable")
	}
	t.Cleanup(config.OverrideForTesting(func(c *config.Config) { c.Tool.EnvironmentAllowlist = nil }))
	for _, name := range []string{"OPENAI_API_KEY", "AI_AGENT_API_KEY", "AI_AGENT_APPROVAL_ENCRYPTION_KEY", "AI_AGENT_CONSOLE_SESSION_KEY", "AWS_SESSION_TOKEN", "SSH_AUTH_SOCK", "BASH_ENV", "ENV", "NODE_OPTIONS", "UNREVIEWED_VALUE"} {
		t.Setenv(name, "fixture-must-not-be-inherited")
	}
	script := `for name in OPENAI_API_KEY AI_AGENT_API_KEY AI_AGENT_APPROVAL_ENCRYPTION_KEY AI_AGENT_CONSOLE_SESSION_KEY AWS_SESSION_TOKEN SSH_AUTH_SOCK BASH_ENV ENV NODE_OPTIONS UNREVIEWED_VALUE; do eval 'value=${'"$name"'-}'; test -z "$value" || exit 9; done; test "$HOME" = "$PWD" || exit 10; test "$GIT_CONFIG_NOSYSTEM" = 1 || exit 11; printf isolated`
	output, err := RunCommand(t.Context(), t.TempDir(), "sh", "-c", script)
	if err != nil || output != "isolated" {
		t.Fatal("subprocess inherited credentials/startup hooks or lost workspace HOME")
	}
}

func TestRunCommandReadsCurrentEnvironmentAllowlist(t *testing.T) {
	if _, err := exec_LookPath("sh"); err != nil {
		t.Skip("sh is unavailable")
	}
	t.Setenv("SAFE_BUILD_LABEL", "reviewed")
	restore := config.OverrideForTesting(func(c *config.Config) { c.Tool.EnvironmentAllowlist = []string{"SAFE_BUILD_LABEL"} })
	t.Cleanup(restore)
	output, err := RunCommand(t.Context(), t.TempDir(), "sh", "-c", `printf '%s' "${SAFE_BUILD_LABEL-}"`)
	if err != nil || output != "reviewed" {
		t.Fatal("reviewed variable was not inherited")
	}
	restore()
	t.Cleanup(config.OverrideForTesting(func(c *config.Config) { c.Tool.EnvironmentAllowlist = nil }))
	output, err = RunCommand(t.Context(), t.TempDir(), "sh", "-c", `printf '%s' "${SAFE_BUILD_LABEL-}"`)
	if err != nil || output != "" {
		t.Fatal("new commands ignored allowlist revocation")
	}
	t.Cleanup(config.OverrideForTesting(func(c *config.Config) { c.Tool.EnvironmentAllowlist = []string{"OPENAI_API_KEY"} }))
	output, err = RunCommand(t.Context(), t.TempDir(), "sh", "-c", `printf should-not-run`)
	if err == nil || output != "" || strings.Contains(err.Error(), "should-not-run") {
		t.Fatal("invalid process policy did not prevent execution")
	}
}
