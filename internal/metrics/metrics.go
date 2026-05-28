package metrics

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	api "go.opentelemetry.io/otel/metric"
)

type Snapshot struct {
	PlannerCalls       int64         `json:"planner_calls"`
	PlannerFailures    int64         `json:"planner_failures"`
	PlannerLatencySum  time.Duration `json:"planner_latency_sum"`
	ExecutorCalls      int64         `json:"executor_calls"`
	ExecutorFailures   int64         `json:"executor_failures"`
	ExecutorLatencySum time.Duration `json:"executor_latency_sum"`
	RunAllCalls        int64         `json:"run_all_calls"`
	TasksCompleted     int64         `json:"tasks_completed"`
	FallbackHits       int64         `json:"fallback_hits"`
}

type Collector struct {
	mu sync.Mutex
	s  Snapshot

	plannerCalls     api.Int64Counter
	plannerFailures  api.Int64Counter
	plannerLatencyMs api.Float64Histogram

	executorCalls     api.Int64Counter
	executorFailures  api.Int64Counter
	executorLatencyMs api.Float64Histogram

	runAllCalls    api.Int64Counter
	tasksCompleted api.Int64Counter
	fallbackHits   api.Int64Counter
}

func NewCollector() *Collector {
	meter := otel.Meter("agent-runtime")

	plannerCalls, _ := meter.Int64Counter("agent.planner.calls")
	plannerFailures, _ := meter.Int64Counter("agent.planner.failures")
	plannerLatencyMs, _ := meter.Float64Histogram("agent.planner.latency_ms")

	executorCalls, _ := meter.Int64Counter("agent.executor.calls")
	executorFailures, _ := meter.Int64Counter("agent.executor.failures")
	executorLatencyMs, _ := meter.Float64Histogram("agent.executor.latency_ms")

	runAllCalls, _ := meter.Int64Counter("agent.run_all.calls")
	tasksCompleted, _ := meter.Int64Counter("agent.tasks.completed")
	fallbackHits, _ := meter.Int64Counter("agent.planner.fallback_hits")

	return &Collector{
		plannerCalls:      plannerCalls,
		plannerFailures:   plannerFailures,
		plannerLatencyMs:  plannerLatencyMs,
		executorCalls:     executorCalls,
		executorFailures:  executorFailures,
		executorLatencyMs: executorLatencyMs,
		runAllCalls:       runAllCalls,
		tasksCompleted:    tasksCompleted,
		fallbackHits:      fallbackHits,
	}
}

func (c *Collector) ObservePlanner(latency time.Duration, err error) {
	c.mu.Lock()
	c.s.PlannerCalls++
	c.s.PlannerLatencySum += latency
	if err != nil {
		c.s.PlannerFailures++
	}
	c.mu.Unlock()

	ctx := context.Background()
	c.plannerCalls.Add(ctx, 1)
	c.plannerLatencyMs.Record(ctx, float64(latency.Milliseconds()))
	if err != nil {
		c.plannerFailures.Add(ctx, 1)
	}
}

func (c *Collector) ObserveExecutor(latency time.Duration, err error, action string) {
	c.mu.Lock()
	c.s.ExecutorCalls++
	c.s.ExecutorLatencySum += latency
	if err != nil {
		c.s.ExecutorFailures++
	}
	c.mu.Unlock()

	ctx := context.Background()
	attrs := api.WithAttributes(attribute.String("action", action))
	c.executorCalls.Add(ctx, 1, attrs)
	c.executorLatencyMs.Record(ctx, float64(latency.Milliseconds()), attrs)
	if err != nil {
		c.executorFailures.Add(ctx, 1, attrs)
	}
}

func (c *Collector) IncRunAll() {
	c.mu.Lock()
	c.s.RunAllCalls++
	c.mu.Unlock()

	c.runAllCalls.Add(context.Background(), 1)
}

func (c *Collector) IncCompleted() {
	c.mu.Lock()
	c.s.TasksCompleted++
	c.mu.Unlock()

	c.tasksCompleted.Add(context.Background(), 1)
}

func (c *Collector) IncFallbackHit() {
	c.mu.Lock()
	c.s.FallbackHits++
	c.mu.Unlock()

	c.fallbackHits.Add(context.Background(), 1)
}

func (c *Collector) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.s
}
