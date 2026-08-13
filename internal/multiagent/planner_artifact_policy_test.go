package multiagent

import (
	"strings"
	"testing"
)

func TestWithPlannerArtifactPolicy(t *testing.T) {
	tests := []struct {
		name   string
		prompt string
	}{
		{name: "remote prompt", prompt: "remote planner instructions"},
		{name: "default prompt", prompt: plannerSystemPrompt},
		{name: "replanner prompt", prompt: replannerSystemPrompt},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := withPlannerArtifactPolicy(test.prompt)
			if !strings.Contains(got, plannerArtifactPolicy) {
				t.Fatal("planner artifact policy was not appended")
			}
			if strings.Count(withPlannerArtifactPolicy(got), plannerArtifactPolicy) != 1 {
				t.Fatal("planner artifact policy must not be duplicated")
			}
		})
	}
}
