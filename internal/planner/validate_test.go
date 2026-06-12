package planner

import (
	"strings"
	"testing"

	"github.com/wuxujun/ai-agent/internal/tools"
)

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

// TestValidateDecisionDispatchesPerToolValidate is the regression test for the
// silent break where http_fetch / git_diff / web_search / use_skill all bypassed
// planner-side validation: validate.go used to hardcode a switch over a handful
// of tool names, so anything outside that set fell through to "no checks" and
// only failed at execute time. Now ValidateDecision dispatches to the tool's
// own Validate(params) via the Validator interface — registering a new tool
// with a Validate method makes its parameter checks immediately effective.
func TestValidateDecisionDispatchesPerToolValidate(t *testing.T) {
	cases := []struct {
		name   string
		action string
		params map[string]any
	}{
		// Before the fix: http_fetch fell through, no URL check at plan time,
		// only failed when Execute tried to dial an empty string.
		{"http_fetch_empty_url", "http_fetch", map[string]any{"url": ""}},
		// Likewise for malformed scheme — empty trim still works, but a value
		// without http(s) scheme is now rejected at plan time.
		{"http_fetch_bad_scheme", "http_fetch", map[string]any{"url": "ftp://example.com"}},
		// git_diff now rejects path traversal at validation time.
		{"git_diff_traversal", "git_diff", map[string]any{"path": "../etc/passwd"}},
		{"git_diff_absolute", "git_diff", map[string]any{"path": "/etc/passwd"}},
		// web_search requires non-empty query.
		{"web_search_empty_query", "web_search", map[string]any{"query": "   "}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := &PlanDecision{
				ThoughtSummary: "t",
				Actions:        []ActionCall{{Action: c.action, Parameters: c.params}},
			}
			err := ValidateDecision(d)
			if err == nil {
				t.Fatalf("expected per-tool Validate to reject %s with params %#v, got nil", c.action, c.params)
			}
			// Error message must mention the action so callers can pinpoint
			// which tool failed validation.
			if !strings.Contains(err.Error(), c.action) {
				t.Errorf("error message should reference action %q, got %q", c.action, err.Error())
			}
		})
	}
}

// TestEveryRegisteredToolImplementsValidator guards the invariant the
// validate.go refactor relies on: ValidateDecision now dispatches to each
// tool's own Validate(params) instead of a hardcoded switch, so a tool that
// forgets to implement Validate silently falls through to "no plan-time
// checks" — the exact regression the refactor was meant to close. Iterating
// DefaultRegistry catches that at test time the moment a new tool is added,
// rather than at execute time in production.
func TestEveryRegisteredToolImplementsValidator(t *testing.T) {
	for _, tool := range tools.DefaultRegistry.List() {
		if _, ok := unwrapValidator(tool); !ok {
			t.Errorf("tool %q does not implement planner.Validator — plan-time validation will silently no-op for it", tool.Name())
		}
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
