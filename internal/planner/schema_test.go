package planner

import (
	"testing"
)

// TestSchemaActionEnumCoversRegisteredTools is the regression test for the bug
// where newly registered tools (git_diff, http_fetch, web_search) were missing
// from the planner action enum, making them unselectable under strict JSON mode.
func TestSchemaActionEnumCoversRegisteredTools(t *testing.T) {
	schema := PlannerDecisionSchema()

	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatal("schema missing properties")
	}
	actionsArray, ok := props["actions"].(map[string]any)
	if !ok {
		t.Fatal("schema missing actions property")
	}
	items, ok := actionsArray["items"].(map[string]any)
	if !ok {
		t.Fatal("actions missing items")
	}
	itemProps, ok := items["properties"].(map[string]any)
	if !ok {
		t.Fatal("items missing properties")
	}
	action, ok := itemProps["action"].(map[string]any)
	if !ok {
		t.Fatal("item missing action property")
	}
	enum, ok := action["enum"].([]string)
	if !ok {
		t.Fatalf("action.enum is not []string, got %T", action["enum"])
	}

	got := map[string]bool{}
	for _, a := range enum {
		got[a] = true
	}

	for _, name := range []string{
		"find_files", "search_text", "read_file", "write_file",
		"execute_code", "git_diff", "http_fetch", "web_search", "none",
	} {
		if !got[name] {
			t.Errorf("action enum missing %q", name)
		}
	}
}

// TestSchemaParametersIncludeURL ensures http_fetch's url parameter made it into
// the merged parameters object (previously absent, so url could not be passed).
func TestSchemaParametersIncludeURL(t *testing.T) {
	schema := PlannerDecisionSchema()
	props := schema["properties"].(map[string]any)
	actionsArray := props["actions"].(map[string]any)
	items := actionsArray["items"].(map[string]any)
	itemProps := items["properties"].(map[string]any)
	params := itemProps["parameters"].(map[string]any)

	paramProps, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatal("parameters.properties missing")
	}
	if _, ok := paramProps["url"]; !ok {
		t.Error("parameters.properties missing \"url\" (http_fetch cannot receive a url)")
	}

	// Strict mode invariant: every property must also appear in required.
	required, ok := params["required"].([]string)
	if !ok {
		t.Fatal("parameters.required is not []string")
	}
	reqSet := map[string]bool{}
	for _, r := range required {
		reqSet[r] = true
	}
	for name := range paramProps {
		if !reqSet[name] {
			t.Errorf("param %q present in properties but missing from required (violates strict mode)", name)
		}
	}
}
