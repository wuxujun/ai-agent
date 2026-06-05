package planner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/logger"
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
)

// LLMPlanner calls an LLM to produce a PlanDecision for each agent step.
//
// APIKey, Model, and BaseURL stored in the struct act as *static overrides*:
// if a field is non-empty it takes precedence over the live configuration.
// If a field is empty, PlanNext resolves the value from config.Get() at call
// time, so a hot-config-reload (e.g. API-key rotation) is picked up
// automatically without restarting the server.
type LLMPlanner struct {
	Provider ProviderType
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
		Client: &http.Client{
			Timeout: timeout,
		},
	}
}

// resolveCredentials returns the effective (provider, apiKey, model, baseURL)
// for this call by merging the struct's static overrides with the current live
// config. This is called at the top of PlanNext so that any config hot-reload
// (e.g. API key rotation) is reflected immediately on the next LLM request.
func (p *LLMPlanner) resolveCredentials() (provider ProviderType, apiKey, model, baseURL string) {
	cfg := config.Get() // always read the latest snapshot

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
	provider, apiKey, model, baseURL := p.resolveCredentials()

	span.SetAttributes(
		attribute.String("agent.task.id", task.ID),
		attribute.String("llm.provider", string(provider)),
		attribute.String("llm.model", model),
		attribute.Int("agent.task.step_count", task.StepCount),
	)

	log.Info("starting planning", "task_id", task.ID, "step_count", task.StepCount, "provider", provider, "model", model)

	systemPrompt := BuildSystemPrompt()
	userPrompt := BuildUserPrompt(task)

	if provider == ProviderGemini {
		client, err := GetGeminiClient(apiKey, baseURL)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "failed to create genai client")
			log.Error("failed to create genai client", "task_id", task.ID, "error", err)
			return nil, err
		}

		contents := []*genai.Content{
			{
				Role: "user",
				Parts: []*genai.Part{
					{Text: userPrompt},
				},
			},
		}

		config := &genai.GenerateContentConfig{
			SystemInstruction: &genai.Content{
				Parts: []*genai.Part{
					{Text: systemPrompt},
				},
			},
			ResponseMIMEType: "application/json",
			ResponseSchema:   PlannerDecisionGenAISchema(),
		}

		log.Info("sending stream request to Gemini", "model", model)
		iter := client.Models.GenerateContentStream(ctx, model, contents, config)
		
		var textBuf strings.Builder
		var usage types.TokenUsage
		
		for resp, err := range iter {
			if err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, "gemini request failed")
				log.Error("Gemini stream failed", "task_id", task.ID, "error", err)
				return nil, err
			}
			
			if len(resp.Candidates) > 0 && resp.Candidates[0].Content != nil && len(resp.Candidates[0].Content.Parts) > 0 {
				chunk := resp.Candidates[0].Content.Parts[0].Text
				if chunk != "" {
					textBuf.WriteString(chunk)
					
					if onChunk != nil {
						onChunk(chunk)
					}
				}
			}
			
			if resp.UsageMetadata != nil {
				usage.PromptTokens = int(resp.UsageMetadata.PromptTokenCount)
				usage.CompletionTokens = int(resp.UsageMetadata.CandidatesTokenCount)
				usage.TotalTokens = int(resp.UsageMetadata.TotalTokenCount)
			}
		}

		textValue := textBuf.String()
		var decision PlanDecision
		if err := unmarshalDecision(textValue, &decision); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "decision unmarshal failed")
			log.Error("failed to unmarshal Gemini response", "task_id", task.ID, "raw", textValue, "error", err)
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
		return &decision, nil
	}

	prov, err := lookupProvider(provider)
	if err != nil {
		return nil, err
	}

	req, respParser, err := prov.BuildRequest(ctx, p.Client, model, apiKey, baseURL, systemPrompt, userPrompt, PlannerDecisionSchema())
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "build request failed")
		log.Error("failed to build request", "task_id", task.ID, "error", err)
		return nil, err
	}

	log.Info("sending request to API", "provider", provider, "base_url", baseURL)
	resp, err := p.Client.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "planner request failed")
		log.Error("request failed", "task_id", task.ID, "error", err)
		return nil, err
	}
	defer resp.Body.Close()

	span.SetAttributes(attribute.Int("http.status_code", resp.StatusCode))
	log.Info("API response received", "status_code", resp.StatusCode)

	if resp.StatusCode >= 300 {
		var bodyErr bytes.Buffer
		_, _ = bodyErr.ReadFrom(resp.Body)
		err := fmt.Errorf("planner API returned status %d: %s", resp.StatusCode, bodyErr.String())
		span.RecordError(err)
		span.SetStatus(codes.Error, "planner API error")
		log.Error("API returned error status", "task_id", task.ID, "status_code", resp.StatusCode, "body", bodyErr.String())
		return nil, err
	}

	// Removed raw body reading; the respParser handles streaming the body directly

	textValue, usage, err := respParser(resp, onChunk)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "parse response failed")
		log.Error("failed to parse response", "task_id", task.ID, "error", err)
		return nil, err
	}

	var decision PlanDecision
	if err := unmarshalDecision(textValue, &decision); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "decision unmarshal failed")
		log.Error("failed to unmarshal decision JSON", "task_id", task.ID, "error", err)
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
		for name := range t.Parameters() {
			// All current tool parameters are strings.
			paramProps[name] = &genai.Schema{Type: genai.TypeString}
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
