package tests

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildScriptOutput(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("build.sh requires bash")
	}
	for _, tc := range []struct {
		name     string
		existing string
		platform string
		wantErr  string
	}{
		{name: "new output"},
		{name: "existing output", existing: "."},
		{name: "other version", existing: "ai-agent-1.0.1-linux-amd64.tar.gz"},
		{name: "version directory", existing: "ai-agent-1.0.2-linux-amd64/", wantErr: "版本包"},
		{name: "linux archive", existing: "ai-agent-1.0.2-linux-amd64.tar.gz", wantErr: "版本包"},
		{name: "windows archive", existing: "ai-agent-1.0.2-windows-amd64.zip", platform: "windows/amd64", wantErr: "版本包"},
		{name: "later platform conflict", existing: "ai-agent-1.0.2-windows-amd64.zip", platform: "linux/amd64,windows/amd64", wantErr: "版本包"},
		{name: "dangling archive symlink", existing: "symlink", wantErr: "版本包"},
		{name: "output file", existing: "file", wantErr: "不是目录"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			write := func(path, contents string, mode os.FileMode) {
				t.Helper()
				if err := os.WriteFile(path, []byte(contents), mode); err != nil {
					t.Fatal(err)
				}
			}
			mkdir := func(path string) {
				t.Helper()
				if err := os.MkdirAll(path, 0755); err != nil {
					t.Fatal(err)
				}
			}
			script, err := os.ReadFile("../build.sh")
			if err != nil {
				t.Fatal(err)
			}
			write(filepath.Join(root, "build.sh"), string(script), 0755)
			write(filepath.Join(root, "config.yaml"), "config fixture", 0644)
			write(filepath.Join(root, "teams.yaml"), "teams fixture", 0644)
			mkdir(filepath.Join(root, "skills"))
			write(filepath.Join(root, "skills", "fixture.md"), "skill fixture", 0644)
			bin := filepath.Join(root, "bin")
			mkdir(bin)
			// Fake only compilation; exercise the real script and tar packaging.
			write(filepath.Join(bin, "go"), `#!/usr/bin/env bash
set -eu
if [[ "$*" == "tool dist list" ]]; then
  printf 'linux/amd64\nwindows/amd64\n'
  exit 0
fi
touch "$BUILD_TEST_MARKER"
while [[ $# -gt 0 ]]; do
  if [[ "$1" == "-o" ]]; then
    printf 'binary fixture' > "$2"
    exit 0
  fi
  shift
done
exit 1
`, 0755)
			output := filepath.Join(root, "dist", "linux")
			if tc.existing == "file" {
				mkdir(filepath.Dir(output))
				write(output, "preserve", 0644)
			} else if tc.existing != "" {
				mkdir(output)
				switch {
				case tc.existing == ".":
				case tc.existing == "symlink":
					err := os.Symlink("missing", filepath.Join(output, "ai-agent-1.0.2-linux-amd64.tar.gz"))
					if err != nil {
						t.Fatal(err)
					}
				case strings.HasSuffix(tc.existing, "/"):
					mkdir(filepath.Join(output, tc.existing))
				default:
					write(filepath.Join(output, tc.existing), "preserve", 0644)
				}
			}
			platform := tc.platform
			if platform == "" {
				platform = "linux/amd64"
			}
			marker := filepath.Join(root, "compiled")
			cmd := exec.Command("bash", "build.sh", platform, "-v", "1.0.2", "-o", "dist/linux")
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "BUILD_TEST_MARKER="+marker)
			got, err := cmd.CombinedOutput()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(string(got), tc.wantErr) {
					t.Fatalf("error = %v, output = %s; want %q", err, got, tc.wantErr)
				}
				if _, err := os.Stat(marker); !os.IsNotExist(err) {
					t.Fatal("conflicting output must be rejected before compilation")
				}
				if tc.existing != "file" {
					entries, err := os.ReadDir(output)
					if err != nil || len(entries) != 1 {
						t.Fatalf("conflict changed output directory: %v, %v", entries, err)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("build failed: %v\n%s", err, got)
			}
			archive := filepath.Join(output, "ai-agent-1.0.2-linux-amd64.tar.gz")
			if info, err := os.Stat(archive); err != nil || info.Size() == 0 {
				t.Fatalf("missing or empty archive: %v", err)
			}
			if tc.existing != "" && tc.existing != "." {
				contents, err := os.ReadFile(filepath.Join(output, tc.existing))
				if err != nil || string(contents) != "preserve" {
					t.Fatalf("existing package changed: %q, %v", contents, err)
				}
			}
		})
	}
}
