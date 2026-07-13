package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	_ "github.com/wuxujun/ai-agent/internal/multiagent"
	"github.com/wuxujun/ai-agent/internal/types"
)

const defaultMaxLineBytes = 4 << 20

type evalCase struct {
	Name             string `json:"name"`
	Scene            string `json:"scene"`
	SystemPrompt     string `json:"system_prompt"`
	UserPrompt       string `json:"user_prompt"`
	ExpectedContains string `json:"expected_contains"`
}

type caseResult struct {
	Type      string `json:"type"`
	Name      string `json:"name"`
	Scene     string `json:"scene"`
	Pass      bool   `json:"pass"`
	LatencyMS int64  `json:"latency_ms"`
	Tokens    int    `json:"tokens"`
	Error     string `json:"error,omitempty"`
}

type evalSummary struct {
	Type           string  `json:"type"`
	Cases          int     `json:"cases"`
	Passed         int     `json:"passed"`
	PassRate       float64 `json:"pass_rate"`
	TotalTokens    int     `json:"total_tokens"`
	TotalLatencyMS int64   `json:"total_latency_ms"`
}

type evalCaller func(context.Context, evalCase, map[string]any) (string, types.TokenUsage, error)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, callLLM))
}

func run(args []string, stdout, stderr io.Writer, call evalCaller) int {
	flags := flag.NewFlagSet("llm-eval", flag.ContinueOnError)
	flags.SetOutput(stderr)
	input := flags.String("input", "Sample/llm-eval.jsonl", "JSONL evaluation cases")
	timeout := flags.Duration("timeout", 30*time.Second, "timeout for each evaluation case")
	maxLineBytes := flags.Int("max-line-bytes", defaultMaxLineBytes, "maximum bytes per JSONL case")
	format := flags.String("format", "text", "output format: text or json")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *timeout <= 0 {
		fmt.Fprintln(stderr, "timeout must be greater than zero")
		return 2
	}
	if *maxLineBytes <= 0 {
		fmt.Fprintln(stderr, "max-line-bytes must be greater than zero")
		return 2
	}
	if *format != "text" && *format != "json" {
		fmt.Fprintf(stderr, "unsupported format %q; use text or json\n", *format)
		return 2
	}

	file, err := os.Open(*input)
	if err != nil {
		fmt.Fprintf(stderr, "open input: %v\n", err)
		return 2
	}
	defer file.Close()

	schema := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"answer": map[string]any{"type": "string"}}, "required": []string{"answer"}}
	summary := evalSummary{Type: "summary"}
	encoder := json.NewEncoder(stdout)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, min(64<<10, *maxLineBytes)), *maxLineBytes)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		var item evalCase
		if err := json.Unmarshal(scanner.Bytes(), &item); err != nil {
			fmt.Fprintf(stderr, "input line %d: invalid JSON: %v\n", lineNumber, err)
			return 2
		}
		if err := validateCase(item); err != nil {
			fmt.Fprintf(stderr, "input line %d: %v\n", lineNumber, err)
			return 2
		}

		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		started := time.Now()
		answer, usage, callErr := call(ctx, item, schema)
		duration := time.Since(started)
		cancel()
		passed := callErr == nil && strings.Contains(strings.ToLower(answer), strings.ToLower(item.ExpectedContains))
		result := caseResult{Type: "case", Name: item.Name, Scene: item.Scene, Pass: passed, LatencyMS: duration.Milliseconds(), Tokens: usage.TotalTokens}
		if callErr != nil {
			result.Error = callErr.Error()
		}
		if err := writeCaseResult(stdout, encoder, *format, result); err != nil {
			fmt.Fprintf(stderr, "write result: %v\n", err)
			return 2
		}
		summary.Cases++
		if passed {
			summary.Passed++
		}
		summary.TotalTokens += usage.TotalTokens
		summary.TotalLatencyMS += duration.Milliseconds()
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintf(stderr, "read input near line %d: %v\n", lineNumber+1, err)
		return 2
	}
	if summary.Cases == 0 {
		fmt.Fprintln(stderr, "input contains no evaluation cases")
		return 2
	}
	summary.PassRate = float64(summary.Passed) / float64(summary.Cases)
	if err := writeSummary(stdout, encoder, *format, summary); err != nil {
		fmt.Fprintf(stderr, "write summary: %v\n", err)
		return 2
	}
	if summary.Passed != summary.Cases {
		return 1
	}
	return 0
}

func validateCase(item evalCase) error {
	if strings.TrimSpace(item.Name) == "" {
		return errors.New("name is required")
	}
	if strings.TrimSpace(item.Scene) == "" {
		return errors.New("scene is required")
	}
	if item.ExpectedContains == "" {
		return errors.New("expected_contains is required")
	}
	return nil
}

func callLLM(ctx context.Context, item evalCase, schema map[string]any) (string, types.TokenUsage, error) {
	var output struct {
		Answer string `json:"answer"`
	}
	usage, err := llmcore.CallJSON(ctx, llmcore.ConfigForScene(item.Scene), item.SystemPrompt, item.UserPrompt, schema, &output)
	return output.Answer, usage, err
}

func writeCaseResult(output io.Writer, encoder *json.Encoder, format string, result caseResult) error {
	if format == "json" {
		return encoder.Encode(result)
	}
	_, err := fmt.Fprintf(output, "name=%q scene=%s pass=%t latency=%s tokens=%d error=%s\n", result.Name, result.Scene, result.Pass, time.Duration(result.LatencyMS)*time.Millisecond, result.Tokens, result.Error)
	return err
}

func writeSummary(output io.Writer, encoder *json.Encoder, format string, summary evalSummary) error {
	if format == "json" {
		return encoder.Encode(summary)
	}
	_, err := fmt.Fprintf(output, "summary cases=%d passed=%d pass_rate=%.2f total_tokens=%d total_latency=%s\n", summary.Cases, summary.Passed, summary.PassRate, summary.TotalTokens, time.Duration(summary.TotalLatencyMS)*time.Millisecond)
	return err
}
