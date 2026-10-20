package refine

import (
	"context"
	"strings"
	"testing"

	"github.com/wuxujun/ai-agent/internal/harness"
	"github.com/wuxujun/ai-agent/internal/improvement"
	"github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/multiagent"
	"github.com/wuxujun/ai-agent/internal/types"
)

type fakeEditorCaller struct {
	proposal     llmProposal
	usage        types.TokenUsage
	err          error
	systemPrompt string
	userPrompt   string
	schema       map[string]any
}

func (f *fakeEditorCaller) CallJSON(_ context.Context, _ llm.Config, system, user string, schema map[string]any, dest any) (types.TokenUsage, error) {
	f.systemPrompt, f.userPrompt, f.schema = system, user, schema
	if f.err != nil {
		return f.usage, f.err
	}
	*dest.(*llmProposal) = f.proposal
	return f.usage, nil
}

func testBase() harness.Snapshot {
	return harness.Snapshot{
		ID: "base-id",
		Teams: multiagent.TeamsConfig{
			ActiveTeam: "software",
			Teams: map[string]multiagent.TeamConfig{
				"software": {Planner: multiagent.AgentConfig{Model: "m1"}},
			},
		},
	}
}

func testEditor(caller *fakeEditorCaller) (LLMEditor, context.Context) {
	editor := LLMEditor{
		LLMConfig:    &llm.Config{Scene: "test", Provider: "fake", Model: "fake-model"},
		SystemPrompt: "test system prompt",
	}
	ctx := llm.WithRuntime(context.Background(), llm.NewRuntime(caller, nil))
	return editor, ctx
}

func TestLLMEditorNotConfigured(t *testing.T) {
	editor := LLMEditor{LLMConfig: &llm.Config{Scene: "test"}} // no model
	_, _, _, err := editor.Propose(context.Background(), testBase(), "software", 1, nil)
	if err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("expected not-configured error, got %v", err)
	}
}

func TestLLMEditorUnknownTeamAndBadK(t *testing.T) {
	editor, ctx := testEditor(&fakeEditorCaller{})
	if _, _, _, err := editor.Propose(ctx, testBase(), "nope", 1, nil); err == nil {
		t.Fatal("expected unknown team error")
	}
	if _, _, _, err := editor.Propose(ctx, testBase(), "software", 0, nil); err == nil {
		t.Fatal("expected k>=1 error")
	}
}

func TestLLMEditorProposesAndDropsInvalid(t *testing.T) {
	caller := &fakeEditorCaller{
		proposal: llmProposal{
			General: []llmEdit{
				{Target: "teams.software.planner.model", Op: "set", Before: "m1", After: "m2", Reason: "better model"},
				{Target: "teams.software.planner.bogus", Op: "set", After: "x", Reason: "invalid target"},
			},
			Domain: []llmEdit{
				{Target: "teams.software.routing.reviewed_min_plan_steps", Op: "set", Before: "0", After: "5", Reason: "more rigor"},
			},
		},
		usage: types.TokenUsage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150},
	}
	editor, ctx := testEditor(caller)
	general, domain, usage, err := editor.Propose(ctx, testBase(), "software", 3, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(general) != 1 || len(domain) != 1 {
		t.Fatalf("want 1+1 valid candidates, got %d+%d", len(general), len(domain))
	}
	if general[0].Edits[0].After != "m2" || general[0].SnapshotID != "base-id" {
		t.Fatalf("bad general candidate: %+v", general[0])
	}
	if !strings.HasPrefix(general[0].Author, "llm-editor:") {
		t.Fatalf("bad author: %q", general[0].Author)
	}
	if usage.TotalTokens != 150 {
		t.Fatalf("usage not propagated: %+v", usage)
	}
	if !strings.Contains(caller.userPrompt, `"software"`) {
		t.Fatal("user prompt should contain the team config")
	}
}

func TestLLMEditorCapsAtK(t *testing.T) {
	var general []llmEdit
	for i := 0; i < 5; i++ {
		general = append(general, llmEdit{
			Target: "teams.software.planner.model", Op: "set", After: "m2", Reason: "x",
		})
	}
	caller := &fakeEditorCaller{proposal: llmProposal{General: general}}
	editor, ctx := testEditor(caller)
	g, _, _, err := editor.Propose(ctx, testBase(), "software", 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(g) != 2 {
		t.Fatalf("want k=2 cap, got %d", len(g))
	}
}

func TestLLMEditorCostCeiling(t *testing.T) {
	caller := &fakeEditorCaller{
		proposal: llmProposal{General: []llmEdit{
			{Target: "teams.software.planner.model", Op: "set", After: "m2", Reason: "x"},
		}},
		usage: types.TokenUsage{PromptTokens: 1_000_000, CompletionTokens: 0, TotalTokens: 1_000_000},
	}
	editor, ctx := testEditor(caller)
	editor.LLMConfig.InputCostPerMillionUSD = 1.0
	editor.MaxCostUSD = 0.5
	_, _, _, err := editor.Propose(ctx, testBase(), "software", 1, nil)
	if err == nil || !strings.Contains(err.Error(), "ceiling") {
		t.Fatalf("expected ceiling error, got %v", err)
	}
}

func TestLLMEditorCallError(t *testing.T) {
	caller := &fakeEditorCaller{err: context.DeadlineExceeded}
	editor, ctx := testEditor(caller)
	_, _, _, err := editor.Propose(ctx, testBase(), "software", 1, nil)
	if err == nil {
		t.Fatal("expected call error")
	}
}

func TestLLMEditorPromptIncludesLessons(t *testing.T) {
	caller := &fakeEditorCaller{}
	editor, ctx := testEditor(caller)
	lessons := []improvement.Lesson{{
		ContrastDelta: "pass 0.50 -> 1.00", Confidence: 0.8,
		Edits:    []harness.Edit{{Target: "teams.software.planner.model", Op: "set", After: "m2"}},
		Evidence: []improvement.EvidenceRef{{Kind: "probe-contrast", Summary: "won on 4/4 cases"}},
	}}
	_, _, _, err := editor.Propose(ctx, testBase(), "software", 1, lessons)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(caller.userPrompt, "pass 0.50 -> 1.00") {
		t.Fatal("user prompt should include lesson contrast deltas")
	}
	if !strings.Contains(caller.userPrompt, "won on 4/4 cases") {
		t.Fatal("user prompt should include lesson evidence")
	}
}

func TestEditorSchema(t *testing.T) {
	s := editorSchema(3)
	props := s["properties"].(map[string]any)
	for _, scope := range []string{"general", "domain"} {
		arr := props[scope].(map[string]any)
		if arr["maxItems"] != 3 {
			t.Fatalf("%s maxItems: %v", scope, arr["maxItems"])
		}
		items := arr["items"].(map[string]any)
		itemProps := items["properties"].(map[string]any)
		op := itemProps["op"].(map[string]any)
		enum := op["enum"].([]string)
		if len(enum) != 3 {
			t.Fatalf("op enum: %v", enum)
		}
	}
}

func TestDefaultSystemPromptCoversWhitelist(t *testing.T) {
	for _, marker := range []string{
		"system_prompt", "prompt_version", "provider", "model", "llm_scene",
		"tools", "add-tools", "remove-tools",
		"reviewed_min_plan_steps", "reviewed_min_remaining_llm_calls", "reviewed_min_remaining_tokens",
		"JSON only",
	} {
		if !strings.Contains(defaultEditorSystemPrompt, marker) {
			t.Fatalf("system prompt missing %q", marker)
		}
	}
	// The editor prompt lives under a non-production label, never production.
	if EditorPromptLabel == "production" || EditorPromptLabel == "latest" {
		t.Fatalf("editor prompt label %q must be non-production", EditorPromptLabel)
	}
}
