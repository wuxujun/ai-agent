package multiagent_test

import (
	"context"
	"strings"
	"testing"

	"github.com/wuxujun/ai-agent/internal/multiagent"
	"github.com/wuxujun/ai-agent/internal/types"
)

// ── ResearcherAgent tests ─────────────────────────────────────────────────────

func TestResearcherAgent_FindFiles(t *testing.T) {
	t.Parallel()

	agent := &multiagent.ResearcherAgent{}
	ev, err := agent.Research(context.Background(), t.TempDir(), multiagent.ResearchStep{
		ID:          "step-1",
		Description: "find all .go files",
		Action:      "find_files",
		FileGlob:    "*.go",
	})

	if err != nil {
		t.Fatalf("Research returned unexpected error: %v", err)
	}
	if ev == nil {
		t.Fatal("Research returned nil evidence")
	}
	// TempDir is empty so 0 files is the expected result — no error.
	if ev.StepID != "step-1" {
		t.Errorf("StepID mismatch: got %q want %q", ev.StepID, "step-1")
	}
	if ev.Action != "find_files" {
		t.Errorf("Action mismatch: got %q want %q", ev.Action, "find_files")
	}
}

func TestResearcherAgent_SearchText(t *testing.T) {
	t.Parallel()

	agent := &multiagent.ResearcherAgent{}
	ev, err := agent.Research(context.Background(), t.TempDir(), multiagent.ResearchStep{
		ID:          "step-2",
		Description: "search for TODO",
		Action:      "search_text",
		SearchQuery: "TODO",
	})

	if err != nil {
		t.Fatalf("Research returned unexpected error: %v", err)
	}
	if ev == nil {
		t.Fatal("Research returned nil evidence")
	}
}

func TestResearcherAgent_ReadFile_Empty(t *testing.T) {
	t.Parallel()

	agent := &multiagent.ResearcherAgent{}
	ev, err := agent.Research(context.Background(), t.TempDir(), multiagent.ResearchStep{
		ID:       "step-3",
		Action:   "read_file",
		FilePath: "", // empty → should skip gracefully
	})

	if err != nil {
		t.Fatalf("Research returned unexpected error: %v", err)
	}
	if ev == nil {
		t.Fatal("Research returned nil evidence")
	}
	if len(ev.Evidence) != 0 {
		t.Errorf("Expected 0 evidence for empty path, got %d", len(ev.Evidence))
	}
}

func TestResearcherAgent_PolicyViolation(t *testing.T) {
	t.Parallel()

	agent := &multiagent.ResearcherAgent{}
	// Try to read a file outside the workspace (path traversal).
	ev, err := agent.Research(context.Background(), t.TempDir(), multiagent.ResearchStep{
		ID:       "step-4",
		Action:   "read_file",
		FilePath: "../../etc/passwd",
	})

	if err != nil {
		t.Fatalf("Research should not return a fatal error for policy violation: %v", err)
	}
	// Should be recorded as an observation, not propagated as a fatal error.
	if ev == nil {
		t.Fatal("Research returned nil evidence")
	}
	if len(ev.Evidence) != 0 {
		t.Errorf("Should have no evidence for policy-violating path, got %d", len(ev.Evidence))
	}
}

func TestResearcherAgent_WriteFileAndExecuteCode(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	agent := &multiagent.ResearcherAgent{}

	// 1. Test write_file
	ev1, err := agent.Research(context.Background(), tmpDir, multiagent.ResearchStep{
		ID:          "step-write",
		Description: "Write script hello.py",
		Action:      "write_file",
		FilePath:    "hello.py",
		Content:     "print('hello world')",
	})
	if err != nil {
		t.Fatalf("unexpected write_file error: %v", err)
	}
	if !strings.Contains(ev1.Observation, "successfully wrote") {
		t.Errorf("expected success message, got: %q", ev1.Observation)
	}

	// 2. Test execute_code
	ev2, err := agent.Research(context.Background(), tmpDir, multiagent.ResearchStep{
		ID:          "step-exec",
		Description: "Run hello.py using python3",
		Action:      "execute_code",
		Command:     "python3",
		Args:        "hello.py",
	})
	if err != nil {
		t.Fatalf("unexpected execute_code error: %v", err)
	}
	if !strings.Contains(ev2.Observation, "command executed") || !strings.Contains(ev2.Observation, "hello world") {
		t.Errorf("expected script output 'hello world' in observation, got: %q", ev2.Observation)
	}
}

func TestResearcherAgent_UnsupportedAction(t *testing.T) {
	t.Parallel()

	agent := &multiagent.ResearcherAgent{}
	ev, err := agent.Research(context.Background(), t.TempDir(), multiagent.ResearchStep{
		ID:     "step-5",
		Action: "unknown_action",
	})

	if err != nil {
		t.Fatalf("Unsupported action should not be a fatal error: %v", err)
	}
	if ev == nil {
		t.Fatal("Research returned nil evidence")
	}
}

// ── Types / constants tests ───────────────────────────────────────────────────

func TestAgentRoleConstants(t *testing.T) {
	if multiagent.RolePlanner != types.AgentRolePlanner {
		t.Errorf("RolePlanner mismatch")
	}
	if multiagent.RoleResearcher != types.AgentRoleResearcher {
		t.Errorf("RoleResearcher mismatch")
	}
	if multiagent.RoleWriter != types.AgentRoleWriter {
		t.Errorf("RoleWriter mismatch")
	}
}

func TestStepEvidence_Fields(t *testing.T) {
	ev := multiagent.StepEvidence{
		StepID:      "step-1",
		StepDesc:    "find Go files",
		Action:      "find_files",
		Observation: "found 3 files",
		Evidence: []types.Evidence{
			{Path: "main.go", Lines: []string{"package main"}, Query: "*.go"},
		},
	}

	if ev.StepID != "step-1" {
		t.Errorf("StepID: got %q", ev.StepID)
	}
	if len(ev.Evidence) != 1 {
		t.Errorf("Evidence count: got %d want 1", len(ev.Evidence))
	}
}

func TestResearchPlan_Fields(t *testing.T) {
	plan := multiagent.ResearchPlan{
		ThoughtSummary: "Search config files",
		Steps: []multiagent.ResearchStep{
			{ID: "step-1", Action: "find_files", FileGlob: "*.yaml"},
		},
	}

	if len(plan.Steps) != 1 {
		t.Errorf("Expected 1 step, got %d", len(plan.Steps))
	}
	if plan.Steps[0].Action != "find_files" {
		t.Errorf("Action mismatch: %s", plan.Steps[0].Action)
	}
}

func TestWriterOutput_Fields(t *testing.T) {
	out := multiagent.WriterOutput{
		FinalAnswer:     "The answer is 42",
		EvidenceSummary: "Found in main.go",
		Confidence:      "high",
	}

	if out.Confidence != "high" {
		t.Errorf("Confidence: got %q want high", out.Confidence)
	}
}
