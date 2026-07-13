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
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wuxujun/ai-agent/internal/config"
	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	_ "github.com/wuxujun/ai-agent/internal/multiagent"
	"github.com/wuxujun/ai-agent/internal/types"
)

const defaultMaxLineBytes = 4 << 20

type evalCase struct {
	Name                         string          `json:"name"`
	Scene                        string          `json:"scene"`
	SystemPrompt                 string          `json:"system_prompt"`
	UserPrompt                   string          `json:"user_prompt"`
	ExpectedContains             string          `json:"expected_contains"`
	ExpectedExact                string          `json:"expected_exact"`
	ExpectedRegex                string          `json:"expected_regex"`
	ExpectedJSONPath             string          `json:"expected_json_path"`
	ExpectedJSONValue            json.RawMessage `json:"expected_json_value,omitempty"`
	InputCostPerMillionUSD       *float64        `json:"input_cost_per_million_usd,omitempty"`
	OutputCostPerMillionUSD      *float64        `json:"output_cost_per_million_usd,omitempty"`
	JudgeCriteria                string          `json:"judge_criteria"`
	JudgeScene                   string          `json:"judge_scene"`
	JudgeMinScore                *float64        `json:"judge_min_score,omitempty"`
	JudgeInputCostPerMillionUSD  *float64        `json:"judge_input_cost_per_million_usd,omitempty"`
	JudgeOutputCostPerMillionUSD *float64        `json:"judge_output_cost_per_million_usd,omitempty"`
}

type caseResult struct {
	Type             string   `json:"type"`
	Name             string   `json:"name"`
	Scene            string   `json:"scene"`
	Pass             bool     `json:"pass"`
	LatencyMS        int64    `json:"latency_ms"`
	Tokens           int      `json:"tokens"`
	PromptTokens     int      `json:"prompt_tokens"`
	CompletionTokens int      `json:"completion_tokens"`
	EstimatedCostUSD float64  `json:"estimated_cost_usd"`
	Assertion        string   `json:"assertion"`
	JudgeScore       *float64 `json:"judge_score,omitempty"`
	JudgeReason      string   `json:"judge_reason,omitempty"`
	Error            string   `json:"error,omitempty"`
}

type evalSummary struct {
	Type             string  `json:"type"`
	Cases            int     `json:"cases"`
	Passed           int     `json:"passed"`
	PassRate         float64 `json:"pass_rate"`
	TotalTokens      int     `json:"total_tokens"`
	TotalLatencyMS   int64   `json:"total_latency_ms"`
	StoppedReason    string  `json:"stopped_reason,omitempty"`
	EstimatedCostUSD float64 `json:"estimated_cost_usd"`
}

type costRates struct {
	inputPerMillionUSD  float64
	outputPerMillionUSD float64
}

type evalCaller func(context.Context, evalCase, map[string]any) (string, types.TokenUsage, error)

type judgeResult struct {
	Score  float64 `json:"score"`
	Reason string  `json:"reason"`
}

type evalJudge func(context.Context, evalCase, string) (judgeResult, types.TokenUsage, error)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, callLLM))
}

func run(args []string, stdout, stderr io.Writer, call evalCaller) int {
	return runWithJudge(args, stdout, stderr, call, judgeLLM)
}

func runWithJudge(args []string, stdout, stderr io.Writer, call evalCaller, judge evalJudge) int {
	flags := flag.NewFlagSet("llm-eval", flag.ContinueOnError)
	flags.SetOutput(stderr)
	input := flags.String("input", "Sample/llm-eval.jsonl", "JSONL evaluation cases")
	timeout := flags.Duration("timeout", 30*time.Second, "timeout for each evaluation case")
	maxLineBytes := flags.Int("max-line-bytes", defaultMaxLineBytes, "maximum bytes per JSONL case")
	maxTotalTokens := flags.Int("max-total-tokens", 0, "maximum total tokens; 0 disables the limit")
	maxTotalCostUSD := flags.Float64("max-total-cost-usd", 0, "maximum estimated cost in USD; 0 disables the limit")
	inputCostPerMillionUSD := flags.Float64("input-cost-per-million-usd", 0, "input token cost per million in USD")
	outputCostPerMillionUSD := flags.Float64("output-cost-per-million-usd", 0, "output token cost per million in USD")
	parallelism := flags.Int("parallelism", 1, "number of concurrent evaluation cases")
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
	if *maxTotalTokens < 0 {
		fmt.Fprintln(stderr, "max-total-tokens must be greater than or equal to zero")
		return 2
	}
	if *maxTotalCostUSD < 0 || *inputCostPerMillionUSD < 0 || *outputCostPerMillionUSD < 0 {
		fmt.Fprintln(stderr, "cost values must be greater than or equal to zero")
		return 2
	}
	if *parallelism <= 0 {
		fmt.Fprintln(stderr, "parallelism must be greater than zero")
		return 2
	}
	if *parallelism > 1 && (*maxTotalTokens > 0 || *maxTotalCostUSD > 0) {
		fmt.Fprintln(stderr, "parallelism greater than one cannot be combined with total token or cost limits")
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

	cases, err := readEvalCases(file, *maxLineBytes)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if len(cases) == 0 {
		fmt.Fprintln(stderr, "input contains no evaluation cases")
		return 2
	}

	rates := costRates{inputPerMillionUSD: *inputCostPerMillionUSD, outputPerMillionUSD: *outputCostPerMillionUSD}
	if *maxTotalCostUSD > 0 && !hasCostRates(cases, rates) {
		fmt.Fprintln(stderr, "max-total-cost-usd requires a non-zero input or output cost rate")
		return 2
	}
	results, stoppedReason := executeCases(cases, *timeout, *parallelism, *maxTotalTokens, *maxTotalCostUSD, rates, call, judge)
	summary := evalSummary{Type: "summary"}
	encoder := json.NewEncoder(stdout)
	for _, result := range results {
		if err := writeCaseResult(stdout, encoder, *format, result); err != nil {
			fmt.Fprintf(stderr, "write result: %v\n", err)
			return 2
		}
		summary.Cases++
		if result.Pass {
			summary.Passed++
		}
		summary.TotalTokens += result.Tokens
		summary.TotalLatencyMS += result.LatencyMS
		summary.EstimatedCostUSD += result.EstimatedCostUSD
	}
	summary.StoppedReason = stoppedReason
	summary.PassRate = float64(summary.Passed) / float64(summary.Cases)
	if err := writeSummary(stdout, encoder, *format, summary); err != nil {
		fmt.Fprintf(stderr, "write summary: %v\n", err)
		return 2
	}
	if summary.Passed != summary.Cases || summary.StoppedReason != "" {
		return 1
	}
	return 0
}

func readEvalCases(input io.Reader, maxLineBytes int) ([]evalCase, error) {
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, min(64<<10, maxLineBytes)), maxLineBytes)
	var cases []evalCase
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		var item evalCase
		if err := json.Unmarshal(scanner.Bytes(), &item); err != nil {
			return nil, fmt.Errorf("input line %d: invalid JSON: %w", lineNumber, err)
		}
		if err := validateCase(item); err != nil {
			return nil, fmt.Errorf("input line %d: %w", lineNumber, err)
		}
		cases = append(cases, item)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read input near line %d: %w", lineNumber+1, err)
	}
	return cases, nil
}

func executeCases(cases []evalCase, timeout time.Duration, parallelism, maxTotalTokens int, maxTotalCostUSD float64, rates costRates, call evalCaller, judge evalJudge) ([]caseResult, string) {
	if parallelism == 1 {
		results := make([]caseResult, 0, len(cases))
		totalTokens := 0
		totalCostUSD := 0.0
		for _, item := range cases {
			if maxTotalTokens > 0 && totalTokens >= maxTotalTokens {
				return results, "max_total_tokens"
			}
			if maxTotalCostUSD > 0 && totalCostUSD >= maxTotalCostUSD {
				return results, "max_total_cost_usd"
			}
			result := executeCase(item, timeout, rates, call, judge)
			results = append(results, result)
			totalTokens += result.Tokens
			totalCostUSD += result.EstimatedCostUSD
			if maxTotalTokens > 0 && totalTokens > maxTotalTokens {
				return results, "max_total_tokens_exceeded"
			}
			if maxTotalCostUSD > 0 && totalCostUSD > maxTotalCostUSD {
				return results, "max_total_cost_usd_exceeded"
			}
		}
		return results, ""
	}

	results := make([]caseResult, len(cases))
	jobs := make(chan int)
	var workers sync.WaitGroup
	for range min(parallelism, len(cases)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				results[index] = executeCase(cases[index], timeout, rates, call, judge)
			}
		}()
	}
	for index := range cases {
		jobs <- index
	}
	close(jobs)
	workers.Wait()
	return results, ""
}

func executeCase(item evalCase, timeout time.Duration, rates costRates, call evalCaller, judge evalJudge) caseResult {
	schema := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"answer": map[string]any{"type": "string"}}, "required": []string{"answer"}}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	started := time.Now()
	answer, usage, callErr := call(ctx, item, schema)
	assertion := assertionType(item)
	var matchErr error
	var judged *judgeResult
	var judgeUsage types.TokenUsage
	if callErr == nil && assertion == "judge" {
		result, currentUsage, err := judge(ctx, item, answer)
		judgeUsage = currentUsage
		if err != nil {
			matchErr = fmt.Errorf("judge call failed: %w", err)
		} else {
			judged = &result
			minimum := 0.7
			if item.JudgeMinScore != nil {
				minimum = *item.JudgeMinScore
			}
			if result.Score < minimum {
				matchErr = fmt.Errorf("judge score %.3f is below minimum %.3f", result.Score, minimum)
			}
		}
	} else if callErr == nil {
		_, matchErr = matchExpected(item, answer)
	}
	duration := time.Since(started)
	cancel()
	answerUsage := usage
	usage.PromptTokens += judgeUsage.PromptTokens
	usage.CompletionTokens += judgeUsage.CompletionTokens
	usage.TotalTokens += judgeUsage.TotalTokens
	caseRates := rates
	if item.InputCostPerMillionUSD != nil {
		caseRates.inputPerMillionUSD = *item.InputCostPerMillionUSD
	}
	if item.OutputCostPerMillionUSD != nil {
		caseRates.outputPerMillionUSD = *item.OutputCostPerMillionUSD
	}
	estimatedCost := float64(answerUsage.PromptTokens)*caseRates.inputPerMillionUSD/1_000_000 + float64(answerUsage.CompletionTokens)*caseRates.outputPerMillionUSD/1_000_000
	if assertion == "judge" {
		judgeRates := caseRates
		if item.JudgeInputCostPerMillionUSD != nil {
			judgeRates.inputPerMillionUSD = *item.JudgeInputCostPerMillionUSD
		}
		if item.JudgeOutputCostPerMillionUSD != nil {
			judgeRates.outputPerMillionUSD = *item.JudgeOutputCostPerMillionUSD
		}
		estimatedCost += float64(judgeUsage.PromptTokens)*judgeRates.inputPerMillionUSD/1_000_000 + float64(judgeUsage.CompletionTokens)*judgeRates.outputPerMillionUSD/1_000_000
	}
	result := caseResult{Type: "case", Name: item.Name, Scene: item.Scene, Pass: callErr == nil && matchErr == nil, LatencyMS: duration.Milliseconds(), Tokens: usage.TotalTokens, PromptTokens: usage.PromptTokens, CompletionTokens: usage.CompletionTokens, EstimatedCostUSD: estimatedCost, Assertion: assertion}
	if judged != nil {
		result.JudgeScore = &judged.Score
		result.JudgeReason = judged.Reason
	}
	if callErr != nil {
		result.Error = callErr.Error()
	} else if matchErr != nil {
		result.Error = matchErr.Error()
	}
	return result
}

func validateCase(item evalCase) error {
	if strings.TrimSpace(item.Name) == "" {
		return errors.New("name is required")
	}
	if strings.TrimSpace(item.Scene) == "" {
		return errors.New("scene is required")
	}
	if item.InputCostPerMillionUSD != nil && *item.InputCostPerMillionUSD < 0 {
		return errors.New("input_cost_per_million_usd must be greater than or equal to zero")
	}
	if item.OutputCostPerMillionUSD != nil && *item.OutputCostPerMillionUSD < 0 {
		return errors.New("output_cost_per_million_usd must be greater than or equal to zero")
	}
	if item.JudgeInputCostPerMillionUSD != nil && *item.JudgeInputCostPerMillionUSD < 0 || item.JudgeOutputCostPerMillionUSD != nil && *item.JudgeOutputCostPerMillionUSD < 0 {
		return errors.New("judge cost rates must be greater than or equal to zero")
	}
	if item.JudgeMinScore != nil && (*item.JudgeMinScore < 0 || *item.JudgeMinScore > 1) {
		return errors.New("judge_min_score must be between 0 and 1")
	}
	assertions := 0
	if item.ExpectedContains != "" {
		assertions++
	}
	if item.ExpectedExact != "" {
		assertions++
	}
	if item.ExpectedRegex != "" {
		assertions++
		if _, err := regexp.Compile(item.ExpectedRegex); err != nil {
			return fmt.Errorf("invalid expected_regex: %w", err)
		}
	}
	if item.ExpectedJSONPath != "" || len(item.ExpectedJSONValue) > 0 {
		if item.ExpectedJSONPath == "" || len(item.ExpectedJSONValue) == 0 {
			return errors.New("expected_json_path and expected_json_value must be configured together")
		}
		assertions++
		if _, err := parseJSONPath(item.ExpectedJSONPath); err != nil {
			return fmt.Errorf("invalid expected_json_path: %w", err)
		}
		var expected any
		if err := decodeJSON(item.ExpectedJSONValue, &expected); err != nil {
			return fmt.Errorf("invalid expected_json_value: %w", err)
		}
	}
	if item.JudgeCriteria != "" || item.JudgeScene != "" || item.JudgeMinScore != nil {
		if strings.TrimSpace(item.JudgeCriteria) == "" {
			return errors.New("judge_criteria is required for a judge assertion")
		}
		assertions++
	}
	if assertions != 1 {
		return errors.New("exactly one contains, exact, regex, JSON Path, or judge assertion is required")
	}
	return nil
}

func hasCostRates(cases []evalCase, rates costRates) bool {
	if rates.inputPerMillionUSD > 0 || rates.outputPerMillionUSD > 0 {
		return true
	}
	for _, item := range cases {
		if item.InputCostPerMillionUSD != nil && *item.InputCostPerMillionUSD > 0 ||
			item.OutputCostPerMillionUSD != nil && *item.OutputCostPerMillionUSD > 0 ||
			item.JudgeInputCostPerMillionUSD != nil && *item.JudgeInputCostPerMillionUSD > 0 ||
			item.JudgeOutputCostPerMillionUSD != nil && *item.JudgeOutputCostPerMillionUSD > 0 {
			return true
		}
	}
	return false
}

func matchExpected(item evalCase, answer string) (string, error) {
	switch {
	case item.ExpectedContains != "":
		if strings.Contains(strings.ToLower(answer), strings.ToLower(item.ExpectedContains)) {
			return "contains", nil
		}
		return "contains", errors.New("answer does not contain expected text")
	case item.ExpectedExact != "":
		if answer == item.ExpectedExact {
			return "exact", nil
		}
		return "exact", errors.New("answer does not exactly match expected text")
	case item.ExpectedRegex != "":
		matched, err := regexp.MatchString(item.ExpectedRegex, answer)
		if err != nil {
			return "regex", err
		}
		if matched {
			return "regex", nil
		}
		return "regex", errors.New("answer does not match expected regex")
	case item.ExpectedJSONPath != "":
		var document any
		if err := decodeJSON([]byte(answer), &document); err != nil {
			return "json_path", fmt.Errorf("answer is not valid JSON: %w", err)
		}
		steps, _ := parseJSONPath(item.ExpectedJSONPath)
		actual, err := resolveJSONPath(document, steps)
		if err != nil {
			return "json_path", err
		}
		var expected any
		if err := decodeJSON(item.ExpectedJSONValue, &expected); err != nil {
			return "json_path", err
		}
		if reflect.DeepEqual(actual, expected) {
			return "json_path", nil
		}
		return "json_path", fmt.Errorf("JSON Path value mismatch: got %v", actual)
	default:
		return "", errors.New("assertion is not configured")
	}
}

func assertionType(item evalCase) string {
	switch {
	case item.ExpectedContains != "":
		return "contains"
	case item.ExpectedExact != "":
		return "exact"
	case item.ExpectedRegex != "":
		return "regex"
	case item.ExpectedJSONPath != "":
		return "json_path"
	case item.JudgeCriteria != "":
		return "judge"
	default:
		return ""
	}
}

type jsonPathStep struct {
	field string
	index *int
}

func parseJSONPath(path string) ([]jsonPathStep, error) {
	if path == "$" {
		return nil, nil
	}
	if !strings.HasPrefix(path, "$") {
		return nil, errors.New("path must start with $")
	}
	var steps []jsonPathStep
	for position := 1; position < len(path); {
		switch path[position] {
		case '.':
			position++
			start := position
			for position < len(path) && path[position] != '.' && path[position] != '[' {
				position++
			}
			if start == position {
				return nil, errors.New("empty field name")
			}
			steps = append(steps, jsonPathStep{field: path[start:position]})
		case '[':
			end := strings.IndexByte(path[position:], ']')
			if end < 0 {
				return nil, errors.New("unclosed array index")
			}
			end += position
			index, err := strconv.Atoi(path[position+1 : end])
			if err != nil || index < 0 {
				return nil, errors.New("array index must be a non-negative integer")
			}
			steps = append(steps, jsonPathStep{index: &index})
			position = end + 1
		default:
			return nil, fmt.Errorf("unexpected character %q", path[position])
		}
	}
	return steps, nil
}

func resolveJSONPath(value any, steps []jsonPathStep) (any, error) {
	current := value
	for _, step := range steps {
		if step.index != nil {
			items, ok := current.([]any)
			if !ok {
				return nil, errors.New("JSON Path expected an array")
			}
			if *step.index >= len(items) {
				return nil, fmt.Errorf("JSON Path array index %d is out of range", *step.index)
			}
			current = items[*step.index]
			continue
		}
		object, ok := current.(map[string]any)
		if !ok {
			return nil, errors.New("JSON Path expected an object")
		}
		next, ok := object[step.field]
		if !ok {
			return nil, fmt.Errorf("JSON Path field %q was not found", step.field)
		}
		current = next
	}
	return current, nil
}

func decodeJSON(raw []byte, destination any) error {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
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

func judgeLLM(ctx context.Context, item evalCase, answer string) (judgeResult, types.TokenUsage, error) {
	scene := item.JudgeScene
	if scene == "" {
		scene = config.LLMSceneAnswerVerifier
	}
	schema := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"score":  map[string]any{"type": "number", "minimum": 0, "maximum": 1},
			"reason": map[string]any{"type": "string", "maxLength": 1000},
		},
		"required": []string{"score", "reason"},
	}
	prompt := fmt.Sprintf("Evaluation criteria:\n%s\n\nOriginal prompt:\n%s\n\nCandidate answer:\n%s", item.JudgeCriteria, item.UserPrompt, answer)
	var result judgeResult
	usage, err := llmcore.CallJSON(ctx, llmcore.ConfigForScene(scene), "Score the candidate answer against the criteria from 0 to 1. Be strict and return JSON only.", prompt, schema, &result)
	if err != nil {
		return judgeResult{}, usage, err
	}
	if result.Score < 0 || result.Score > 1 {
		return judgeResult{}, usage, fmt.Errorf("judge returned score %g outside [0,1]", result.Score)
	}
	return result, usage, nil
}

func writeCaseResult(output io.Writer, encoder *json.Encoder, format string, result caseResult) error {
	if format == "json" {
		return encoder.Encode(result)
	}
	judgeScore := ""
	if result.JudgeScore != nil {
		judgeScore = strconv.FormatFloat(*result.JudgeScore, 'f', 3, 64)
	}
	_, err := fmt.Fprintf(output, "name=%q scene=%s pass=%t assertion=%s judge_score=%s judge_reason=%q latency=%s tokens=%d estimated_cost_usd=%.8f error=%s\n", result.Name, result.Scene, result.Pass, result.Assertion, judgeScore, result.JudgeReason, time.Duration(result.LatencyMS)*time.Millisecond, result.Tokens, result.EstimatedCostUSD, result.Error)
	return err
}

func writeSummary(output io.Writer, encoder *json.Encoder, format string, summary evalSummary) error {
	if format == "json" {
		return encoder.Encode(summary)
	}
	_, err := fmt.Fprintf(output, "summary cases=%d passed=%d pass_rate=%.2f total_tokens=%d estimated_cost_usd=%.8f total_latency=%s stopped_reason=%s\n", summary.Cases, summary.Passed, summary.PassRate, summary.TotalTokens, summary.EstimatedCostUSD, time.Duration(summary.TotalLatencyMS)*time.Millisecond, summary.StoppedReason)
	return err
}
