package metrics

import (
	"context"
	"sync"
	"time"

	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	api "go.opentelemetry.io/otel/metric"
)

type Snapshot struct {
	PlannerCalls            int64         `json:"planner_calls"`
	PlannerFailures         int64         `json:"planner_failures"`
	PlannerLatencySum       time.Duration `json:"planner_latency_sum"`
	WriterCalls             int64         `json:"writer_calls"`
	WriterFailures          int64         `json:"writer_failures"`
	WriterLatencySum        time.Duration `json:"writer_latency_sum"`
	ExecutorCalls           int64         `json:"executor_calls"`
	ExecutorFailures        int64         `json:"executor_failures"`
	ExecutorLatencySum      time.Duration `json:"executor_latency_sum"`
	RunAllCalls             int64         `json:"run_all_calls"`
	TasksCompleted          int64         `json:"tasks_completed"`
	FallbackHits            int64         `json:"fallback_hits"`
	PromptTokens            int64         `json:"prompt_tokens"`
	CompletionTokens        int64         `json:"completion_tokens"`
	TotalTokens             int64         `json:"total_tokens"`
	LLMSceneCalls           int64         `json:"llm_scene_calls"`
	LLMSceneErrors          int64         `json:"llm_scene_errors"`
	LLMPromptTokens         int64         `json:"llm_prompt_tokens"`
	LLMCompletionTokens     int64         `json:"llm_completion_tokens"`
	LLMTotalTokens          int64         `json:"llm_total_tokens"`
	LLMEstimatedCostUSD     float64       `json:"llm_estimated_cost_usd"`
	LLMCircuitOpened        int64         `json:"llm_circuit_opened"`
	LLMCircuitRejected      int64         `json:"llm_circuit_rejected"`
	LLMRetryBudgetExhausted int64         `json:"llm_retry_budget_exhausted"`
	LLMFallbackSucceeded    int64         `json:"llm_fallback_succeeded"`
	LLMFallbackFailed       int64         `json:"llm_fallback_failed"`
}

type Collector struct {
	mu sync.Mutex
	s  Snapshot

	plannerCalls     api.Int64Counter
	plannerFailures  api.Int64Counter
	plannerLatencyMs api.Float64Histogram

	writerCalls     api.Int64Counter
	writerFailures  api.Int64Counter
	writerLatencyMs api.Float64Histogram

	executorCalls     api.Int64Counter
	executorFailures  api.Int64Counter
	executorLatencyMs api.Float64Histogram

	runAllCalls    api.Int64Counter
	tasksCompleted api.Int64Counter
	fallbackHits   api.Int64Counter

	promptTokens             api.Int64Counter
	completionTokens         api.Int64Counter
	totalTokens              api.Int64Counter
	llmSceneCalls            api.Int64Counter
	llmSceneErrors           api.Int64Counter
	llmSceneLatency          api.Float64Histogram
	llmScenePromptTokens     api.Int64Counter
	llmSceneCompletionTokens api.Int64Counter
	llmSceneTotalTokens      api.Int64Counter
	llmSceneEstimatedCost    api.Float64Counter
	llmCircuitOpened         api.Int64Counter
	llmCircuitRejected       api.Int64Counter
	llmRetryBudgetExhausted  api.Int64Counter
	llmFallbackSucceeded     api.Int64Counter
	llmFallbackFailed        api.Int64Counter
}

func NewCollector() *Collector {
	meter := otel.Meter("ai-agent")

	plannerCalls, _ := meter.Int64Counter("agent.planner.calls")
	plannerFailures, _ := meter.Int64Counter("agent.planner.failures")
	plannerLatencyMs, _ := meter.Float64Histogram("agent.planner.latency_ms")

	writerCalls, _ := meter.Int64Counter("agent.writer.calls")
	writerFailures, _ := meter.Int64Counter("agent.writer.failures")
	writerLatencyMs, _ := meter.Float64Histogram("agent.writer.latency_ms")

	executorCalls, _ := meter.Int64Counter("agent.executor.calls")
	executorFailures, _ := meter.Int64Counter("agent.executor.failures")
	executorLatencyMs, _ := meter.Float64Histogram("agent.executor.latency_ms")

	runAllCalls, _ := meter.Int64Counter("agent.run_all.calls")
	tasksCompleted, _ := meter.Int64Counter("agent.tasks.completed")
	fallbackHits, _ := meter.Int64Counter("agent.planner.fallback_hits")

	promptTokens, _ := meter.Int64Counter("agent.tokens.prompt")
	completionTokens, _ := meter.Int64Counter("agent.tokens.completion")
	totalTokens, _ := meter.Int64Counter("agent.tokens.total")
	llmSceneCalls, _ := meter.Int64Counter("agent.llm.scene.calls")
	llmSceneErrors, _ := meter.Int64Counter("agent.llm.scene.errors")
	llmSceneLatency, _ := meter.Float64Histogram("agent.llm.scene.latency_ms")
	llmScenePromptTokens, _ := meter.Int64Counter("agent.llm.scene.prompt_tokens")
	llmSceneCompletionTokens, _ := meter.Int64Counter("agent.llm.scene.completion_tokens")
	llmSceneTotalTokens, _ := meter.Int64Counter("agent.llm.scene.total_tokens")
	llmSceneEstimatedCost, _ := meter.Float64Counter("agent.llm.scene.estimated_cost_usd")
	llmCircuitOpened, _ := meter.Int64Counter("agent.llm.circuit.opened")
	llmCircuitRejected, _ := meter.Int64Counter("agent.llm.circuit.rejected")
	llmRetryBudgetExhausted, _ := meter.Int64Counter("agent.llm.retry.budget_exhausted")
	llmFallbackSucceeded, _ := meter.Int64Counter("agent.llm.fallback.succeeded")
	llmFallbackFailed, _ := meter.Int64Counter("agent.llm.fallback.failed")

	return &Collector{
		plannerCalls:             plannerCalls,
		plannerFailures:          plannerFailures,
		plannerLatencyMs:         plannerLatencyMs,
		writerCalls:              writerCalls,
		writerFailures:           writerFailures,
		writerLatencyMs:          writerLatencyMs,
		executorCalls:            executorCalls,
		executorFailures:         executorFailures,
		executorLatencyMs:        executorLatencyMs,
		runAllCalls:              runAllCalls,
		tasksCompleted:           tasksCompleted,
		fallbackHits:             fallbackHits,
		promptTokens:             promptTokens,
		completionTokens:         completionTokens,
		totalTokens:              totalTokens,
		llmSceneCalls:            llmSceneCalls,
		llmSceneErrors:           llmSceneErrors,
		llmSceneLatency:          llmSceneLatency,
		llmScenePromptTokens:     llmScenePromptTokens,
		llmSceneCompletionTokens: llmSceneCompletionTokens,
		llmSceneTotalTokens:      llmSceneTotalTokens,
		llmSceneEstimatedCost:    llmSceneEstimatedCost,
		llmCircuitOpened:         llmCircuitOpened,
		llmCircuitRejected:       llmCircuitRejected,
		llmRetryBudgetExhausted:  llmRetryBudgetExhausted,
		llmFallbackSucceeded:     llmFallbackSucceeded,
		llmFallbackFailed:        llmFallbackFailed,
	}
}

func (c *Collector) ObserveLLMCall(event llmcore.CallEvent) {
	c.ObserveLLMCallContext(context.Background(), event)
}

func (c *Collector) ObserveLLMCallContext(ctx context.Context, event llmcore.CallEvent) {
	attrs := api.WithAttributes(attribute.String("llm.scene", event.Scene), attribute.String("llm.provider", event.Provider), attribute.String("llm.model", event.Model), attribute.Int("llm.attempts", event.Attempts), attribute.Bool("llm.fallback_used", event.FallbackUsed))
	c.mu.Lock()
	c.s.LLMSceneCalls++
	c.s.LLMPromptTokens += int64(event.Usage.PromptTokens)
	c.s.LLMCompletionTokens += int64(event.Usage.CompletionTokens)
	c.s.LLMTotalTokens += int64(event.Usage.TotalTokens)
	c.s.LLMEstimatedCostUSD += event.EstimatedCostUSD
	if event.Err != nil {
		c.s.LLMSceneErrors++
	}
	c.mu.Unlock()
	c.llmSceneCalls.Add(ctx, 1, attrs)
	c.llmSceneLatency.Record(ctx, float64(event.Duration.Milliseconds()), attrs)
	c.llmScenePromptTokens.Add(ctx, int64(event.Usage.PromptTokens), attrs)
	c.llmSceneCompletionTokens.Add(ctx, int64(event.Usage.CompletionTokens), attrs)
	c.llmSceneTotalTokens.Add(ctx, int64(event.Usage.TotalTokens), attrs)
	c.llmSceneEstimatedCost.Add(ctx, event.EstimatedCostUSD, attrs)
	if event.Err != nil {
		c.llmSceneErrors.Add(ctx, 1, attrs)
	}
}

func (c *Collector) ObserveLLMReliability(ctx context.Context, event llmcore.ReliabilityEvent) {
	attrs := api.WithAttributes(attribute.String("llm.scene", event.Scene), attribute.String("llm.provider", event.Provider), attribute.String("llm.model", event.Model), attribute.String("llm.fallback_scene", event.FallbackScene))
	c.mu.Lock()
	switch event.Kind {
	case llmcore.ReliabilityCircuitOpened:
		c.s.LLMCircuitOpened++
	case llmcore.ReliabilityCircuitRejected:
		c.s.LLMCircuitRejected++
	case llmcore.ReliabilityRetryBudgetExhausted:
		c.s.LLMRetryBudgetExhausted++
	case llmcore.ReliabilityFallbackSucceeded:
		c.s.LLMFallbackSucceeded++
	case llmcore.ReliabilityFallbackFailed:
		c.s.LLMFallbackFailed++
	}
	c.mu.Unlock()
	switch event.Kind {
	case llmcore.ReliabilityCircuitOpened:
		c.llmCircuitOpened.Add(ctx, 1, attrs)
	case llmcore.ReliabilityCircuitRejected:
		c.llmCircuitRejected.Add(ctx, 1, attrs)
	case llmcore.ReliabilityRetryBudgetExhausted:
		c.llmRetryBudgetExhausted.Add(ctx, 1, attrs)
	case llmcore.ReliabilityFallbackSucceeded:
		c.llmFallbackSucceeded.Add(ctx, 1, attrs)
	case llmcore.ReliabilityFallbackFailed:
		c.llmFallbackFailed.Add(ctx, 1, attrs)
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

// ObserveWriter records a WriterAgent LLM call duration and outcome.
// It uses dedicated writer metrics so writer latency is not conflated with
// planner latency in dashboards and alerts.
func (c *Collector) ObserveWriter(latency time.Duration, err error) {
	c.mu.Lock()
	c.s.WriterCalls++
	c.s.WriterLatencySum += latency
	if err != nil {
		c.s.WriterFailures++
	}
	c.mu.Unlock()

	ctx := context.Background()
	c.writerCalls.Add(ctx, 1)
	c.writerLatencyMs.Record(ctx, float64(latency.Milliseconds()))
	if err != nil {
		c.writerFailures.Add(ctx, 1)
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

func (c *Collector) ObserveTokens(prompt, completion, total int, role string) {
	c.mu.Lock()
	c.s.PromptTokens += int64(prompt)
	c.s.CompletionTokens += int64(completion)
	c.s.TotalTokens += int64(total)
	c.mu.Unlock()

	ctx := context.Background()
	attrs := api.WithAttributes(attribute.String("role", role))
	c.promptTokens.Add(ctx, int64(prompt), attrs)
	c.completionTokens.Add(ctx, int64(completion), attrs)
	c.totalTokens.Add(ctx, int64(total), attrs)
}

func (c *Collector) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.s
}
