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
	"github.com/wuxujun/ai-agent/internal/logger"
	"github.com/wuxujun/ai-agent/internal/telemetry"
	"github.com/wuxujun/ai-agent/internal/tools"
	"github.com/wuxujun/ai-agent/internal/types"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"google.golang.org/genai"
)

var tracer = otel.Tracer("ai-agent/planner")
var log = logger.Component("planner")

type ProviderType string

const (
	ProviderOpenAIResponses ProviderType = "openai-responses"
	ProviderOpenAI          ProviderType = "openai"
	ProviderGemini          ProviderType = "gemini"
	ProviderOllama          ProviderType = "ollama"
	ProviderLiteLLM         ProviderType = "litellm"
)

// LLMPlanner calls an LLM to produce a PlanDecision for each agent step.
//
// APIKey, Model, and BaseURL stored in the struct act as *static overrides*:
// if a field is non-empty it takes precedence over the live configuration.
// If a field is empty, PlanNext resolves the value from config.Get() at call
// time, so a hot-config-reload (e.g. API-key rotation) is picked up
// automatically without restarting the server.
type LLMPlanner struct {
	Scene      string
	Compressor ContextCompressor
	Provider   ProviderType
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
func (p *LLMPlanner) resolveCredentials() (provider ProviderType, apiKey, model, baseURL string, timeout time.Duration) {
	cfg := config.Get() // always read the latest snapshot
	timeout = time.Duration(cfg.LLM.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if p.Scene != "" && p.Provider == "" && p.APIKey == "" && p.Model == "" && p.BaseURL == "" {
		resolved := cfg.ResolveLLMScene(p.Scene)
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
	ctx, span := tracer.Start(ctx, "planner.plan_next")
	defer span.End()

	// Resolve credentials at call time so hot-reloaded config (e.g. rotated API
	// keys) is picked up without restarting the server.
	provider, apiKey, model, baseURL, timeout := p.resolveCredentials()
	client := p.Client
	if p.Scene != "" {
		client = telemetry.NewHTTPClient(timeout)
	}

	span.SetAttributes(
		attribute.String("agent.task.id", task.ID),
		attribute.String("llm.provider", string(provider)),
		attribute.String("llm.model", model),
		attribute.Int("agent.task.step_count", task.StepCount),
	)

	log.Info("starting planning", "task_id", task.ID, "step_count", task.StepCount, "provider", provider, "model", model)
	promptTask := task
	var compressionUsage types.TokenUsage
	threshold := config.Get().LLM.ContextCompressionTraceThreshold
	_, compressionEnabled := config.Get().LLM.Scenes[config.LLMSceneContextCompressor]
	previousSummary, traceStart := traceSinceSummary(task)
	newTraces := task.Trace[traceStart:]
	if p.Compressor != nil && compressionEnabled && llmcore.AllowedForTask(config.LLMSceneContextCompressor, task) && threshold > 0 && len(newTraces) >= threshold {
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
	textValue, usage, err := prov.Plan(ctx, req, onChunk)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "planner provider failed")
		log.Error("provider Plan failed", "task_id", task.ID, "provider", provider, "error", err)
		return nil, err
	}

	var decision PlanDecision
	if err := unmarshalDecision(textValue, &decision); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "decision unmarshal failed")
		log.Error("failed to unmarshal decision JSON", "task_id", task.ID, "raw", textValue, "error", err)
		return nil, fmt.Errorf("invalid planner decision: %w", err)
	}

	if err := ValidateDecision(&decision); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "decision validation failed")
		log.Error("validation failed for decision", "task_id", task.ID, "decision", decision, "error", err)
		return nil, err
	}

	var actionNames []string
	for _, ac := range decision.Actions {
		actionNames = append(actionNames, ac.Action)
	}
	span.SetAttributes(
		attribute.StringSlice("agent.planner.actions", actionNames),
		attribute.Bool("agent.planner.stop", decision.Stop),
	)

	log.Info("decision ready", "task_id", task.ID, "thought", decision.ThoughtSummary, "actions", actionNames, "stop", decision.Stop, "final_answer", decision.FinalAnswer, "num_actions", len(decision.Actions))

	decision.TokenUsage = usage
	decision.TokenUsage.PromptTokens += compressionUsage.PromptTokens
	decision.TokenUsage.CompletionTokens += compressionUsage.CompletionTokens
	decision.TokenUsage.TotalTokens += compressionUsage.TotalTokens
	return &decision, nil
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
