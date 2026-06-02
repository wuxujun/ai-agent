package planner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/wuxujun/ai-agent/internal/tools"
	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/types"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"google.golang.org/genai"
)

var tracer = otel.Tracer("ai-agent/planner")

type ProviderType string

const (
	ProviderOpenAIResponses ProviderType = "openai-responses"
	ProviderOpenAI          ProviderType = "openai"
	ProviderGemini          ProviderType = "gemini"
	ProviderOllama          ProviderType = "ollama"
)

type LLMPlanner struct {
	Provider ProviderType
	APIKey   string
	Model    string
	BaseURL  string
	Client   *http.Client
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

func (p *LLMPlanner) PlanNext(ctx context.Context, task *types.Task) (*PlanDecision, error) {
	ctx, span := tracer.Start(ctx, "planner.plan_next")
	defer span.End()

	span.SetAttributes(
		attribute.String("agent.task.id", task.ID),
		attribute.String("llm.provider", string(p.Provider)),
		attribute.String("llm.model", p.Model),
		attribute.Int("agent.task.step_count", task.StepCount),
	)

	log.Printf("[LLM Planner] Starting planning for task %s, step_count: %d, provider: %s, model: %s", task.ID, task.StepCount, p.Provider, p.Model)

	systemPrompt := BuildSystemPrompt()
	userPrompt := BuildUserPrompt(task)

	if p.Provider == ProviderGemini {
		client, err := GetGeminiClient(p.APIKey, p.BaseURL)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "failed to create genai client")
			log.Printf("[LLM Planner Error] Task %s - failed to create genai client: %v", task.ID, err)
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

		log.Printf("[LLM Planner] Sending request to Gemini: model=%s", p.Model)
		resp, err := client.Models.GenerateContent(ctx, p.Model, contents, config)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "gemini request failed")
			log.Printf("[LLM Planner Error] Task %s - Gemini request failed: %v", task.ID, err)
			return nil, err
		}

		textValue := resp.Text()
		var decision PlanDecision
		if err := unmarshalDecision(textValue, &decision); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "decision unmarshal failed")
			log.Printf("[LLM Planner Error] Task %s - failed to unmarshal Gemini response %q: %v", task.ID, textValue, err)
			return nil, fmt.Errorf("invalid planner decision: %w", err)
		}

		if err := ValidateDecision(&decision); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "decision validation failed")
			log.Printf("[LLM Planner Error] Task %s - validation failed for decision %+v: %v", task.ID, decision, err)
			return nil, err
		}

		span.SetAttributes(
			attribute.String("agent.planner.action", decision.Action),
			attribute.Bool("agent.planner.stop", decision.Stop),
		)

		log.Printf("[LLM Planner] Task %s decision - Thought: %q | Action: %q | Stop: %t | FinalAnswer: %q | Parameters: %+v",
			task.ID, decision.ThoughtSummary, decision.Action, decision.Stop, decision.FinalAnswer, decision.Parameters)

		return &decision, nil
	}

	prov, err := lookupProvider(p.Provider)
	if err != nil {
		return nil, err
	}

	req, respParser, err := prov.BuildRequest(ctx, p.Client, p.Model, p.APIKey, p.BaseURL, systemPrompt, userPrompt, PlannerDecisionSchema())
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "build request failed")
		log.Printf("[LLM Planner Error] Task %s - failed to build request: %v", task.ID, err)
		return nil, err
	}

	log.Printf("[LLM Planner] Sending request to API (%s): %s", p.Provider, p.BaseURL)
	resp, err := p.Client.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "planner request failed")
		log.Printf("[LLM Planner Error] Task %s - request failed: %v", task.ID, err)
		return nil, err
	}
	defer resp.Body.Close()

	span.SetAttributes(attribute.Int("http.status_code", resp.StatusCode))
	log.Printf("[LLM Planner] API response received with status %d", resp.StatusCode)

	if resp.StatusCode >= 300 {
		var bodyErr bytes.Buffer
		_, _ = bodyErr.ReadFrom(resp.Body)
		err := fmt.Errorf("planner API returned status %d: %s", resp.StatusCode, bodyErr.String())
		span.RecordError(err)
		span.SetStatus(codes.Error, "planner API error")
		log.Printf("[LLM Planner Error] Task %s - API returned status %d: %s", task.ID, resp.StatusCode, bodyErr.String())
		return nil, err
	}

	var rawBody bytes.Buffer
	if _, err := rawBody.ReadFrom(resp.Body); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "read body failed")
		log.Printf("[LLM Planner Error] Task %s - failed to read response body: %v", task.ID, err)
		return nil, err
	}

	textValue, err := respParser(rawBody.Bytes())
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "parse response failed")
		log.Printf("[LLM Planner Error] Task %s - failed to parse response: %v", task.ID, err)
		return nil, err
	}

	var decision PlanDecision
	if err := unmarshalDecision(textValue, &decision); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "decision unmarshal failed")
		log.Printf("[LLM Planner Error] Task %s - failed to unmarshal decision JSON: %v", task.ID, err)
		return nil, fmt.Errorf("invalid planner decision: %w", err)
	}

	if err := ValidateDecision(&decision); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "decision validation failed")
		log.Printf("[LLM Planner Error] Task %s - validation failed for decision %+v: %v", task.ID, decision, err)
		return nil, err
	}

	span.SetAttributes(
		attribute.String("agent.planner.action", decision.Action),
		attribute.Bool("agent.planner.stop", decision.Stop),
	)

	log.Printf("[LLM Planner] Task %s decision - Thought: %q | Action: %q | Stop: %t | FinalAnswer: %q | Parameters: %+v",
		task.ID, decision.ThoughtSummary, decision.Action, decision.Stop, decision.FinalAnswer, decision.Parameters)

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
		Required: []string{"thought_summary", "stop", "final_answer", "action", "parameters"},
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
			log.Printf("[LLM Planner] Successfully parsed decision JSON after extracting from raw response")
			return nil
		}
	}

	return json.Unmarshal([]byte(textValue), decision)
}
