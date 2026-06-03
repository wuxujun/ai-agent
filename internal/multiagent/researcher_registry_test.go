package multiagent

import (
	"context"
	"testing"

	"github.com/wuxujun/ai-agent/internal/tools"
	"github.com/wuxujun/ai-agent/internal/types"
)

// recordingTool captures the params it was invoked with so the test can assert
// that ResearcherAgent correctly maps ResearchStep fields onto tool parameters.
type recordingTool struct {
	gotParams map[string]interface{}
}

func (r *recordingTool) Name() string             { return "recording_tool" }
func (r *recordingTool) Description() string       { return "test stub" }
func (r *recordingTool) Parameters() map[string]any { return map[string]any{"url": map[string]any{"type": "string"}} }
func (r *recordingTool) RiskLevel() types.RiskLevel { return types.RiskLevelLow }
func (r *recordingTool) Execute(ctx context.Context, ws string, p map[string]interface{}) (*tools.ToolResult, error) {
	r.gotParams = p
	return &tools.ToolResult{
		Query:       "captured",
		Observation: "stub observation",
		Evidence:    []types.Evidence{{Path: "x", Query: "captured"}},
	}, nil
}

// TestResearcherRoutesUnknownActionThroughRegistry verifies the default branch
// dispatches any registered tool (beyond the five hard-coded actions) and maps
// the ToolResult back into StepEvidence.
func TestResearcherRoutesUnknownActionThroughRegistry(t *testing.T) {
	stub := &recordingTool{}
	tools.Register(stub) // adds to DefaultRegistry for this test process

	agent := &ResearcherAgent{}
	ev, err := agent.Research(context.Background(), t.TempDir(), ResearchStep{
		ID:          "step-1",
		Description: "drive the stub tool",
		Action:      "recording_tool",
		URL:         "https://example.com",
		SearchQuery: "kw",
		FilePath:    "f.txt",
	})
	if err != nil {
		t.Fatalf("Research returned fatal error: %v", err)
	}
	if ev.Failed {
		t.Errorf("step should not be marked failed: %q", ev.Observation)
	}
	if ev.Observation != "stub observation" {
		t.Errorf("observation not propagated from tool: got %q", ev.Observation)
	}
	if len(ev.Evidence) != 1 || ev.Evidence[0].Path != "x" {
		t.Errorf("evidence not propagated from tool: %+v", ev.Evidence)
	}

	// Verify ResearchStep fields were mapped onto the generic param map.
	if stub.gotParams["url"] != "https://example.com" {
		t.Errorf("url param not mapped, got %v", stub.gotParams["url"])
	}
	if stub.gotParams["query"] != "kw" {
		t.Errorf("query param not mapped from SearchQuery, got %v", stub.gotParams["query"])
	}
	if stub.gotParams["path"] != "f.txt" {
		t.Errorf("path param not mapped from FilePath, got %v", stub.gotParams["path"])
	}
}

// TestResearcherStillRejectsUnregisteredAction ensures a genuinely unknown
// action remains a non-fatal failure (unchanged behaviour).
func TestResearcherStillRejectsUnregisteredAction(t *testing.T) {
	agent := &ResearcherAgent{}
	ev, err := agent.Research(context.Background(), t.TempDir(), ResearchStep{
		ID:     "step-x",
		Action: "definitely_not_registered",
	})
	if err != nil {
		t.Fatalf("unregistered action should be non-fatal, got %v", err)
	}
	if !ev.Failed {
		t.Error("unregistered action should set Failed=true")
	}
}
