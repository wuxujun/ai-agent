package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

func TestRunValidatesAllInputBeforeCallingLLM(t *testing.T) {
	input := writeEvalInput(t, `{"name":"valid","scene":"task_finalizer","expected_exact":"ok"}`+"\n{bad json}\n")
	calls := 0
	var stdout, stderr bytes.Buffer
	code := run([]string{"-input", input}, &stdout, &stderr, func(context.Context, evalCase, map[string]any) (string, types.TokenUsage, error) {
		calls++
		return "ok", types.TokenUsage{}, nil
	})
	if code != 2 || calls != 0 || !strings.Contains(stderr.String(), "input line 2") {
		t.Fatalf("exit=%d calls=%d stderr=%q", code, calls, stderr.String())
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

func TestRunSupportsExactAndRegexAssertions(t *testing.T) {
	input := writeEvalInput(t, strings.Join([]string{
		`{"name":"exact","scene":"task_finalizer","expected_exact":"Hello"}`,
		`{"name":"regex","scene":"task_finalizer","expected_regex":"^ticket-[0-9]+$"}`,
	}, "\n")+"\n")
	answers := []string{"Hello", "ticket-42"}
	calls := 0
	var stdout, stderr bytes.Buffer
	code := run([]string{"-input", input, "-format", "json"}, &stdout, &stderr, func(context.Context, evalCase, map[string]any) (string, types.TokenUsage, error) {
		answer := answers[calls]
		calls++
		return answer, types.TokenUsage{}, nil
	})
	if code != 0 || calls != 2 {
		t.Fatalf("exit=%d calls=%d stdout=%q stderr=%q", code, calls, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), `"assertion":"exact"`) || !strings.Contains(stdout.String(), `"assertion":"regex"`) {
		t.Fatalf("assertions missing from output: %q", stdout.String())
	}
}

func TestRunSupportsJSONPathAssertion(t *testing.T) {
	input := writeEvalInput(t, `{"name":"json","scene":"task_finalizer","expected_json_path":"$.items[1].score","expected_json_value":2}`+"\n")
	var stdout, stderr bytes.Buffer
	code := run([]string{"-input", input, "-format", "json"}, &stdout, &stderr, func(context.Context, evalCase, map[string]any) (string, types.TokenUsage, error) {
		return `{"items":[{"score":1},{"score":2}]}`, types.TokenUsage{}, nil
	})
	if code != 0 || !strings.Contains(stdout.String(), `"assertion":"json_path"`) {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestRunJSONPathMismatchFailsCase(t *testing.T) {
	input := writeEvalInput(t, `{"name":"json","scene":"task_finalizer","expected_json_path":"$.status","expected_json_value":"ready"}`+"\n")
	var stdout, stderr bytes.Buffer
	code := run([]string{"-input", input}, &stdout, &stderr, func(context.Context, evalCase, map[string]any) (string, types.TokenUsage, error) {
		return `{"status":"pending"}`, types.TokenUsage{}, nil
	})
	if code != 1 || !strings.Contains(stdout.String(), "JSON Path value mismatch") {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestRunRejectsInvalidJSONPathBeforeCallingLLM(t *testing.T) {
	input := writeEvalInput(t, `{"name":"json","scene":"task_finalizer","expected_json_path":"items[0]","expected_json_value":1}`+"\n")
	calls := 0
	var stdout, stderr bytes.Buffer
	code := run([]string{"-input", input}, &stdout, &stderr, func(context.Context, evalCase, map[string]any) (string, types.TokenUsage, error) {
		calls++
		return "", types.TokenUsage{}, nil
	})
	if code != 2 || calls != 0 || !strings.Contains(stderr.String(), "path must start with $") {
		t.Fatalf("exit=%d calls=%d stderr=%q", code, calls, stderr.String())
	}
}

func TestRunRejectsConflictingOrInvalidAssertions(t *testing.T) {
	tests := []string{
		`{"name":"conflict","scene":"task_finalizer","expected_contains":"a","expected_exact":"a"}`,
		`{"name":"regex","scene":"task_finalizer","expected_regex":"["}`,
	}
	for _, raw := range tests {
		input := writeEvalInput(t, raw+"\n")
		var stdout, stderr bytes.Buffer
		code := run([]string{"-input", input}, &stdout, &stderr, nil)
		if code != 2 || !strings.Contains(stderr.String(), "input line 1") {
			t.Fatalf("exit=%d stderr=%q", code, stderr.String())
		}
	}
}

func TestRunStopsBeforeCaseBeyondTokenBudget(t *testing.T) {
	caseLine := `{"name":"budget","scene":"task_finalizer","expected_exact":"ok"}`
	input := writeEvalInput(t, strings.Repeat(caseLine+"\n", 3))
	calls := 0
	var stdout, stderr bytes.Buffer
	code := run([]string{"-input", input, "-format", "json", "-max-total-tokens", "10"}, &stdout, &stderr, func(context.Context, evalCase, map[string]any) (string, types.TokenUsage, error) {
		calls++
		return "ok", types.TokenUsage{TotalTokens: 5}, nil
	})
	if code != 1 || calls != 2 {
		t.Fatalf("exit=%d calls=%d stdout=%q stderr=%q", code, calls, stdout.String(), stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	var summary evalSummary
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.TotalTokens != 10 || summary.StoppedReason != "max_total_tokens" {
		t.Fatalf("summary = %+v", summary)
	}
}

func TestRunMarksSingleCaseTokenBudgetOverrun(t *testing.T) {
	input := writeEvalInput(t, `{"name":"budget","scene":"task_finalizer","expected_exact":"ok"}`+"\n")
	var stdout, stderr bytes.Buffer
	code := run([]string{"-input", input, "-format", "json", "-max-total-tokens", "10"}, &stdout, &stderr, func(context.Context, evalCase, map[string]any) (string, types.TokenUsage, error) {
		return "ok", types.TokenUsage{TotalTokens: 11}, nil
	})
	if code != 1 || !strings.Contains(stdout.String(), `"stopped_reason":"max_total_tokens_exceeded"`) {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestRunParallelismIsBoundedAndOutputOrderIsStable(t *testing.T) {
	input := writeEvalInput(t, strings.Join([]string{
		`{"name":"first","scene":"task_finalizer","expected_exact":"first"}`,
		`{"name":"second","scene":"task_finalizer","expected_exact":"second"}`,
		`{"name":"third","scene":"task_finalizer","expected_exact":"third"}`,
	}, "\n")+"\n")
	var current, maximum atomic.Int32
	var stdout, stderr bytes.Buffer
	code := run([]string{"-input", input, "-parallelism", "2"}, &stdout, &stderr, func(_ context.Context, item evalCase, _ map[string]any) (string, types.TokenUsage, error) {
		active := current.Add(1)
		for {
			seen := maximum.Load()
			if active <= seen || maximum.CompareAndSwap(seen, active) {
				break
			}
		}
		if item.Name == "first" {
			time.Sleep(20 * time.Millisecond)
		} else {
			time.Sleep(time.Millisecond)
		}
		current.Add(-1)
		return item.Name, types.TokenUsage{TotalTokens: 1}, nil
	})
	if code != 0 || maximum.Load() != 2 {
		t.Fatalf("exit=%d max_concurrency=%d stderr=%q", code, maximum.Load(), stderr.String())
	}
	output := stdout.String()
	first := strings.Index(output, `name="first"`)
	second := strings.Index(output, `name="second"`)
	third := strings.Index(output, `name="third"`)
	if first < 0 || !(first < second && second < third) {
		t.Fatalf("output order is unstable: %q", output)
	}
}

func TestRunRejectsParallelTokenBudget(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"-parallelism", "2", "-max-total-tokens", "100"}, &stdout, &stderr, nil)
	if code != 2 || !strings.Contains(stderr.String(), "cannot be combined") {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
}

func TestRunCalculatesEstimatedCost(t *testing.T) {
	input := writeEvalInput(t, `{"name":"cost","scene":"task_finalizer","expected_exact":"ok"}`+"\n")
	var stdout, stderr bytes.Buffer
	code := run([]string{"-input", input, "-format", "json", "-input-cost-per-million-usd", "2", "-output-cost-per-million-usd", "8"}, &stdout, &stderr, func(context.Context, evalCase, map[string]any) (string, types.TokenUsage, error) {
		return "ok", types.TokenUsage{PromptTokens: 500_000, CompletionTokens: 250_000, TotalTokens: 750_000}, nil
	})
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	var result caseResult
	if err := json.Unmarshal([]byte(lines[0]), &result); err != nil {
		t.Fatal(err)
	}
	var summary evalSummary
	if err := json.Unmarshal([]byte(lines[1]), &summary); err != nil {
		t.Fatal(err)
	}
	if result.EstimatedCostUSD != 3 || summary.EstimatedCostUSD != 3 {
		t.Fatalf("result=%+v summary=%+v", result, summary)
	}
}

func TestRunUsesPerCaseCostOverridesAndStopsAtBudget(t *testing.T) {
	caseLine := `{"name":"cost","scene":"task_finalizer","expected_exact":"ok","input_cost_per_million_usd":1}`
	input := writeEvalInput(t, caseLine+"\n"+caseLine+"\n")
	calls := 0
	var stdout, stderr bytes.Buffer
	code := run([]string{"-input", input, "-format", "json", "-max-total-cost-usd", "1"}, &stdout, &stderr, func(context.Context, evalCase, map[string]any) (string, types.TokenUsage, error) {
		calls++
		return "ok", types.TokenUsage{PromptTokens: 1_000_000, TotalTokens: 1_000_000}, nil
	})
	if code != 1 || calls != 1 || !strings.Contains(stdout.String(), `"stopped_reason":"max_total_cost_usd"`) {
		t.Fatalf("exit=%d calls=%d stdout=%q stderr=%q", code, calls, stdout.String(), stderr.String())
	}
}

func TestRunMarksSingleCaseCostBudgetOverrun(t *testing.T) {
	input := writeEvalInput(t, `{"name":"cost","scene":"task_finalizer","expected_exact":"ok"}`+"\n")
	var stdout, stderr bytes.Buffer
	code := run([]string{"-input", input, "-format", "json", "-input-cost-per-million-usd", "1", "-max-total-cost-usd", "0.5"}, &stdout, &stderr, func(context.Context, evalCase, map[string]any) (string, types.TokenUsage, error) {
		return "ok", types.TokenUsage{PromptTokens: 1_000_000, TotalTokens: 1_000_000}, nil
	})
	if code != 1 || !strings.Contains(stdout.String(), `"stopped_reason":"max_total_cost_usd_exceeded"`) {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestRunCostBudgetRequiresPricing(t *testing.T) {
	input := writeEvalInput(t, `{"name":"cost","scene":"task_finalizer","expected_exact":"ok"}`+"\n")
	var stdout, stderr bytes.Buffer
	code := run([]string{"-input", input, "-max-total-cost-usd", "1"}, &stdout, &stderr, nil)
	if code != 2 || !strings.Contains(stderr.String(), "requires a non-zero") {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
}

func TestRunJudgeAssertionCombinesUsageAndCost(t *testing.T) {
	input := writeEvalInput(t, `{"name":"judge","scene":"task_finalizer","judge_criteria":"Accurate and complete","judge_min_score":0.8,"judge_input_cost_per_million_usd":3,"judge_output_cost_per_million_usd":4}`+"\n")
	var stdout, stderr bytes.Buffer
	code := runWithJudge(
		[]string{"-input", input, "-format", "json", "-input-cost-per-million-usd", "1", "-output-cost-per-million-usd", "2"},
		&stdout,
		&stderr,
		func(context.Context, evalCase, map[string]any) (string, types.TokenUsage, error) {
			return "candidate", types.TokenUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}, nil
		},
		func(_ context.Context, item evalCase, answer string) (judgeResult, types.TokenUsage, error) {
			if item.JudgeCriteria == "" || answer != "candidate" {
				t.Fatal("judge did not receive case context")
			}
			return judgeResult{Score: 0.9, Reason: "meets criteria"}, types.TokenUsage{PromptTokens: 20, CompletionTokens: 10, TotalTokens: 30}, nil
		},
	)
	if code != 0 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	var result caseResult
	if err := json.Unmarshal([]byte(lines[0]), &result); err != nil {
		t.Fatal(err)
	}
	if result.Assertion != "judge" || result.JudgeScore == nil || *result.JudgeScore != 0.9 || result.JudgeReason != "meets criteria" || result.Tokens != 45 {
		t.Fatalf("result = %+v", result)
	}
	if math.Abs(result.EstimatedCostUSD-0.00012) > 1e-12 {
		t.Fatalf("estimated cost = %.12f", result.EstimatedCostUSD)
	}
}

func TestRunJudgeAssertionFailsBelowThreshold(t *testing.T) {
	input := writeEvalInput(t, `{"name":"judge","scene":"task_finalizer","judge_criteria":"Accurate"}`+"\n")
	var stdout, stderr bytes.Buffer
	code := runWithJudge(
		[]string{"-input", input}, &stdout, &stderr,
		func(context.Context, evalCase, map[string]any) (string, types.TokenUsage, error) {
			return "candidate", types.TokenUsage{}, nil
		},
		func(context.Context, evalCase, string) (judgeResult, types.TokenUsage, error) {
			return judgeResult{Score: 0.6, Reason: "missing evidence"}, types.TokenUsage{}, nil
		},
	)
	if code != 1 || !strings.Contains(stdout.String(), "below minimum 0.700") || !strings.Contains(stdout.String(), "missing evidence") {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestRunJudgeFailureStillCountsUsageAndCost(t *testing.T) {
	input := writeEvalInput(t, `{"name":"judge","scene":"task_finalizer","judge_criteria":"Accurate","judge_input_cost_per_million_usd":3}`+"\n")
	var stdout, stderr bytes.Buffer
	code := runWithJudge(
		[]string{"-input", input, "-format", "json", "-max-total-cost-usd", "2"}, &stdout, &stderr,
		func(context.Context, evalCase, map[string]any) (string, types.TokenUsage, error) {
			return "candidate", types.TokenUsage{PromptTokens: 10, TotalTokens: 10}, nil
		},
		func(context.Context, evalCase, string) (judgeResult, types.TokenUsage, error) {
			return judgeResult{}, types.TokenUsage{PromptTokens: 1_000_000, TotalTokens: 1_000_000}, errors.New("judge unavailable")
		},
	)
	if code != 1 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	var result caseResult
	if err := json.Unmarshal([]byte(lines[0]), &result); err != nil {
		t.Fatal(err)
	}
	if result.Tokens != 1_000_010 || result.EstimatedCostUSD != 3 || result.JudgeScore != nil || !strings.Contains(result.Error, "judge unavailable") {
		t.Fatalf("result = %+v", result)
	}
	var summary evalSummary
	if err := json.Unmarshal([]byte(lines[1]), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.EstimatedCostUSD != 3 || summary.StoppedReason != "max_total_cost_usd_exceeded" {
		t.Fatalf("summary = %+v", summary)
	}
}

func TestRunRejectsJudgeCombinedWithTextAssertion(t *testing.T) {
	input := writeEvalInput(t, `{"name":"judge","scene":"task_finalizer","expected_exact":"ok","judge_criteria":"Accurate"}`+"\n")
	var stdout, stderr bytes.Buffer
	code := runWithJudge([]string{"-input", input}, &stdout, &stderr, nil, nil)
	if code != 2 || !strings.Contains(stderr.String(), "exactly one") {
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
