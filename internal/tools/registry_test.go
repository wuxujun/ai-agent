package tools

import (
	"context"
	"testing"

	"github.com/wuxujun/ai-agent/internal/types"
)

// TestDefaultRegistryHasAllTools ensures every tool that self-registers via
// init() is discoverable through the registry. If a new tool is added, extend
// this list.
func TestDefaultRegistryHasAllTools(t *testing.T) {
	want := []string{
		"analyze_image",
		"find_files", "search_text", "read_file", "write_file",
		"execute_code", "git_diff", "http_fetch", "web_search",
		"web_browser", "sql_query", "apply_patch",
	}
	for _, name := range want {
		if _, ok := Get(name); !ok {
			t.Errorf("tool %q is not registered in DefaultRegistry", name)
		}
	}
}

// TestRegisteredToolsExposeParameters guards against the regression where a
// tool is registered but its parameter schema is empty, which would leave the
// planner unable to pass it any input.
func TestRegisteredToolsExposeParameters(t *testing.T) {
	// Tools that legitimately take no parameters can be added to this set.
	noParams := map[string]bool{}

	for _, tool := range DefaultRegistry.List() {
		params := tool.Parameters()
		if params == nil {
			t.Errorf("tool %q returned nil Parameters()", tool.Name())
			continue
		}
		if len(params) == 0 && !noParams[tool.Name()] {
			t.Errorf("tool %q exposes no parameters; planner cannot drive it", tool.Name())
		}
		// Each parameter spec must be a JSON-schema object with a type.
		for pName, spec := range params {
			m, ok := spec.(map[string]any)
			if !ok {
				t.Errorf("tool %q param %q spec is not a map", tool.Name(), pName)
				continue
			}
			if _, ok := m["type"]; !ok {
				t.Errorf("tool %q param %q is missing a \"type\"", tool.Name(), pName)
			}
		}
	}
}

// TestListIsSorted verifies List() returns tools in deterministic name order.
func TestListIsSorted(t *testing.T) {
	list := DefaultRegistry.List()
	for i := 1; i < len(list); i++ {
		if list[i-1].Name() > list[i].Name() {
			t.Errorf("List() not sorted: %q came before %q", list[i-1].Name(), list[i].Name())
		}
	}
}

type stubTool struct{ name string }

func (s *stubTool) Name() string               { return s.name }
func (s *stubTool) Description() string        { return "stub" }
func (s *stubTool) Parameters() map[string]any { return map[string]any{} }
func (s *stubTool) RiskLevel() types.RiskLevel { return types.RiskLevelLow }
func (s *stubTool) Execute(ctx context.Context, ws string, p map[string]interface{}) (*ToolResult, error) {
	return &ToolResult{Observation: "ok"}, nil
}

// TestRegisterAndGetRoundTrip checks a freshly registered tool is retrievable.
func TestRegisterAndGetRoundTrip(t *testing.T) {
	r := NewRegistry()
	st := &stubTool{name: "stub_tool"}
	r.Register(st)

	got, ok := r.Get("stub_tool")
	if !ok {
		t.Fatal("expected stub_tool to be registered")
	}
	if got.Name() != "stub_tool" {
		t.Errorf("got %q, want stub_tool", got.Name())
	}
	if _, ok := r.Get("missing"); ok {
		t.Error("expected missing tool lookup to fail")
	}
}
