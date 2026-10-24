package tests

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type workflow struct {
	On          map[string]any         `yaml:"on"`
	Permissions map[string]string      `yaml:"permissions"`
	Jobs        map[string]workflowJob `yaml:"jobs"`
}
type workflowJob struct {
	Uses        string            `yaml:"uses"`
	Needs       yaml.Node         `yaml:"needs"`
	If          string            `yaml:"if"`
	Permissions map[string]string `yaml:"permissions"`
	Steps       []struct {
		Name string            `yaml:"name"`
		Uses string            `yaml:"uses"`
		Run  string            `yaml:"run"`
		With map[string]string `yaml:"with"`
	} `yaml:"steps"`
}

func readWorkflow(t *testing.T, name string) workflow {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", ".github", "workflows", name))
	if err != nil {
		t.Fatal(err)
	}
	var w workflow
	if err := yaml.Unmarshal(data, &w); err != nil {
		t.Fatal(err)
	}
	return w
}
func TestReleaseRequiresSameCommitQualityGate(t *testing.T) {
	release := readWorkflow(t, "release.yml")
	ci := readWorkflow(t, "ci.yml")
	if _, ok := ci.On["workflow_call"]; !ok {
		t.Fatal("CI cannot be reused")
	}
	if release.Jobs["quality"].Uses != "./.github/workflows/ci.yml" {
		t.Fatal("release does not run local same-revision CI")
	}
	if release.Permissions["contents"] != "read" || release.Jobs["publish-release"].Permissions["contents"] != "write" {
		t.Fatal("release token permissions are not scoped")
	}
	for job, want := range map[string][]string{"build-release": {"quality"}, "publish-release": {"quality", "build-release"}} {
		j := release.Jobs[job]
		var needs []string
		if j.Needs.Kind == yaml.ScalarNode {
			needs = []string{j.Needs.Value}
		} else {
			if err := j.Needs.Decode(&needs); err != nil {
				t.Fatal(err)
			}
		}
		for _, required := range want {
			found := false
			for _, n := range needs {
				found = found || n == required
			}
			if !found {
				t.Fatalf("%s can bypass %s", job, required)
			}
		}
		if j.If != "" {
			t.Fatalf("%s overrides default successful dependency gate", job)
		}
	}
	for _, job := range []string{"test", "console-browser", "vulnerability-scan", "external-integration"} {
		if _, ok := ci.Jobs[job]; !ok {
			t.Fatalf("missing required quality job %s", job)
		}
	}
	for _, w := range []workflow{ci, release} {
		for name, j := range w.Jobs {
			for _, step := range j.Steps {
				if strings.HasPrefix(step.Uses, "actions/checkout@") && step.With["ref"] != "${{ github.sha }}" {
					t.Fatalf("%s checkout is not pinned to the triggering commit", name)
				}
			}
		}
	}
	// Validate real shell syntax after substituting deterministic matrix expressions.
	for _, j := range release.Jobs {
		for _, step := range j.Steps {
			if step.Run == "" {
				continue
			}
			script := step.Run
			for from, to := range map[string]string{"${{ matrix.os }}": "linux", "${{ matrix.arch }}": "amd64", "${{ matrix.artifact_name }}": "ai-agent-linux-amd64", "${{ matrix.asset_name }}": "ai-agent-linux-amd64.tar.gz"} {
				script = strings.ReplaceAll(script, from, to)
			}
			cmd := exec.Command("bash", "-n")
			cmd.Stdin = strings.NewReader(script)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%s shell syntax: %v %s", step.Name, err, out)
			}
		}
	}
}
