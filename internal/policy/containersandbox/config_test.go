package containersandbox

import (
	"math"
	"path/filepath"
	"strings"
	"testing"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	return Config{Mode: "docker", SocketPath: filepath.Join(t.TempDir(), "d.sock"), Image: "sha256:" + strings.Repeat("a", 64), User: "65532:65532", CPUs: 1, MemoryMiB: 256, PidsLimit: 64, TmpfsMiB: 64}
}

func TestSandboxConfig(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Config)
	}{
		{"unknown driver", func(c *Config) { c.Mode = "fallback" }},
		{"relative socket", func(c *Config) { c.SocketPath = "docker.sock" }},
		{"remote endpoint", func(c *Config) { c.SocketPath = "tcp://example.invalid:2375" }},
		{"mutable image", func(c *Config) { c.Image = "image:latest" }},
		{"short image id", func(c *Config) { c.Image = "sha256:abcdef" }},
		{"root user", func(c *Config) { c.User = "0:1000" }},
		{"root group", func(c *Config) { c.User = "1000:0" }},
		{"named user", func(c *Config) { c.User = "nobody" }},
		{"special uid", func(c *Config) { c.User = "4294967295:1000" }},
		{"unlimited cpu", func(c *Config) { c.CPUs = 0 }},
		{"nan cpu", func(c *Config) { c.CPUs = math.NaN() }},
		{"unlimited memory", func(c *Config) { c.MemoryMiB = 0 }},
		{"unlimited processes", func(c *Config) { c.PidsLimit = 0 }},
		{"unbounded tmpfs", func(c *Config) { c.TmpfsMiB = 512 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t)
			tc.change(&cfg)
			if cfg.Validate() == nil {
				t.Fatal("unsafe sandbox configuration accepted")
			}
		})
	}
	if err := testConfig(t).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (Config{}).Validate(); err != nil {
		t.Fatal("legacy local default rejected")
	}
}
