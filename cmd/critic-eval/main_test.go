package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wuxujun/ai-agent/internal/criticeval"
	"github.com/wuxujun/ai-agent/internal/multiagent"
)

func TestRunRequiresExplicitLiveOptIn(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"-mode", "online"}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "requires explicit --allow-live") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestOptionalSelectorValidation(t *testing.T) {
	if selector, err := optionalSelector("", 0); err != nil || selector != nil {
		t.Fatalf("selector=%+v err=%v", selector, err)
	}
	if _, err := optionalSelector("latest", 2); err == nil {
		t.Fatal("expected ambiguous selector rejection")
	}
	selector, err := optionalSelector("", 12)
	if err != nil || selector.Version != 12 {
		t.Fatalf("selector=%+v err=%v", selector, err)
	}
}

func TestCriticPromptNameRequiresManagedNameForInlinePrompt(t *testing.T) {
	if name, err := criticPromptName(multiagent.AgentConfig{LangfusePrompt: "alias"}); err != nil || name != "alias" {
		t.Fatalf("name=%q err=%v", name, err)
	}
	if _, err := criticPromptName(multiagent.AgentConfig{SystemPrompt: "inline"}); err == nil {
		t.Fatal("expected inline-only prompt comparison rejection")
	}
}

func TestRunRejectsOfflineSelectors(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"-candidate-label", "latest"}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "prompt selectors require online mode") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestRunRejectsAsymmetricComparisonTokenLimit(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"-mode", "online", "-allow-live", "-candidate-label", "latest", "-max-total-tokens", "100"}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "comparison mode cannot be combined with max-total-tokens") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestWriteComparisonJSONIncludesVariantAndGate(t *testing.T) {
	baselineResult := criticeval.CaseResult{Name: "case", DecisionCorrect: true}
	candidateResult := criticeval.CaseResult{Name: "case", DecisionCorrect: false}
	comparison := criticeval.Comparison{Type: "comparison", Regressions: []string{"case:decision"}, Passed: false}
	var output bytes.Buffer
	err := writeComparison(&output, "json", "software_reviewed",
		promptVariant{Selector: "label:production", PromptName: "critic", ResolvedVersion: 7},
		promptVariant{Selector: "label:latest", PromptName: "critic", ResolvedVersion: 8, ResolvedLabels: []string{"latest"}},
		[]criticeval.CaseResult{baselineResult}, criticeval.Summary{Type: "summary"},
		[]criticeval.CaseResult{candidateResult}, criticeval.Summary{Type: "summary"}, comparison)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{`"variant":"baseline"`, `"variant":"candidate"`, `"candidate":{"selector":"label:latest","prompt_name":"critic","resolved_version":8,"resolved_labels":["latest"]}`, `"regressions":["case:decision"]`, `"passed":false`} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("output %q does not contain %q", output.String(), expected)
		}
	}
}

func TestRunOfflineJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dataset.yaml")
	data := `
version: 1
thresholds:
  min_accuracy: 1
  min_category_match_rate: 1
  max_false_rejection_rate: 0
  max_false_acceptance_rate: 0
  max_high_risk_miss_rate: 0
  max_error_rate: 0
cases:
  - name: unknown
    goal: inspect files
    risk: normal
    expected_approved: false
    expected_issue_categories: [feasibility]
    plan:
      steps:
        - action: unavailable_action
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"-input", path, "-format", "json"}, &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 || !strings.Contains(stdout.String(), `"thresholds_passed":true`) || !strings.Contains(stdout.String(), `"mode":"offline"`) {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}
