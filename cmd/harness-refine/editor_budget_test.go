package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEditorCostFlagsRejectBeforeSideEffects(t *testing.T) {
	for _, command := range []struct {
		name string
		run  func([]string, io.Writer, io.Writer) int
	}{{"propose", runPropose}, {"run", runAll}} {
		for _, value := range []string{"-1", "NaN", "+Inf", "-Inf"} {
			t.Run(command.name+"/"+value, func(t *testing.T) {
				out := filepath.Join(t.TempDir(), "must-not-be-created")
				args := []string{"--team", "software", "--out", out, "--editor-max-cost-usd", value}
				if command.name == "run" {
					args = append(args, "--service-url", "https://service.example.invalid", "--service-teams-yaml", filepath.Join(out, "teams.yaml"))
				}
				var stderr bytes.Buffer
				if code := command.run(args, io.Discard, &stderr); code != 2 || !strings.Contains(stderr.String(), "--editor-max-cost-usd") {
					t.Fatalf("exit=%d stderr=%s", code, stderr.String())
				}
				if _, err := os.Stat(out); !os.IsNotExist(err) {
					t.Fatal("invalid ceiling caused filesystem side effects")
				}
			})
		}
	}
}
