package buildinfo

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// Execute the workflow's actual archive commands against a staged fixture.
// Compilation is deliberately outside this regression's scope.
func TestReleaseArchivesFromCleanDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("release workflow runs on Ubuntu and requires Unix archive tools")
	}
	for _, tool := range []string{"bash", "tar", "zip"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("release regression requires %s: %v", tool, err)
		}
	}
	data, err := os.ReadFile("../../.github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Strategy struct {
				Matrix struct {
					Include []struct {
						OS           string `yaml:"os"`
						ArtifactName string `yaml:"artifact_name"`
						AssetName    string `yaml:"asset_name"`
					} `yaml:"include"`
				} `yaml:"matrix"`
			} `yaml:"strategy"`
			Steps []struct{ Name, Run string } `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	job := workflow.Jobs["build-release"]
	var script string
	for _, step := range job.Steps {
		if step.Name == "Create Archive (.tar.gz / .zip)" {
			script = step.Run
		}
	}
	if script == "" || len(job.Strategy.Matrix.Include) == 0 {
		t.Fatal("release archive step or platform matrix is missing")
	}
	for _, platform := range job.Strategy.Matrix.Include {
		t.Run(platform.ArtifactName, func(t *testing.T) {
			root := t.TempDir()
			binary := "server"
			if platform.OS == "windows" {
				binary += ".exe"
			}
			files := map[string]string{
				binary: "fixture binary", "config.yaml": "fixture config",
				"teams.yaml": "fixture teams", "skills/example/SKILL.md": "fixture skill",
			}
			if platform.OS == "linux" {
				files["ai-agent.service"] = "fixture service"
			}
			stage := filepath.Join(root, "build_stage", platform.ArtifactName)
			for name, content := range files {
				path := filepath.Join(stage, filepath.FromSlash(name))
				if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(content), 0644); err != nil {
					t.Fatal(err)
				}
			}
			for _, name := range []string{"data", "logs", "workspace"} {
				if err := os.Mkdir(filepath.Join(stage, name), 0755); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := os.Stat(filepath.Join(root, "dist")); !os.IsNotExist(err) {
				t.Fatalf("fixture must start without dist: %v", err)
			}
			rendered := strings.NewReplacer(
				"${{ matrix.os }}", platform.OS,
				"${{ matrix.artifact_name }}", platform.ArtifactName,
				"${{ matrix.asset_name }}", platform.AssetName,
			).Replace(script)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "bash", "-e", "-o", "pipefail", "-c", rendered)
			cmd.Dir = root
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("archive failed: %v\n%s", err, output)
			}
			contents := readReleaseArchive(t, filepath.Join(root, "dist", platform.AssetName))
			for name, want := range files {
				if got := contents[platform.ArtifactName+"/"+name]; got != want {
					t.Errorf("archive entry %s = %q, want %q", name, got, want)
				}
			}
			for _, name := range []string{"data", "logs", "workspace"} {
				if _, ok := contents[platform.ArtifactName+"/"+name+"/"]; !ok {
					t.Errorf("archive directory %s is missing", name)
				}
			}
		})
	}
}

func readReleaseArchive(t *testing.T, path string) map[string]string {
	t.Helper()
	contents := make(map[string]string)
	read := func(name string, reader io.Reader) {
		data, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		contents[name] = string(data)
	}
	if strings.HasSuffix(path, ".zip") {
		archive, err := zip.OpenReader(path)
		if err != nil {
			t.Fatal(err)
		}
		defer archive.Close()
		for _, file := range archive.File {
			reader, err := file.Open()
			if err != nil {
				t.Fatal(err)
			}
			read(file.Name, reader)
			reader.Close()
		}
		return contents
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	archive := tar.NewReader(gz)
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		read(header.Name, archive)
	}
	return contents
}
