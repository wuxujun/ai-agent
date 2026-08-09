package planner

import (
	"strings"
	"testing"
)

func TestPlannedStepsForLogIncludesStepAndSafeParameters(t *testing.T) {
	steps := plannedStepsForLog(3, []ActionCall{{
		Action: "web_search",
		Parameters: map[string]any{
			"query":   "查教师李松霖的信息",
			"api_key": "secret-value",
			"content": "do not write this body to logs",
		},
	}})

	if len(steps) != 1 || steps[0].Step != 3 || steps[0].ActionIndex != 0 || steps[0].Action != "web_search" {
		t.Fatalf("unexpected planned steps: %#v", steps)
	}
	if value, _ := steps[0].Parameters["query"].(string); !strings.HasPrefix(value, "<") || !strings.HasSuffix(value, " chars>") {
		t.Fatalf("query body was not summarized: %#v", steps[0].Parameters)
	}
	if steps[0].Parameters["api_key"] != "[REDACTED]" {
		t.Fatalf("api key was not redacted: %#v", steps[0].Parameters)
	}
	if value, _ := steps[0].Parameters["content"].(string); !strings.HasPrefix(value, "<") || !strings.HasSuffix(value, " chars>") {
		t.Fatalf("content body was not summarized: %#v", steps[0].Parameters)
	}
}

func TestPlannedStepsForLogTruncatesLongNonContentValues(t *testing.T) {
	steps := plannedStepsForLog(1, []ActionCall{{Action: "read_file", Parameters: map[string]any{"path": strings.Repeat("界", plannedParameterLogLimit+1)}}})
	path, _ := steps[0].Parameters["path"].(string)
	if len([]rune(path)) != plannedParameterLogLimit+3 || !strings.HasSuffix(path, "...") {
		t.Fatalf("long path was not rune-safe truncated: rune length=%d", len([]rune(path)))
	}
}
