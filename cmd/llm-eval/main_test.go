package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wuxujun/ai-agent/internal/types"
)

func TestRunJSONOutputAndSuccessExitCode(t *testing.T) {
	input := writeEvalInput(t, `{"name":"writer","scene":"task_finalizer","user_prompt":"write","expected_contains":"hello"}`+"\n")
	var stdout, stderr bytes.Buffer
	code := run([]string{"-input", input, "-format", "json"}, &stdout, &stderr, func(context.Context, evalCase, map[string]any) (string, types.TokenUsage, error) {
		return "Hello world", types.TokenUsage{TotalTokens: 12}, nil
	})
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("output lines = %d: %q", len(lines), stdout.String())
	}
	var summary evalSummary
	if err := json.Unmarshal([]byte(lines[1]), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Cases != 1 || summary.Passed != 1 || summary.TotalTokens != 12 {
		t.Fatalf("summary = %+v", summary)
	}
}

func TestRunReturnsOneWhenCaseFails(t *testing.T) {
	input := writeEvalInput(t, `{"name":"writer","scene":"task_finalizer","user_prompt":"write","expected_contains":"expected"}`+"\n")
	var stdout, stderr bytes.Buffer
	code := run([]string{"-input", input}, &stdout, &stderr, func(context.Context, evalCase, map[string]any) (string, types.TokenUsage, error) {
		return "different", types.TokenUsage{}, nil
	})
	if code != 1 {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
}

func TestRunReportsInputLineAndReturnsTwo(t *testing.T) {
	input := writeEvalInput(t, "\n{bad json}\n")
	var stdout, stderr bytes.Buffer
	code := run([]string{"-input", input}, &stdout, &stderr, nil)
	if code != 2 || !strings.Contains(stderr.String(), "input line 2") {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
}

func TestRunAppliesPerCaseTimeout(t *testing.T) {
	input := writeEvalInput(t, `{"name":"slow","scene":"task_finalizer","user_prompt":"write","expected_contains":"done"}`+"\n")
	var stdout, stderr bytes.Buffer
	code := run([]string{"-input", input, "-timeout", "1ms"}, &stdout, &stderr, func(ctx context.Context, _ evalCase, _ map[string]any) (string, types.TokenUsage, error) {
		<-ctx.Done()
		return "", types.TokenUsage{}, ctx.Err()
	})
	if code != 1 || !strings.Contains(stdout.String(), context.DeadlineExceeded.Error()) {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestRunAcceptsPromptLargerThanScannerDefault(t *testing.T) {
	item := evalCase{Name: "large", Scene: "task_finalizer", UserPrompt: strings.Repeat("x", 70<<10), ExpectedContains: "ok"}
	raw, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	input := writeEvalInput(t, string(raw)+"\n")
	var stdout, stderr bytes.Buffer
	code := run([]string{"-input", input}, &stdout, &stderr, func(context.Context, evalCase, map[string]any) (string, types.TokenUsage, error) {
		return "ok", types.TokenUsage{}, nil
	})
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
}

func TestRunRejectsInvalidOptions(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"-timeout", "0s"}, &stdout, &stderr, func(context.Context, evalCase, map[string]any) (string, types.TokenUsage, error) {
		return "", types.TokenUsage{}, errors.New("must not run")
	})
	if code != 2 || !strings.Contains(stderr.String(), "timeout") {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
}

func writeEvalInput(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cases.jsonl")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
