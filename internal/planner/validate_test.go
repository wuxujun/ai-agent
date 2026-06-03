package planner

import "testing"

// TestValidateDecisionAcceptsRegisteredTools is the regression test for the bug
// where ValidateDecision hardcoded only six actions, so registry tools that the
// planner schema legitimately exposed (git_diff, http_fetch, web_search) were
// rejected at validation time and failed the whole task. Validation must accept
// every action the registry (and therefore the schema) can produce.
func TestValidateDecisionAcceptsRegisteredTools(t *testing.T) {
	cases := []struct {
		action string
		params map[string]any
	}{
		{"find_files", map[string]any{"pattern": "*.go"}},
		{"search_text", map[string]any{"query": "needle"}},
		{"git_diff", map[string]any{}},
		{"http_fetch", map[string]any{"url": "https://example.com"}},
		{"web_search", map[string]any{"query": "golang"}},
	}
	for _, c := range cases {
		d := &PlanDecision{
			ThoughtSummary: "t",
			Stop:           false,
			Actions:        []ActionCall{{Action: c.action, Parameters: c.params}},
		}
		if err := ValidateDecision(d); err != nil {
			t.Errorf("action %q rejected by validator but is registry-backed: %v", c.action, err)
		}
	}
}

// TestValidateDecisionRejectsUnknownAction guards the other direction: an action
// that is neither "none" nor a registered tool must still be rejected.
func TestValidateDecisionRejectsUnknownAction(t *testing.T) {
	d := &PlanDecision{
		Actions: []ActionCall{{Action: "definitely_not_a_tool", Parameters: map[string]any{}}},
	}
	if err := ValidateDecision(d); err == nil {
		t.Error("expected unknown action to be rejected, got nil error")
	}
}

// TestGenAISchemaUsesActionsArray is the regression test for the Gemini schema
// drifting to a singular action/parameters shape, which deserialised to zero
// actions in PlanDecision and failed validation on every Gemini turn.
func TestGenAISchemaUsesActionsArray(t *testing.T) {
	schema := PlannerDecisionGenAISchema()
	actions, ok := schema.Properties["actions"]
	if !ok {
		t.Fatal("genai schema missing top-level \"actions\" property")
	}
	if actions.Items == nil {
		t.Fatal("genai schema \"actions\" must be an array with item schema")
	}
	if _, ok := actions.Items.Properties["action"]; !ok {
		t.Error("genai actions item missing \"action\" property")
	}
	if _, ok := schema.Properties["action"]; ok {
		t.Error("genai schema still exposes singular \"action\" (drifted from PlanDecision.Actions)")
	}
}
