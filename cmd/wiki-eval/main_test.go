package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunEvaluatesLocalWikiAndAppliesGate(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "wiki", "concepts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "wiki", "concepts", "pbl.md"), []byte("# PBL Guide\n\nWrite an 800 word travel guide.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dataset := filepath.Join(t.TempDir(), "wiki.jsonl")
	if err := os.WriteFile(dataset, []byte(`{"name":"pbl","query":"PBL Guide","expected_uris":["wiki://local/concepts/pbl"],"expected_keywords":["800 word","travel guide"]}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"-directory", root, "-input", dataset, "-format", "json", "-max-p95-ms", "5000"}, &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 || !strings.Contains(stdout.String(), `"thresholds_passed":true`) || !strings.Contains(stdout.String(), `"search_explanations"`) || !strings.Contains(stdout.String(), `"index_version":"sha256:`) {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
}

func TestRunReturnsOneWhenQualityGateFails(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "wiki", "concepts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "wiki", "concepts", "other.md"), []byte("# Other\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dataset := filepath.Join(t.TempDir(), "wiki.jsonl")
	_ = os.WriteFile(dataset, []byte(`{"name":"missing","query":"PBL","expected_uris":["wiki://local/concepts/pbl"]}`+"\n"), 0o600)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-directory", root, "-input", dataset, "-max-p95-ms", "5000"}, &stdout, &stderr); code != 1 || !strings.Contains(stdout.String(), "passed=false") {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
}

func TestRunEvaluatesNoAnswerFalsePositiveGate(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "wiki", "concepts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "wiki", "concepts", "other.md"), []byte("# Other\n\nUnrelated material.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dataset := filepath.Join(t.TempDir(), "wiki.jsonl")
	content := strings.Join([]string{
		`{"name":"known","query":"Other","expected_uris":["wiki://local/concepts/other"]}`,
		`{"name":"unknown","query":"unrelated","expect_no_results":true}`,
	}, "\n") + "\n"
	if err := os.WriteFile(dataset, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"-directory", root, "-input", dataset, "-search-mode", "legacy", "-max-p95-ms", "5000", "-max-no-answer-false-positive-rate", "0"}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stdout.String(), "no_answer_false_positive_rate 1.000 > 0.000") {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
}

func TestRunUsesBM25ByDefault(t *testing.T) {
	t.Setenv("AI_AGENT_WIKI_LOCAL_SEARCH_MODE", "")
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "wiki", "concepts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "wiki", "concepts", "other.md"), []byte("# Other\n\nUnrelated material.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dataset := filepath.Join(t.TempDir(), "wiki.jsonl")
	content := strings.Join([]string{
		`{"name":"known","query":"Other","expected_uris":["wiki://local/concepts/other"]}`,
		`{"name":"unknown","query":"unrelated nonce 94721","expect_no_results":true}`,
	}, "\n") + "\n"
	if err := os.WriteFile(dataset, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"-directory", root, "-input", dataset, "-max-p95-ms", "5000", "-max-no-answer-false-positive-rate", "0"}, &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 || !strings.Contains(stdout.String(), "passed=true") {
		t.Fatalf("default BM25 code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
}
