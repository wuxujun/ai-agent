// Package commandenv bounds the environment inherited by task subprocesses.
// It does not restrict filesystem, network, or process access.
package commandenv

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

var baseline = []string{"PATH", "LANG", "LC_ALL", "LC_CTYPE", "LC_MESSAGES", "TZ", "TMPDIR", "TMP", "TEMP", "SYSTEMROOT", "WINDIR", "COMSPEC", "PATHEXT"}

// ValidateAllowlist accepts explicit environment names only. Secret-bearing
// families, credential agents, and loader/interpreter injection settings cannot
// be enabled. Operator-defined names still require review of their values.
func ValidateAllowlist(names []string) error {
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		upper := strings.ToUpper(name)
		if !validName(name) || seen[upper] || forbidden(upper) {
			return errors.New("tool.environment_allowlist contains an invalid, duplicate, reserved, or sensitive name")
		}
		seen[upper] = true
	}
	return nil
}

func validName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for i, r := range name {
		if r != '_' && !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(i > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func forbidden(upper string) bool {
	switch upper {
	case "HOME", "USERPROFILE", "PWD", "GIT_CONFIG_NOSYSTEM", "GIT_CONFIG_GLOBAL":
		return true // Set by Build, never inherited from the service.
	case "ENV", "BASH_ENV", "SHELLOPTS", "BASHOPTS", "CDPATH", "NODE_OPTIONS", "NODE_PATH", "PYTHONPATH", "PYTHONHOME", "RUBYOPT", "RUBYLIB", "PERL5OPT", "PERL5LIB", "GOENV", "GOFLAGS", "GOPROXY", "SSH_AGENT_PID", "PGPASSFILE", "PGSERVICE", "PGSERVICEFILE", "DATABASE_URL", "REDIS_URL", "PGPASSWORD", "MYSQL_PWD", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "FTP_PROXY", "NETRC", "GIT_SSH", "GIT_SSH_COMMAND", "KRB5CCNAME":
		return true
	}
	for _, prefix := range []string{"AI_AGENT_", "OPENAI_", "GEMINI_", "GOOGLE_", "LANGFUSE_", "AWS_", "AZURE_", "LD_", "DYLD_", "BASH_FUNC_", "GIT_CONFIG_", "GIT_ASKPASS", "SSH_ASKPASS"} {
		if strings.HasPrefix(upper, prefix) {
			return true
		}
	}
	for _, part := range strings.Split(upper, "_") {
		switch part {
		case "KEY", "TOKEN", "SECRET", "PASSWORD", "CREDENTIAL", "CREDENTIALS", "AUTH", "AUTHORIZATION", "DSN":
			return true
		}
	}
	return false
}

// Build constructs a non-nil environment, even if the service has no baseline
// variables. HOME and Git global configuration are isolated from the service.
func Build(parent []string, dir string, extra []string) ([]string, error) {
	if err := ValidateAllowlist(extra); err != nil {
		return nil, err
	}
	workspace, err := filepath.Abs(dir)
	if err != nil {
		return nil, errors.New("cannot determine subprocess working directory")
	}
	normalize := func(name string) string {
		if runtime.GOOS == "windows" {
			return strings.ToUpper(name)
		}
		return name
	}
	allowed := make(map[string]bool, len(baseline)+len(extra))
	for _, name := range baseline {
		allowed[normalize(name)] = true
	}
	for _, name := range extra {
		allowed[normalize(name)] = true
	}
	values := make(map[string]string, len(allowed)+5)
	for _, entry := range parent {
		name, value, ok := strings.Cut(entry, "=")
		name = normalize(name)
		if ok && allowed[name] && !strings.ContainsRune(value, '\x00') {
			values[name] = value
		}
	}
	values["HOME"], values["USERPROFILE"], values["PWD"] = workspace, workspace, workspace
	values["GIT_CONFIG_NOSYSTEM"], values["GIT_CONFIG_GLOBAL"] = "1", os.DevNull
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]string, 0, len(values))
	for _, name := range names {
		result = append(result, name+"="+values[name])
	}
	return result, nil
}
