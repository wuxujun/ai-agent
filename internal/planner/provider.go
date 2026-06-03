package planner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/wuxujun/ai-agent/internal/types"
)

// LLMProvider is the plugin interface for LLM backends.
// Implement this interface to add a new provider without modifying LLMPlanner.
type LLMProvider interface {
	// Name returns the provider identifier (used for logging and metrics).
	Name() ProviderType
	// BuildRequest constructs the HTTP request to send to the LLM API.
	BuildRequest(ctx context.Context, client *http.Client, model, apiKey, baseURL, systemPrompt, userPrompt string, schema map[string]any) (*http.Request, ResponseParser, error)
}

// ResponseParser extracts the text content and token usage from the raw HTTP response.
// The onChunk callback (if non-nil) is called as incremental output arrives.
type ResponseParser func(resp *http.Response, onChunk func(string)) (string, types.TokenUsage, error)

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

	parser := func(resp *http.Response, onChunk func(string)) (string, types.TokenUsage, error) {
		var m map[string]any
		var usage types.TokenUsage
		
		var rawBody bytes.Buffer
		if _, err := rawBody.ReadFrom(resp.Body); err != nil {
			return "", usage, err
		}
		raw := rawBody.Bytes()

		if err := json.Unmarshal(raw, &m); err != nil {
			return "", usage, err
		}
		
		if u, ok := m["usage"].(map[string]any); ok {
			if p, ok := u["prompt_tokens"].(float64); ok {
				usage.PromptTokens = int(p)
			}
			if c, ok := u["completion_tokens"].(float64); ok {
				usage.CompletionTokens = int(c)
			}
			if t, ok := u["total_tokens"].(float64); ok {
				usage.TotalTokens = int(t)
			}
		}

		txt, err := extractStructuredText(m)
		// No streaming support for responses API yet, but we can emit the whole chunk
		if onChunk != nil && txt != "" {
			onChunk(txt)
		}
		return txt, usage, err
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
		"stream": true,
		"stream_options": map[string]any{
			"include_usage": true,
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

	parser := func(resp *http.Response, onChunk func(string)) (string, types.TokenUsage, error) {
		var textBuf bytes.Buffer
		var usage types.TokenUsage
		
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := strings.TrimPrefix(line, "data: ")
			if data == "[DONE]" {
				break
			}
			
			type chatResponse struct {
				Choices []struct {
					Delta struct {
						Content string `json:"content"`
					} `json:"delta"`
				} `json:"choices"`
				Usage *struct {
					PromptTokens     int `json:"prompt_tokens"`
					CompletionTokens int `json:"completion_tokens"`
					TotalTokens      int `json:"total_tokens"`
				} `json:"usage"`
			}
			var r chatResponse
			if err := json.Unmarshal([]byte(data), &r); err != nil {
				continue
			}
			
			if len(r.Choices) > 0 && r.Choices[0].Delta.Content != "" {
				chunk := r.Choices[0].Delta.Content
				textBuf.WriteString(chunk)
				if onChunk != nil {
					onChunk(chunk)
				}
			}
			if r.Usage != nil {
				usage.PromptTokens = r.Usage.PromptTokens
				usage.CompletionTokens = r.Usage.CompletionTokens
				usage.TotalTokens = r.Usage.TotalTokens
			}
		}
		
		if textBuf.Len() == 0 {
			return "", usage, errors.New("empty choices in OpenAI response")
		}
		return textBuf.String(), usage, scanner.Err()
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
		"stream": true,
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

	parser := func(resp *http.Response, onChunk func(string)) (string, types.TokenUsage, error) {
		var textBuf bytes.Buffer
		var usage types.TokenUsage
		
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}
			
			type ollamaResp struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
				PromptEvalCount int  `json:"prompt_eval_count"`
				EvalCount       int  `json:"eval_count"`
				Done            bool `json:"done"`
			}
			var r ollamaResp
			if err := json.Unmarshal(line, &r); err != nil {
				continue
			}
			
			if r.Message.Content != "" {
				textBuf.WriteString(r.Message.Content)
				if onChunk != nil {
					onChunk(r.Message.Content)
				}
			}
			if r.Done {
				usage.PromptTokens = r.PromptEvalCount
				usage.CompletionTokens = r.EvalCount
				usage.TotalTokens = r.PromptEvalCount + r.EvalCount
			}
		}
		return textBuf.String(), usage, scanner.Err()
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
