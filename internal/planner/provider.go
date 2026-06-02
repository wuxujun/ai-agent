package planner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// LLMProvider is the plugin interface for LLM backends.
// Implement this interface to add a new provider without modifying LLMPlanner.
type LLMProvider interface {
	// Name returns the provider identifier (used for logging and metrics).
	Name() ProviderType
	// BuildRequest constructs the HTTP request to send to the LLM API.
	BuildRequest(ctx context.Context, client *http.Client, model, apiKey, baseURL, systemPrompt, userPrompt string, schema map[string]any) (*http.Request, ResponseParser, error)
}

// ResponseParser extracts the text content from the raw HTTP response body.
type ResponseParser func(raw []byte) (string, error)

// providerRegistry maps provider names to their implementations.
var providerRegistry = map[ProviderType]LLMProvider{}

// RegisterProvider registers a custom LLM provider.
// Call this from an init() function or before creating the LLMPlanner.
func RegisterProvider(p LLMProvider) {
	providerRegistry[p.Name()] = p
}

func init() {
	// Register the built-in HTTP-based providers.
	RegisterProvider(&openAIResponsesProvider{})
	RegisterProvider(&openAIChatProvider{})
	RegisterProvider(&ollamaProvider{})
}

// ── OpenAI Responses API ──────────────────────────────────────────────────────

type openAIResponsesProvider struct{}

func (p *openAIResponsesProvider) Name() ProviderType { return ProviderOpenAIResponses }

func (p *openAIResponsesProvider) BuildRequest(ctx context.Context, client *http.Client, model, apiKey, baseURL, systemPrompt, userPrompt string, schema map[string]any) (*http.Request, ResponseParser, error) {
	reqBody := map[string]any{
		"model": model,
		"input": []map[string]any{
			{
				"role": "system",
				"content": []map[string]any{
					{"type": "input_text", "text": systemPrompt},
				},
			},
			{
				"role": "user",
				"content": []map[string]any{
					{"type": "input_text", "text": userPrompt},
				},
			},
		},
		"text": map[string]any{
			"format": map[string]any{
				"type":   "json_schema",
				"name":   "planner_decision",
				"strict": true,
				"schema": schema,
			},
		},
	}
	b, err := json.Marshal(reqBody)
	if err != nil {
		return nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", baseURL, bytes.NewReader(b))
	if err != nil {
		return nil, nil, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	req.Header.Set("Content-Type", "application/json")

	parser := func(raw []byte) (string, error) {
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			return "", err
		}
		return extractStructuredText(m)
	}
	return req, parser, nil
}

// ── OpenAI Chat Completions API ───────────────────────────────────────────────

type openAIChatProvider struct{}

func (p *openAIChatProvider) Name() ProviderType { return ProviderOpenAI }

func (p *openAIChatProvider) BuildRequest(ctx context.Context, client *http.Client, model, apiKey, baseURL, systemPrompt, userPrompt string, schema map[string]any) (*http.Request, ResponseParser, error) {
	reqBody := map[string]any{
		"model": model,
		"messages": []map[string]any{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": userPrompt},
		},
		"response_format": map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   "planner_decision",
				"strict": true,
				"schema": schema,
			},
		},
	}
	b, err := json.Marshal(reqBody)
	if err != nil {
		return nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", baseURL, bytes.NewReader(b))
	if err != nil {
		return nil, nil, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	req.Header.Set("Content-Type", "application/json")

	parser := func(raw []byte) (string, error) {
		type chatResponse struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		var r chatResponse
		if err := json.Unmarshal(raw, &r); err != nil {
			return "", err
		}
		if len(r.Choices) == 0 {
			return "", errors.New("empty choices in OpenAI response")
		}
		return r.Choices[0].Message.Content, nil
	}
	return req, parser, nil
}

// ── Ollama API ────────────────────────────────────────────────────────────────

type ollamaProvider struct{}

func (p *ollamaProvider) Name() ProviderType { return ProviderOllama }

func (p *ollamaProvider) BuildRequest(ctx context.Context, client *http.Client, model, apiKey, baseURL, systemPrompt, userPrompt string, schema map[string]any) (*http.Request, ResponseParser, error) {
	reqBody := map[string]any{
		"model": model,
		"messages": []map[string]any{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": userPrompt},
		},
		"stream": false,
		"format": schema,
	}
	b, err := json.Marshal(reqBody)
	if err != nil {
		return nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", baseURL, bytes.NewReader(b))
	if err != nil {
		return nil, nil, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	req.Header.Set("Content-Type", "application/json")

	parser := func(raw []byte) (string, error) {
		type ollamaResp struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		}
		var r ollamaResp
		if err := json.Unmarshal(raw, &r); err != nil {
			return "", err
		}
		return r.Message.Content, nil
	}
	return req, parser, nil
}

// lookupProvider returns a registered provider by name, or an error.
func lookupProvider(name ProviderType) (LLMProvider, error) {
	if p, ok := providerRegistry[name]; ok {
		return p, nil
	}
	return nil, fmt.Errorf("unsupported LLM provider %q: register it via planner.RegisterProvider()", name)
}
