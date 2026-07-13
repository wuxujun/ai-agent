package planner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/wuxujun/ai-agent/internal/config"
	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/llmprovider"
	"github.com/wuxujun/ai-agent/internal/logger"
	"github.com/wuxujun/ai-agent/internal/telemetry"
	"github.com/wuxujun/ai-agent/internal/tools"
	"github.com/wuxujun/ai-agent/internal/types"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/genai"
)

var tracer = otel.Tracer("ai-agent/planner")
var log = logger.Component("planner")

type ProviderType string

const (
	ProviderOpenAIResponses ProviderType = llmprovider.OpenAIResponses
	ProviderOpenAI          ProviderType = llmprovider.OpenAI
	ProviderGemini          ProviderType = llmprovider.Gemini
	ProviderOllama          ProviderType = llmprovider.Ollama
	ProviderLiteLLM         ProviderType = llmprovider.LiteLLM
)

// LLMPlanner calls an LLM to produce a PlanDecision for each agent step.
//
// APIKey, Model, and BaseURL stored in the struct act as *static overrides*:
// if a field is non-empty it takes precedence over the live configuration.
// If a field is empty, PlanNext resolves the value from config.Get() at call
// time, so a hot-config-reload (e.g. API-key rotation) is picked up
// automatically without restarting the server.
type LLMPlanner struct {
	Scene            string
	Compressor       ContextCompressor
	ArgumentRepairer ToolArgumentRepairer
	Provider         ProviderType
	// APIKey overrides config.Get().ResolveLLMAPIKey when non-empty.
	APIKey string
	// Model overrides config.Get().ResolveLLMModel when non-empty.
	Model string
	// BaseURL overrides config.Get().ResolveLLMBaseURL when non-empty.
	BaseURL string
	Client  *http.Client
}

func NewLLMPlanner(apiKey, model, baseURL string) *LLMPlanner {
	return NewLLMPlannerWithProvider(ProviderOpenAIResponses, apiKey, model, baseURL)
}

func NewLLMPlannerWithProvider(provider ProviderType, apiKey, model, baseURL string) *LLMPlanner {
	cfg := config.Get()
	timeout := time.Duration(cfg.LLM.TimeoutSeconds) * time.Second
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	return &LLMPlanner{
		Provider: provider,
		APIKey:   apiKey,
		Model:    model,
		BaseURL:  baseURL,
		Client:   telemetry.NewHTTPClient(timeout),
	}
}

func NewLLMPlannerForScene(scene string) *LLMPlanner {
	p := NewLLMPlannerWithProvider("", "", "", "")
	p.Scene = scene
	return p
}

// resolveCredentials returns the effective (provider, apiKey, model, baseURL)
// for this call by merging the struct's static overrides with the current live
// config. This is called at the top of PlanNext so that any config hot-reload
// (e.g. API key rotation) is reflected immediately on the next LLM request.
func (p *LLMPlanner) resolveCredentials(scene string) (provider ProviderType, apiKey, model, baseURL string, timeout time.Duration) {
	cfg := config.Get() // always read the latest snapshot
	timeout = time.Duration(cfg.LLM.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if scene != "" && p.Provider == "" && p.APIKey == "" && p.Model == "" && p.BaseURL == "" {
		resolved := cfg.ResolveLLMScene(scene)
		return ProviderType(resolved.Provider), resolved.APIKey, resolved.Model, resolved.BaseURL, time.Duration(resolved.TimeoutSeconds) * time.Second
	}

	provider = p.Provider
	if provider == "" {
		provider = ProviderType(cfg.ResolveLLMProvider())
	}

	apiKey = p.APIKey
	if apiKey == "" {
		apiKey = cfg.ResolveLLMAPIKey(string(provider))
	}

	model = p.Model
	if model == "" {
		model = cfg.ResolveLLMModel(string(provider))
	}

	baseURL = p.BaseURL
	if baseURL == "" {
		baseURL = cfg.ResolveLLMBaseURL(string(provider))
	}
	return
}

func (p *LLMPlanner) PlanNext(ctx context.Context, task *types.Task, onChunk func(string)) (*PlanDecision, error) {
	ctx = llmcore.WithTaskBudget(ctx, task)
	ctx = llmcore.WithTaskRoutingHints(ctx, task)
	ctx, span := tracer.Start(ctx, "planner.plan_next")
	defer span.End()
	activeScene := p.Scene
	if p.Provider == "" && p.APIKey == "" && p.Model == "" && p.BaseURL == "" {
		activeScene = llmcore.ResolveRoutedScene(ctx, p.Scene)
	}
	if err := llmcore.ReserveTaskLLMCallForConfig(ctx, llmcore.ConfigForScene(activeScene)); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		llmcore.ObserveReliabilityContext(ctx, llmcore.ReliabilityEvent{Kind: llmcore.ReliabilityTaskBudgetRejected, Scene: activeScene})
		return nil, err
	}

	// Resolve credentials at call time so hot-reloaded config (e.g. rotated API
	// keys) is picked up without restarting the server.
	provider, apiKey, model, baseURL, timeout := p.resolveCredentials(activeScene)
	client := p.Client
	if activeScene != "" {
		client = telemetry.NewHTTPClient(timeout)
	}

	span.SetAttributes(
		attribute.String("agent.task.id", task.ID),
		attribute.String("llm.provider", string(provider)),
		attribute.String("llm.model", model),
		attribute.String("llm.logical_scene", p.Scene),
		attribute.String("llm.scene", activeScene),
		attribute.Int("agent.task.step_count", task.StepCount),
	)

	log.Info("starting planning", "task_id", task.ID, "step_count", task.StepCount, "provider", provider, "model", model)
	promptTask := task
	var compressionUsage types.TokenUsage
	threshold := config.Get().LLM.ContextCompressionTraceThreshold
	tokenThreshold := config.Get().LLM.ContextCompressionTokenThreshold
	_, compressionEnabled := config.Get().LLM.Scenes[config.LLMSceneContextCompressor]
	previousSummary, traceStart := traceSinceSummary(task)
	newTraces := task.Trace[traceStart:]

	// Calculate accumulated tokens across new traces for token-budget-based trigger.
	var accumulatedTokens int
	for _, tr := range newTraces {
		accumulatedTokens += tr.TokenUsage.TotalTokens
	}
	traceLimitHit := threshold > 0 && len(newTraces) >= threshold
	tokenLimitHit := tokenThreshold > 0 && accumulatedTokens >= tokenThreshold

	if p.Compressor != nil && compressionEnabled && llmcore.AllowedForTask(config.LLMSceneContextCompressor, task) && (traceLimitHit || tokenLimitHit) {
		reason := "trace_count"
		if tokenLimitHit && !traceLimitHit {
			reason = "token_budget"
		}
		log.Info("triggering context compression", "task_id", task.ID, "reason", reason, "new_traces", len(newTraces), "accumulated_tokens", accumulatedTokens)
		span.AddEvent("context_compression_triggered", trace.WithAttributes(
			attribute.String("compression.reason", reason),
			attribute.Int("compression.new_trace_count", len(newTraces)),
			attribute.Int("compression.accumulated_tokens", accumulatedTokens),
		))

		if summary, usage, compressErr := p.Compressor.Compress(ctx, task); compressErr != nil {
			log.Warn("context compression failed; using original trace", "task_id", task.ID, "error", compressErr)
		} else {
			compressionUsage = usage
			task.Trace = append(task.Trace, types.StepTrace{Step: task.StepCount, Action: "context_summary", Observation: summary})
			copyTask := *task
			copyTask.Trace = []types.StepTrace{{Step: task.StepCount, Action: "context_summary", Observation: summary}}
			promptTask = &copyTask
		}
	} else if previousSummary != "" {
		copyTask := *task
		copyTask.Trace = []types.StepTrace{{Step: task.StepCount, Action: "context_summary", Observation: combinedTraceContext(previousSummary, newTraces)}}
		promptTask = &copyTask
	}

	prov, err := lookupProvider(provider)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "lookup provider failed")
		return nil, err
	}

	req := PlanRequest{
		Client:       client,
		Model:        model,
		APIKey:       apiKey,
		BaseURL:      baseURL,
		SystemPrompt: BuildSystemPrompt(),
		UserPrompt:   BuildUserPrompt(promptTask),
	}

	log.Info("sending request to provider", "provider", provider, "base_url", baseURL, "model", model)
	maxRetries, fallbackScene := 0, ""
	if activeScene != "" {
		policy := config.Get().ResolveLLMScene(activeScene)
		maxRetries, fallbackScene = policy.MaxRetries, policy.FallbackScene
	}
	var textValue string
	var usage types.TokenUsage
	primaryStarted := time.Now()
	primaryAttempts := 0
	var primaryUsage types.TokenUsage
	primaryResilience := llmcore.ConfigForScene(activeScene)
	primaryResilience.Provider = string(provider)
	primaryResilience.APIKey = apiKey
	primaryResilience.Model = model
	primaryResilience.BaseURL = baseURL
	for attempt := 0; attempt <= maxRetries; attempt++ {
		primaryAttempts++
		if err = llmcore.BeforeAttempt(ctx, primaryResilience); err != nil {
			break
		}
		var attemptUsage types.TokenUsage
		textValue, attemptUsage, err = prov.Plan(ctx, req, onChunk)
		llmcore.RecordAttempt(ctx, primaryResilience, err)
		usage.PromptTokens += attemptUsage.PromptTokens
		usage.CompletionTokens += attemptUsage.CompletionTokens
		usage.TotalTokens += attemptUsage.TotalTokens
		primaryUsage.PromptTokens += attemptUsage.PromptTokens
		primaryUsage.CompletionTokens += attemptUsage.CompletionTokens
		primaryUsage.TotalTokens += attemptUsage.TotalTokens
		if err == nil {
			break
		}
		if attempt == maxRetries || !llmcore.IsRetryable(err) {
			break
		}
		if budgetErr := llmcore.ConsumeRetryForConfig(ctx, primaryResilience, err); budgetErr != nil {
			err = budgetErr
			break
		}
		if waitErr := llmcore.WaitRetry(ctx, attempt, err); waitErr != nil {
			err = waitErr
			break
		}
	}
	primaryCostUSD := llmcore.EstimateCostUSD(primaryResilience, primaryUsage)
	if costErr := llmcore.RecordTaskLLMCost(ctx, primaryCostUSD); costErr != nil {
		err = costErr
		fallbackScene = ""
	}
	llmcore.ObserveContext(ctx, llmcore.CallEvent{Scene: activeScene, Provider: string(provider), Model: model, Usage: primaryUsage, Duration: time.Since(primaryStarted), Err: err, Attempts: primaryAttempts, FallbackUsed: err != nil && fallbackScene != "", EstimatedCostUSD: primaryCostUSD})
	fallbackAttempted := err != nil && fallbackScene != ""
	activeFallback := fallbackScene
	for err != nil && activeFallback != "" {
		if budgetErr := llmcore.ReserveTaskLLMCallForConfig(ctx, llmcore.ConfigForScene(activeFallback)); budgetErr != nil {
			err = budgetErr
			llmcore.ObserveReliabilityContext(ctx, llmcore.ReliabilityEvent{Kind: llmcore.ReliabilityTaskBudgetRejected, Scene: activeFallback})
			break
		}
		span.SetAttributes(attribute.Bool("llm.fallback.triggered", true), attribute.String("llm.fallback.scene", activeFallback))
		fallback := config.Get().ResolveLLMScene(activeFallback)
		fallbackProvider, lookupErr := lookupProvider(ProviderType(fallback.Provider))
		if lookupErr == nil {
			fallbackReq := req
			fallbackReq.Model, fallbackReq.APIKey, fallbackReq.BaseURL = fallback.Model, fallback.APIKey, fallback.BaseURL
			fallbackReq.Client = telemetry.NewHTTPClient(time.Duration(fallback.TimeoutSeconds) * time.Second)
			fallbackStarted := time.Now()
			fallbackAttempts := 0
			var sceneUsage types.TokenUsage
			fallbackResilience := llmcore.ConfigForScene(activeFallback)
			for attempt := 0; attempt <= fallback.MaxRetries; attempt++ {
				fallbackAttempts++
				if err = llmcore.BeforeAttempt(ctx, fallbackResilience); err != nil {
					break
				}
				var fallbackUsage types.TokenUsage
				textValue, fallbackUsage, err = fallbackProvider.Plan(ctx, fallbackReq, onChunk)
				llmcore.RecordAttempt(ctx, fallbackResilience, err)
				usage.PromptTokens += fallbackUsage.PromptTokens
				usage.CompletionTokens += fallbackUsage.CompletionTokens
				usage.TotalTokens += fallbackUsage.TotalTokens
				sceneUsage.PromptTokens += fallbackUsage.PromptTokens
				sceneUsage.CompletionTokens += fallbackUsage.CompletionTokens
				sceneUsage.TotalTokens += fallbackUsage.TotalTokens
				if err == nil {
					break
				}
				if attempt == fallback.MaxRetries || !llmcore.IsRetryable(err) {
					break
				}
				if budgetErr := llmcore.ConsumeRetryForConfig(ctx, fallbackResilience, err); budgetErr != nil {
					err = budgetErr
					break
				}
				if waitErr := llmcore.WaitRetry(ctx, attempt, err); waitErr != nil {
					err = waitErr
					break
				}
			}
			fallbackCostUSD := llmcore.EstimateCostUSD(fallbackResilience, sceneUsage)
			if costErr := llmcore.RecordTaskLLMCost(ctx, fallbackCostUSD); costErr != nil {
				err = costErr
			}
			llmcore.ObserveContext(ctx, llmcore.CallEvent{Scene: activeFallback, Provider: fallback.Provider, Model: fallback.Model, Usage: sceneUsage, Duration: time.Since(fallbackStarted), Err: err, Attempts: fallbackAttempts, FallbackUsed: err != nil && fallback.FallbackScene != "", EstimatedCostUSD: fallbackCostUSD})
		} else {
			err = lookupErr
			llmcore.ObserveContext(ctx, llmcore.CallEvent{Scene: activeFallback, Provider: fallback.Provider, Model: fallback.Model, Err: err, Attempts: 1, FallbackUsed: fallback.FallbackScene != ""})
		}
		if llmcore.IsTaskBudgetError(err) {
			break
		}
		activeFallback = fallback.FallbackScene
	}
	if fallbackAttempted {
		kind := llmcore.ReliabilityFallbackSucceeded
		if err != nil {
			kind = llmcore.ReliabilityFallbackFailed
		}
		llmcore.ObserveReliabilityContext(ctx, llmcore.ReliabilityEvent{Kind: kind, Scene: activeScene, Provider: string(provider), Model: model, FallbackScene: fallbackScene})
	}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "planner provider failed")
		log.Error("provider Plan failed", "task_id", task.ID, "provider", provider, "error", err)
		return nil, err
	}
	if fallbackScene != "" {
		span.SetStatus(codes.Ok, "planner call succeeded")
	}

	var decision PlanDecision
	if err := unmarshalDecision(textValue, &decision); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "decision unmarshal failed")
		log.Error("failed to unmarshal decision JSON", "task_id", task.ID, "raw", textValue, "error", err)
		return nil, fmt.Errorf("invalid planner decision: %w", err)
	}

	if validationErr := ValidateDecision(&decision); validationErr != nil {
		var argumentErr *ToolArgumentValidationError
		if p.ArgumentRepairer != nil && toolArgumentRepairConfigured() && llmcore.AllowedForTask(config.LLMSceneToolArgumentRepair, task) && errors.As(validationErr, &argumentErr) {
			repaired, repairUsage, repairErr := p.ArgumentRepairer.Repair(ctx, task.Goal, argumentErr.Action, decision.Actions[argumentErr.ActionIndex].Parameters, argumentErr)
			addTokenUsage(&usage, repairUsage)
			if repairErr == nil {
				decision.Actions[argumentErr.ActionIndex].Parameters = repaired
				validationErr = ValidateDecision(&decision)
				if validationErr == nil {
					span.AddEvent("tool_arguments_repaired", trace.WithAttributes(attribute.String("tool.action", argumentErr.Action), attribute.Int("tool.action_index", argumentErr.ActionIndex)))
				}
			} else {
				validationErr = fmt.Errorf("%w; tool argument repair failed: %v", validationErr, repairErr)
			}
		}
		if validationErr != nil {
			span.RecordError(validationErr)
			span.SetStatus(codes.Error, "decision validation failed")
			log.Error("validation failed for decision", "task_id", task.ID, "decision", decision, "error", validationErr)
			return nil, validationErr
		}
	}

	var actionNames []string
	for _, ac := range decision.Actions {
		actionNames = append(actionNames, ac.Action)
	}
	span.SetAttributes(
		attribute.StringSlice("agent.planner.actions", actionNames),
		attribute.Bool("agent.planner.stop", decision.Stop),
		attribute.Int("llm.usage.prompt_tokens", usage.PromptTokens),
		attribute.Int("llm.usage.completion_tokens", usage.CompletionTokens),
		attribute.Int("llm.usage.total_tokens", usage.TotalTokens),
	)

	log.Info("decision ready", "task_id", task.ID, "thought", decision.ThoughtSummary, "actions", actionNames, "stop", decision.Stop, "final_answer", decision.FinalAnswer, "num_actions", len(decision.Actions))

	decision.TokenUsage = usage
	decision.TokenUsage.PromptTokens += compressionUsage.PromptTokens
	decision.TokenUsage.CompletionTokens += compressionUsage.CompletionTokens
	decision.TokenUsage.TotalTokens += compressionUsage.TotalTokens
	return &decision, nil
}

func addTokenUsage(total *types.TokenUsage, additional types.TokenUsage) {
	total.PromptTokens += additional.PromptTokens
	total.CompletionTokens += additional.CompletionTokens
	total.TotalTokens += additional.TotalTokens
}

func extractStructuredText(raw map[string]any) (string, error) {
	output, ok := raw["output"].([]any)
	if !ok || len(output) == 0 {
		return "", errors.New("missing output")
	}

	for _, item := range output {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		content, ok := obj["content"].([]any)
		if !ok {
			continue
		}
		for _, c := range content {
			part, ok := c.(map[string]any)
			if !ok {
				continue
			}
			if txt, ok := part["text"].(string); ok && txt != "" {
				return txt, nil
			}
		}
	}

	return "", errors.New("structured text not found")
}

// PlannerDecisionGenAISchema is the genai (Gemini) equivalent of
// PlannerDecisionSchema. Its action enum and parameter set are likewise derived
// from the tool registry, so the OpenAI and Gemini planner paths stay in sync
// automatically as tools are added.
func PlannerDecisionGenAISchema() *genai.Schema {
	registered := tools.DefaultRegistry.List() // sorted by name

	actions := make([]string, 0, len(registered)+1)
	paramProps := map[string]*genai.Schema{}
	for _, t := range registered {
		actions = append(actions, t.Name())
		for name, spec := range t.Parameters() {
			// Derive the genai parameter type from the tool's JSON-Schema spec
			// instead of hardcoding TypeString. This keeps the Gemini schema
			// structurally aligned with the OpenAI PlannerDecisionSchema (which
			// uses the raw spec) so a non-string parameter added to any tool is
			// reflected on both planner paths automatically.
			paramProps[name] = genaiSchemaFromSpec(spec)
		}
	}
	actions = append(actions, "none")

	paramKeys := make([]string, 0, len(paramProps))
	for k := range paramProps {
		paramKeys = append(paramKeys, k)
	}
	sort.Strings(paramKeys)

	// Each element of the actions array mirrors ActionCall (action + parameters).
	// This MUST stay structurally identical to the OpenAI PlannerDecisionSchema:
	// PlanDecision unmarshals into Actions []ActionCall, so a singular
	// action/parameters shape (the previous version) deserialised to zero
	// actions and failed ValidateDecision on every Gemini turn.
	actionItem := &genai.Schema{
		Type: genai.TypeObject,
		Properties: map[string]*genai.Schema{
			"action": {
				Type:        genai.TypeString,
				Enum:        actions,
				Description: "The single next action to execute. Use none only when stop is true.",
			},
			"parameters": {
				Type:       genai.TypeObject,
				Properties: paramProps,
				Required:   paramKeys,
			},
		},
		Required: []string{"action", "parameters"},
	}

	return &genai.Schema{
		Type: genai.TypeObject,
		Properties: map[string]*genai.Schema{
			"thought_summary": {
				Type:        genai.TypeString,
				Description: "A brief internal summary of why this next step was chosen, under 30 words.",
			},
			"stop": {
				Type:        genai.TypeBoolean,
				Description: "Whether the agent should stop now.",
			},
			"final_answer": {
				Type:        genai.TypeString,
				Description: "If stop is true, provide a concise final answer; otherwise empty string.",
			},
			"actions": {
				Type:        genai.TypeArray,
				Description: "One or more independent tool actions to execute in parallel. If no tools are needed, use a single 'none' action.",
				Items:       actionItem,
			},
		},
		Required: []string{"thought_summary", "stop", "final_answer", "actions"},
	}
}

// genaiSchemaFromSpec converts a tool parameter's JSON-Schema fragment (as
// returned by Tool.Parameters(), e.g. {"type":"string","description":"..."})
// into the equivalent *genai.Schema. Unknown or missing types fall back to
// string, preserving the previous behaviour for tools that omit a type.
func genaiSchemaFromSpec(spec any) *genai.Schema {
	m, ok := spec.(map[string]any)
	if !ok {
		return &genai.Schema{Type: genai.TypeString}
	}

	out := &genai.Schema{Type: genai.TypeString}
	if desc, ok := m["description"].(string); ok {
		out.Description = desc
	}
	switch t, _ := m["type"].(string); t {
	case "integer":
		out.Type = genai.TypeInteger
	case "number":
		out.Type = genai.TypeNumber
	case "boolean":
		out.Type = genai.TypeBoolean
	case "array":
		out.Type = genai.TypeArray
	case "object":
		out.Type = genai.TypeObject
	default: // "string", "", or unknown
		out.Type = genai.TypeString
	}
	return out
}

func unmarshalDecision(textValue string, decision *PlanDecision) error {
	// First try: direct unmarshal
	if err := json.Unmarshal([]byte(textValue), decision); err == nil {
		return nil
	}

	// Fallback: extract the JSON block between the first '{' and last '}'
	firstIdx := strings.Index(textValue, "{")
	lastIdx := strings.LastIndex(textValue, "}")
	if firstIdx != -1 && lastIdx != -1 && firstIdx < lastIdx {
		cleaned := textValue[firstIdx : lastIdx+1]
		if err := json.Unmarshal([]byte(cleaned), decision); err == nil {
			log.Info("successfully parsed decision JSON after extracting from raw response")
			return nil
		}
	}

	return json.Unmarshal([]byte(textValue), decision)
}
