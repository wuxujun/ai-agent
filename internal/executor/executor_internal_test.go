package executor

import "testing"

func TestIsParallelActionUsesRegisteredRiskLevel(t *testing.T) {
	for _, action := range []string{"read_file", "search_text", "json_query", "sql_query"} {
		if !isParallelAction(action) {
			t.Errorf("low-risk action %q should be parallel", action)
		}
	}
	for _, action := range []string{"write_file", "apply_patch", "run_tests", "unknown_action"} {
		if isParallelAction(action) {
			t.Errorf("high-risk or unknown action %q must be serial", action)
		}
	}
}
