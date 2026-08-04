// Package buildinfo exposes immutable metadata embedded in release binaries.
package buildinfo

import (
	"runtime/debug"
	"strings"
)

// Version is overridden by release builds with:
//
//	-X github.com/wuxujun/ai-agent/internal/buildinfo.Version=<version>
//
// Keep a non-empty development value so every log record has app_version.
var Version = "dev"

// Current returns the embedded release version. Module builds may provide a
// version through Go build information; local source builds fall back to dev.
func Current() string {
	if version := strings.TrimSpace(Version); version != "" && version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		if version := strings.TrimSpace(info.Main.Version); version != "" && version != "(devel)" {
			return version
		}
	}
	return "dev"
}
