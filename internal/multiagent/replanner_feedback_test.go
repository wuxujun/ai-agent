package multiagent

import (
	"strings"
	"testing"

	"github.com/wuxujun/ai-agent/internal/plancritic"
	"github.com/wuxujun/ai-agent/internal/types"
)

func TestFormatTracesForReplannerIncludesCriticRecommendations(t *testing.T) {
	traces := []types.StepTrace{{
		Step:        1,
		Action:      plancritic.TraceAction,
		AgentRole:   RoleCritic,
		Observation: "approved=false issues=1 summary=missing validation",
		Evidence: []types.Evidence{{
			Path:  "write_file",
			Query: "safety",
			Lines: []string{"[high] existing content is not inspected", "Recommendation: read and compare the file before writing"},
		}},
	}}

	formatted := formatTracesForReplanner(traces)
	for _, expected := range []string{"missing validation", "existing content is not inspected", "read and compare the file before writing"} {
		if !strings.Contains(formatted, expected) {
			t.Fatalf("formatted trace omitted %q: %s", expected, formatted)
		}
	}
}
