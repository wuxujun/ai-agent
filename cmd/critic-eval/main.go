package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/wuxujun/ai-agent/internal/criticeval"
	"github.com/wuxujun/ai-agent/internal/multiagent"
	"github.com/wuxujun/ai-agent/internal/plancritic"
	"github.com/wuxujun/ai-agent/internal/promptmanager"
	"github.com/wuxujun/ai-agent/internal/types"
)

type liveEvaluator struct {
	critic   plancritic.Critic
	selector *promptmanager.Selector
}

type promptVariant struct {
	Selector        string   `json:"selector"`
	PromptName      string   `json:"prompt_name"`
	ResolvedVersion int      `json:"resolved_version"`
	ResolvedLabels  []string `json:"resolved_labels,omitempty"`
}

func (e liveEvaluator) Evaluate(ctx context.Context, evalCase criticeval.Case) (*plancritic.Result, types.TokenUsage, error) {
	if e.selector != nil {
		var err error
		ctx, err = multiagent.WithCriticPromptSelector(ctx, *e.selector)
		if err != nil {
			return nil, types.TokenUsage{}, err
		}
	}
	task := &types.Task{ID: "critic-eval:" + evalCase.Name, Goal: evalCase.Goal}
	return e.critic.Critique(ctx, task, evalCase.Plan)
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("critic-eval", flag.ContinueOnError)
	flags.SetOutput(stderr)
	input := flags.String("input", "evals/plan_critic.yaml", "YAML critic evaluation dataset")
	mode := flags.String("mode", "offline", "evaluation mode: offline or online")
	allowLive := flags.Bool("allow-live", false, "allow online mode to call the configured LLM")
	team := flags.String("team", "", "teams.yaml team used in online mode; empty uses active_team")
	baselineLabel := flags.String("baseline-label", "", "override the baseline Langfuse prompt label")
	baselineVersion := flags.Int("baseline-version", 0, "override the baseline Langfuse prompt version")
	candidateLabel := flags.String("candidate-label", "", "candidate Langfuse prompt label; enables comparison")
	candidateVersion := flags.Int("candidate-version", 0, "candidate Langfuse prompt version; enables comparison")
	timeout := flags.Duration("timeout", 30*time.Second, "timeout for each evaluation case")
	maxCases := flags.Int("max-cases", 0, "maximum cases to run; 0 runs all cases")
	maxTotalTokens := flags.Int("max-total-tokens", 0, "stop before the next case after this token total; 0 disables the limit")
	format := flags.String("format", "text", "output format: text or json")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *mode != "offline" && *mode != "online" {
		fmt.Fprintf(stderr, "unsupported mode %q; use offline or online\n", *mode)
		return 2
	}
	if *mode == "online" && !*allowLive {
		fmt.Fprintln(stderr, "online mode requires explicit --allow-live")
		return 2
	}
	if *timeout <= 0 || *maxCases < 0 || *maxTotalTokens < 0 {
		fmt.Fprintln(stderr, "timeout must be positive and limits must be non-negative")
		return 2
	}
	if *format != "text" && *format != "json" {
		fmt.Fprintf(stderr, "unsupported format %q; use text or json\n", *format)
		return 2
	}
	baselineSelector, err := optionalSelector(*baselineLabel, *baselineVersion)
	if err != nil {
		fmt.Fprintf(stderr, "invalid baseline selector: %v\n", err)
		return 2
	}
	candidateSelector, err := optionalSelector(*candidateLabel, *candidateVersion)
	if err != nil {
		fmt.Fprintf(stderr, "invalid candidate selector: %v\n", err)
		return 2
	}
	if *mode == "offline" && (baselineSelector != nil || candidateSelector != nil) {
		fmt.Fprintln(stderr, "prompt selectors require online mode")
		return 2
	}
	if candidateSelector != nil && *maxTotalTokens > 0 {
		fmt.Fprintln(stderr, "comparison mode cannot be combined with max-total-tokens; use max-cases for symmetric variants")
		return 2
	}
	file, err := os.Open(*input)
	if err != nil {
		fmt.Fprintf(stderr, "open input: %v\n", err)
		return 2
	}
	defer file.Close()
	dataset, err := criticeval.Load(file)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}

	var evaluator criticeval.Evaluator = criticeval.RuleEvaluator{}
	resolvedTeam := "offline"
	selectorDescription := "offline"
	var baselineVariant, candidateVariant promptVariant
	if *mode == "online" {
		if strings.TrimSpace(*team) != "" {
			if err := os.Setenv("AI_AGENT_MULTIAGENT_TEAM", strings.TrimSpace(*team)); err != nil {
				fmt.Fprintf(stderr, "set team: %v\n", err)
				return 2
			}
		}
		teams := multiagent.GetTeamsConfig()
		resolvedTeam = teams.ActiveTeam
		activeTeam, ok := teams.Teams[resolvedTeam]
		if !ok {
			fmt.Fprintf(stderr, "team %q is not defined in teams.yaml\n", resolvedTeam)
			return 2
		}
		configuredSelector, selectorErr := (promptmanager.Selector{Label: activeTeam.Critic.PromptLabel, Version: activeTeam.Critic.PromptVersion}).Normalize()
		if selectorErr != nil {
			fmt.Fprintf(stderr, "team %q has invalid critic prompt selector: %v\n", resolvedTeam, selectorErr)
			return 2
		}
		selectorDescription = "configured:" + configuredSelector.String()
		if baselineSelector != nil {
			selectorDescription = baselineSelector.String()
		} else if candidateSelector != nil {
			// Comparisons resolve both variants strictly. Copy the configured
			// selector so the baseline cannot silently use a local fallback.
			baselineSelector = &configuredSelector
		}
		if candidateSelector != nil {
			promptName, promptNameErr := criticPromptName(activeTeam.Critic)
			if promptNameErr != nil {
				fmt.Fprintf(stderr, "resolve critic prompt name: %v\n", promptNameErr)
				return 2
			}
			baselineResolved, resolveErr := promptmanager.GetManager().ResolveStrict(context.Background(), promptName, *baselineSelector)
			if resolveErr != nil {
				fmt.Fprintf(stderr, "resolve baseline prompt: %v\n", resolveErr)
				return 2
			}
			candidateResolved, resolveErr := promptmanager.GetManager().ResolveStrict(context.Background(), promptName, *candidateSelector)
			if resolveErr != nil {
				fmt.Fprintf(stderr, "resolve candidate prompt: %v\n", resolveErr)
				return 2
			}
			baselineVariant = newPromptVariant(selectorDescription, baselineResolved)
			candidateVariant = newPromptVariant(candidateSelector.String(), candidateResolved)
		}
		evaluator = liveEvaluator{critic: &multiagent.CriticAgent{}, selector: baselineSelector}
	}

	results, summary := criticeval.Run(context.Background(), dataset, evaluator, *timeout, *maxCases, *maxTotalTokens)
	if len(results) == 0 {
		fmt.Fprintln(stderr, "no evaluation cases were run")
		return 2
	}
	if candidateSelector == nil {
		if err := writeResults(stdout, *format, *mode, resolvedTeam, selectorDescription, results, summary); err != nil {
			fmt.Fprintf(stderr, "write results: %v\n", err)
			return 2
		}
		if !summary.ThresholdsPassed {
			return 1
		}
		return 0
	}

	candidateEvaluator := liveEvaluator{critic: &multiagent.CriticAgent{}, selector: candidateSelector}
	candidateResults, candidateSummary := criticeval.Run(context.Background(), dataset, candidateEvaluator, *timeout, *maxCases, 0)
	comparison, err := criticeval.Compare(results, candidateResults, summary, candidateSummary)
	if err != nil {
		fmt.Fprintf(stderr, "compare results: %v\n", err)
		return 2
	}
	if err := writeComparison(stdout, *format, resolvedTeam, baselineVariant, candidateVariant, results, summary, candidateResults, candidateSummary, comparison); err != nil {
		fmt.Fprintf(stderr, "write results: %v\n", err)
		return 2
	}
	if !comparison.Passed {
		return 1
	}
	return 0
}

func criticPromptName(agentCfg multiagent.AgentConfig) (string, error) {
	promptName := strings.TrimSpace(agentCfg.PromptName)
	if promptName == "" {
		promptName = strings.TrimSpace(agentCfg.LangfusePrompt)
	}
	if promptName != "" {
		return promptName, nil
	}
	if strings.TrimSpace(agentCfg.SystemPrompt) != "" {
		return "", fmt.Errorf("prompt comparison requires prompt_name when system_prompt is configured")
	}
	return "multiagent_critic_prompt", nil
}

func newPromptVariant(selector string, resolved promptmanager.ResolvedPrompt) promptVariant {
	return promptVariant{
		Selector: selector, PromptName: resolved.Name, ResolvedVersion: resolved.Version,
		ResolvedLabels: append([]string(nil), resolved.Labels...),
	}
}

func optionalSelector(label string, version int) (*promptmanager.Selector, error) {
	label = strings.TrimSpace(label)
	if label == "" && version == 0 {
		return nil, nil
	}
	selector, err := (promptmanager.Selector{Label: label, Version: version}).Normalize()
	if err != nil {
		return nil, err
	}
	return &selector, nil
}

func writeResults(writer io.Writer, format, mode, team, selector string, results []criticeval.CaseResult, summary criticeval.Summary) error {
	if format == "json" {
		encoder := json.NewEncoder(writer)
		for _, result := range results {
			if err := encoder.Encode(result); err != nil {
				return err
			}
		}
		return encoder.Encode(struct {
			Mode     string `json:"mode"`
			Team     string `json:"team"`
			Selector string `json:"selector"`
			criticeval.Summary
		}{Mode: mode, Team: team, Selector: selector, Summary: summary})
	}
	for _, result := range results {
		status := "PASS"
		if !result.DecisionCorrect || result.Error != "" {
			status = "FAIL"
		}
		fmt.Fprintf(writer, "%s %-32s expected=%t actual=%t categories=%s latency=%dms tokens=%d",
			status, result.Name, result.ExpectedApproved, result.ActualApproved,
			strings.Join(result.ActualIssueCategories, ","), result.LatencyMS, result.TokenUsage.TotalTokens)
		if result.Error != "" {
			fmt.Fprintf(writer, " error=%s", result.Error)
		}
		fmt.Fprintln(writer)
	}
	fmt.Fprintf(writer, "SUMMARY mode=%s team=%s selector=%s cases=%d accuracy=%.3f false_rejection_rate=%.3f false_acceptance_rate=%.3f high_risk_miss_rate=%.3f error_rate=%.3f tokens=%d thresholds_passed=%t\n",
		mode, team, selector, summary.Cases, summary.Accuracy, summary.FalseRejectionRate, summary.FalseAcceptanceRate,
		summary.HighRiskMissRate, summary.ErrorRate, summary.TotalTokens, summary.ThresholdsPassed)
	for _, failure := range summary.FailedThresholds {
		fmt.Fprintf(writer, "THRESHOLD_FAIL %s\n", failure)
	}
	return nil
}

func writeComparison(writer io.Writer, format, team string, baselineVariant, candidateVariant promptVariant, baselineResults []criticeval.CaseResult, baselineSummary criticeval.Summary, candidateResults []criticeval.CaseResult, candidateSummary criticeval.Summary, comparison criticeval.Comparison) error {
	if format == "json" {
		encoder := json.NewEncoder(writer)
		for _, variant := range []struct {
			name     string
			selector string
			results  []criticeval.CaseResult
			summary  criticeval.Summary
		}{{"baseline", baselineVariant.Selector, baselineResults, baselineSummary}, {"candidate", candidateVariant.Selector, candidateResults, candidateSummary}} {
			for _, result := range variant.results {
				if err := encoder.Encode(struct {
					Variant  string `json:"variant"`
					Selector string `json:"selector"`
					criticeval.CaseResult
				}{Variant: variant.name, Selector: variant.selector, CaseResult: result}); err != nil {
					return err
				}
			}
			if err := encoder.Encode(struct {
				Variant  string `json:"variant"`
				Selector string `json:"selector"`
				Team     string `json:"team"`
				criticeval.Summary
			}{Variant: variant.name, Selector: variant.selector, Team: team, Summary: variant.summary}); err != nil {
				return err
			}
		}
		return encoder.Encode(struct {
			Team      string        `json:"team"`
			Baseline  promptVariant `json:"baseline"`
			Candidate promptVariant `json:"candidate"`
			criticeval.Comparison
		}{Team: team, Baseline: baselineVariant, Candidate: candidateVariant, Comparison: comparison})
	}
	fmt.Fprintf(writer, "VARIANT baseline selector=%s prompt=%s resolved_version=%d labels=%s\n", baselineVariant.Selector, baselineVariant.PromptName, baselineVariant.ResolvedVersion, strings.Join(baselineVariant.ResolvedLabels, ","))
	writeTextCases(writer, baselineResults)
	writeTextSummary(writer, baselineSummary)
	fmt.Fprintf(writer, "VARIANT candidate selector=%s prompt=%s resolved_version=%d labels=%s\n", candidateVariant.Selector, candidateVariant.PromptName, candidateVariant.ResolvedVersion, strings.Join(candidateVariant.ResolvedLabels, ","))
	writeTextCases(writer, candidateResults)
	writeTextSummary(writer, candidateSummary)
	fmt.Fprintf(writer, "COMPARISON accuracy_delta=%+.3f category_match_delta=%+.3f false_rejection_delta=%+.3f false_acceptance_delta=%+.3f high_risk_miss_delta=%+.3f error_delta=%+.3f token_delta=%+d latency_ms_delta=%+d regressions=%d improvements=%d passed=%t\n",
		comparison.AccuracyDelta, comparison.CategoryMatchRateDelta, comparison.FalseRejectionRateDelta,
		comparison.FalseAcceptanceRateDelta, comparison.HighRiskMissRateDelta, comparison.ErrorRateDelta,
		comparison.TokenDelta, comparison.LatencyMSDelta, len(comparison.Regressions), len(comparison.Improvements), comparison.Passed)
	for _, regression := range comparison.Regressions {
		fmt.Fprintf(writer, "REGRESSION %s\n", regression)
	}
	for _, improvement := range comparison.Improvements {
		fmt.Fprintf(writer, "IMPROVEMENT %s\n", improvement)
	}
	return nil
}

func writeTextCases(writer io.Writer, results []criticeval.CaseResult) {
	for _, result := range results {
		status := "PASS"
		if !result.DecisionCorrect || result.Error != "" {
			status = "FAIL"
		}
		fmt.Fprintf(writer, "%s %-32s expected=%t actual=%t categories=%s latency=%dms tokens=%d",
			status, result.Name, result.ExpectedApproved, result.ActualApproved,
			strings.Join(result.ActualIssueCategories, ","), result.LatencyMS, result.TokenUsage.TotalTokens)
		if result.Error != "" {
			fmt.Fprintf(writer, " error=%s", result.Error)
		}
		fmt.Fprintln(writer)
	}
}

func writeTextSummary(writer io.Writer, summary criticeval.Summary) {
	fmt.Fprintf(writer, "SUMMARY cases=%d accuracy=%.3f category_match_rate=%.3f false_rejection_rate=%.3f false_acceptance_rate=%.3f high_risk_miss_rate=%.3f error_rate=%.3f tokens=%d thresholds_passed=%t\n",
		summary.Cases, summary.Accuracy, summary.CategoryMatchRate, summary.FalseRejectionRate,
		summary.FalseAcceptanceRate, summary.HighRiskMissRate, summary.ErrorRate, summary.TotalTokens, summary.ThresholdsPassed)
	for _, failure := range summary.FailedThresholds {
		fmt.Fprintf(writer, "THRESHOLD_FAIL %s\n", failure)
	}
}
