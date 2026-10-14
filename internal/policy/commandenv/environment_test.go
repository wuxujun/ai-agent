package commandenv

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestBuildOmitsSecretsAndServiceHome(t *testing.T) {
	dir := t.TempDir()
	parent := []string{"PATH=/usr/bin:/bin", "LANG=C", "HOME=/service-home", "USERPROFILE=/service-home", "PWD=/service-home", "OPENAI_API_KEY=fixture-secret", "AI_AGENT_APPROVAL_ENCRYPTION_KEY=fixture-secret", "AWS_SESSION_TOKEN=fixture-secret", "SSH_AUTH_SOCK=/agent", "BASH_ENV=/startup-script", "NODE_OPTIONS=--require /startup-script", "UNREVIEWED=fixture-secret", "SAFE_BUILD_LABEL=reviewed"}
	result, err := Build(parent, dir, []string{"SAFE_BUILD_LABEL"})
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	for _, entry := range result {
		name, value, _ := strings.Cut(entry, "=")
		values[name] = value
	}
	if values["PATH"] != "/usr/bin:/bin" || values["LANG"] != "C" || values["SAFE_BUILD_LABEL"] != "reviewed" {
		t.Fatal("baseline or reviewed settings lost")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"HOME", "USERPROFILE", "PWD"} {
		if values[name] != abs {
			t.Fatalf("%s was not confined to workspace", name)
		}
	}
	if values["GIT_CONFIG_GLOBAL"] != os.DevNull || values["GIT_CONFIG_NOSYSTEM"] != "1" {
		t.Fatal("service Git configuration remains enabled")
	}
	for _, name := range []string{"OPENAI_API_KEY", "AI_AGENT_APPROVAL_ENCRYPTION_KEY", "AWS_SESSION_TOKEN", "SSH_AUTH_SOCK", "BASH_ENV", "NODE_OPTIONS", "UNREVIEWED"} {
		if _, ok := values[name]; ok {
			t.Fatalf("unexpected variable name %s", name)
		}
	}
	if strings.Contains(strings.Join(result, "\n"), "fixture-secret") {
		t.Fatal("a service secret was inherited")
	}
	empty, err := Build(nil, dir, nil)
	if err != nil || empty == nil || len(empty) == 0 {
		t.Fatal("empty parent resulted in implicit environment inheritance")
	}
}

func TestAllowlistCannotEnableSecretsOrInjection(t *testing.T) {
	for _, name := range []string{"OPENAI_API_KEY", "openai_api_key", "AI_AGENT_API_KEY", "GEMINI_API_KEY", "GOOGLE_APPLICATION_CREDENTIALS", "AWS_PROFILE", "LANGFUSE_PUBLIC_KEY", "SSH_AUTH_SOCK", "DATABASE_URL", "PGPASSFILE", "PGPASSWORD", "MYSQL_PWD", "HTTPS_PROXY", "GIT_SSH_COMMAND", "KRB5CCNAME", "LD_PRELOAD", "DYLD_INSERT_LIBRARIES", "BASH_ENV", "ENV", "NODE_OPTIONS", "PYTHONPATH", "GOFLAGS", "GOPROXY", "GIT_CONFIG_COUNT", "GIT_CONFIG_GLOBAL", "HOME", "USERPROFILE", "PWD", "CUSTOM_SECRET", "CUSTOM_TOKEN", "CUSTOM_AUTHORIZATION", "CUSTOM_DSN", "VALUE=x", " WITH_SPACE", "", "1INVALID"} {
		if ValidateAllowlist([]string{name}) == nil {
			t.Fatalf("blocked variable name accepted: %q", name)
		}
	}
	if ValidateAllowlist([]string{"SAFE_LABEL", "safe_label"}) == nil {
		t.Fatal("case-insensitive duplicate accepted")
	}
	if err := ValidateAllowlist([]string{"GOCACHE", "GOMODCACHE", "GOROOT", "GOTOOLCHAIN", "SAFE_LABEL"}); err != nil {
		t.Fatal(err)
	}
}

func TestBuildIsDeterministicAndDoesNotMutateInputs(t *testing.T) {
	parent := []string{"PATH=/first", "SAFE=reviewed", "PATH=/last", "INVALID_ENTRY"}
	before := append([]string(nil), parent...)
	extra := []string{"SAFE"}
	first, err := Build(parent, ".", extra)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Build(parent, ".", extra)
	if err != nil || !reflect.DeepEqual(first, second) || !reflect.DeepEqual(parent, before) || !reflect.DeepEqual(extra, []string{"SAFE"}) {
		t.Fatal("environment construction mutated inputs or was nondeterministic")
	}
	if !strings.Contains(strings.Join(first, "\n"), "PATH=/last") {
		t.Fatal("last inherited PATH value was lost")
	}
	if result, err := Build(parent, ".", []string{"OPENAI_API_KEY"}); err == nil || result != nil {
		t.Fatal("invalid runtime policy did not fail closed")
	}
}
