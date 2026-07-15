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
	variants, ok := items["anyOf"].([]any)
	if !ok {
		t.Fatalf("items.anyOf is not []any, got %T", items["anyOf"])
	}

	got := map[string]bool{}
	for _, raw := range variants {
		variant := raw.(map[string]any)
		props := variant["properties"].(map[string]any)
		action := props["action"].(map[string]any)
		enum := action["enum"].([]string)
		if len(enum) != 1 {
			t.Fatalf("action variant enum=%v, want exactly one action", enum)
		}
		got[enum[0]] = true
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

// TestSchemaParametersAreScopedByAction ensures strict structured output does
// not require every tool call to emit parameters belonging to unrelated tools.
func TestSchemaParametersAreScopedByAction(t *testing.T) {
	schema := PlannerDecisionSchema()
	httpVariant := openAIActionVariant(t, schema, "http_fetch")
	params := httpVariant["properties"].(map[string]any)["parameters"].(map[string]any)

	paramProps, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatal("parameters.properties missing")
	}
	if _, ok := paramProps["url"]; !ok {
		t.Error("parameters.properties missing \"url\" (http_fetch cannot receive a url)")
	}
	if _, ok := paramProps["ids"]; ok {
		t.Error("http_fetch parameters unexpectedly include retrieval-only ids")
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

func openAIActionVariant(t *testing.T, schema map[string]any, name string) map[string]any {
	t.Helper()
	props := schema["properties"].(map[string]any)
	actionsArray := props["actions"].(map[string]any)
	items := actionsArray["items"].(map[string]any)
	variants := items["anyOf"].([]any)
	for _, raw := range variants {
		variant := raw.(map[string]any)
		variantProps := variant["properties"].(map[string]any)
		action := variantProps["action"].(map[string]any)
		enum := action["enum"].([]string)
		if len(enum) == 1 && enum[0] == name {
			return variant
		}
	}
	t.Fatalf("action variant %q not found", name)
	return nil
}
