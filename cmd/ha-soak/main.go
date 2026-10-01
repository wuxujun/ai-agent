package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/wuxujun/ai-agent/internal/hasoak"
)

func main() { os.Exit(run()) }
func run() int {
	var cfg hasoak.Config
	var resultsPath, reportPath string
	flag.StringVar(&cfg.Endpoints[0], "node-a", "", "instance A origin URL")
	flag.StringVar(&cfg.Endpoints[1], "node-b", "", "instance B origin URL")
	flag.BoolVar(&cfg.AllowPrivateNetwork, "allow-private-network", false, "allow explicitly configured private test endpoints")
	flag.StringVar(&cfg.Goal, "goal", "", "operator-approved read-only fixture goal")
	flag.StringVar(&cfg.Workspace, "workspace", "", "shared test workspace path")
	flag.StringVar(&cfg.Team, "team", "", "configured multiagent test team")
	flag.DurationVar(&cfg.Duration, "duration", time.Hour, "admission window; less than 1h is smoke only")
	flag.DurationVar(&cfg.Interval, "interval", 30*time.Second, "minimum gap between admitted tasks")
	flag.DurationVar(&cfg.TaskTimeout, "task-timeout", 3*time.Minute, "per-task deadline")
	flag.DurationVar(&cfg.RequestTimeout, "request-timeout", 20*time.Second, "per-request deadline")
	flag.IntVar(&cfg.Concurrency, "concurrency", 2, "maximum active tasks (1..32)")
	flag.IntVar(&cfg.MaxTasks, "max-tasks", 150, "hard admission cap (1..10000); reaching it early keeps report incomplete")
	flag.IntVar(&cfg.LLMCallBudget, "llm-call-budget", 12, "LLM call limit per task")
	flag.Float64Var(&cfg.LLMCostBudgetUSD, "llm-cost-budget-usd", 0.05, "LLM cost limit per task; configure provider prices on server")
	flag.StringVar(&resultsPath, "results", "", "new JSONL result file (must not exist)")
	flag.StringVar(&reportPath, "report", "", "new JSON summary file (must not exist)")
	flag.Parse()
	cfg.APIKey = os.Getenv("AI_AGENT_HA_API_KEY")
	runner, err := hasoak.New(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if resultsPath == "" || reportPath == "" {
		fmt.Fprintln(os.Stderr, "--results and --report are required")
		return 2
	}
	results, err := os.OpenFile(resultsPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot create results file")
		return 2
	}
	defer results.Close()
	reportFile, err := os.OpenFile(reportPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot create report file")
		return 2
	}
	defer reportFile.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	encoder := json.NewEncoder(results)
	report, runErr := runner.Run(ctx, func(result hasoak.Result) error { return encoder.Encode(result) })
	if err := json.NewEncoder(reportFile).Encode(report); err != nil {
		fmt.Fprintln(os.Stderr, "cannot write report")
		return 2
	}
	if err := results.Sync(); err != nil {
		fmt.Fprintln(os.Stderr, "cannot sync results")
		return 2
	}
	if err := reportFile.Sync(); err != nil {
		fmt.Fprintln(os.Stderr, "cannot sync report")
		return 2
	}
	fmt.Printf("tasks=%d passed=%d failed=%d full_window=%t load_checks_passed=%t full_ha_acceptance=false\n", report.Scheduled, report.Passed, report.Failed, report.WindowComplete, report.LoadChecksPassed)
	if runErr != nil {
		fmt.Fprintln(os.Stderr, runErr)
		return 2
	}
	if !report.LoadChecksPassed {
		return 1
	}
	return 0
}
