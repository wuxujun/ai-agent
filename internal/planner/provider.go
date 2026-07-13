package planner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/types"
)

// PlanRequest carries the per-call inputs every provider needs to issue one
// planning round-trip. It deliberately omits the response schema: each provider
// owns its own schema construction (HTTP backends use PlannerDecisionSchema;
// the Gemini backend uses PlannerDecisionGenAISchema) so the contract does not
// leak transport-specific types.
type PlanRequest struct {
	Client       *http.Client
	Model        string
	APIKey       string
	BaseURL      string
	SystemPrompt string
	UserPrompt   string
}

// LLMProvider is the plugin interface for LLM backends. Implement Plan to add
// a new provider; non-HTTP backends (Gemini SDK, etc.) plug in via the same
// interface without forcing the planner to know about *http.Request.
type LLMProvider interface {
	// Name returns the provider identifier (used for logging and metrics).
	Name() ProviderType
	// Plan performs a single planning round-trip and returns the decision text
	// to unmarshal plus the token usage. onChunk is called as incremental output
	// arrives (streaming providers); it may be nil.
	Plan(ctx context.Context, req PlanRequest, onChunk func(string)) (string, types.TokenUsage, error)
}

// providerRegistry maps provider names to their implementations.
var providerRegistry = map[ProviderType]LLMProvider{}

// RegisterProvider registers a custom LLM provider. Safe to call from init().
func RegisterProvider(p LLMProvider) {
	providerRegistry[p.Name()] = p
}

func init() {
	RegisterProvider(&openAIResponsesProvider{})
	RegisterProvider(&openAIChatProvider{})
	RegisterProvider(&ollamaProvider{})
	RegisterProvider(&geminiProvider{})
	RegisterProvider(&liteLLMProvider{})
}

// runHTTPPlan is the shared scaffolding for HTTP-shaped providers: build the
// request, dispatch it, surface a useful error on non-2xx, then hand the
// response body to the provider-specific parser. Centralising this keeps the
// status-code / body-on-error handling identical across the OpenAI and Ollama
// providers — previously it lived inside LLMPlanner.PlanNext and was easy to
// drift.
func runHTTPPlan(req *http.Request, client *http.Client, parser func(*http.Response, func(string)) (string, types.TokenUsage, error), onChunk func(string)) (string, types.TokenUsage, error) {
	resp, err := client.Do(req)
	if err != nil {
		return "", types.TokenUsage{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, (4<<10)+1))
		return "", types.TokenUsage{}, llmcore.NewHTTPStatusError(resp.StatusCode, resp.Header, body)
	}
	return parser(resp, onChunk)
}

// ── OpenAI Responses API ──────────────────────────────────────────────────────

type openAIResponsesProvider struct{}

func (p *openAIResponsesProvider) Name() ProviderType { return ProviderOpenAIResponses }

func (p *openAIResponsesProvider) Plan(ctx context.Context, req PlanRequest, onChunk func(string)) (string, types.TokenUsage, error) {
	reqBody := map[string]any{
		"model": req.Model,
		"input": []map[string]any{
			{
				"role": "system",
				"content": []map[string]any{
					{"type": "input_text", "text": req.SystemPrompt},
				},
			},
			{
				"role": "user",
				"content": []map[string]any{
					{"type": "input_text", "text": req.UserPrompt},
				},
			},
		},
		"text": map[string]any{
			"format": map[string]any{
				"type":   "json_schema",
				"name":   "planner_decision",
				"strict": true,
				"schema": PlannerDecisionSchema(),
			},
		},
	}
	b, err := json.Marshal(reqBody)
	if err != nil {
		return "", types.TokenUsage{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST", req.BaseURL, bytes.NewReader(b))
	if err != nil {
		return "", types.TokenUsage{}, err
	}
	if req.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+req.APIKey)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	return runHTTPPlan(httpReq, req.Client, parseOpenAIResponses, onChunk)
}

// firstFloat returns the first key in keys whose value in m is a JSON number.
// Used to read usage fields that differ across OpenAI API shapes (Responses:
// input_tokens/output_tokens; Chat Completions: prompt_tokens/completion_tokens).
func firstFloat(m map[string]any, keys ...string) float64 {
	for _, k := range keys {
		if v, ok := m[k].(float64); ok {
			return v
		}
	}
	return 0
}

func parseOpenAIResponses(resp *http.Response, onChunk func(string)) (string, types.TokenUsage, error) {
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
		// The OpenAI Responses API reports usage as input_tokens/output_tokens.
		// Fall back to the older Chat-Completions field names
		// (prompt_tokens/completion_tokens) so this parser stays correct if the
		// payload ever comes from a chat-style endpoint.
		usage.PromptTokens = int(firstFloat(u, "input_tokens", "prompt_tokens"))
		usage.CompletionTokens = int(firstFloat(u, "output_tokens", "completion_tokens"))
		usage.TotalTokens = int(firstFloat(u, "total_tokens"))
		if usage.TotalTokens == 0 {
			usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
		}
	}

	txt, err := extractStructuredText(m)
	if onChunk != nil && txt != "" {
		onChunk(txt)
	}
	return txt, usage, err
}

// ── OpenAI Chat Completions API ───────────────────────────────────────────────

type openAIChatProvider struct{}

// liteLLMProvider uses LiteLLM's OpenAI-compatible chat-completions endpoint.
type liteLLMProvider struct{ openAIChatProvider }

func (p *liteLLMProvider) Name() ProviderType { return ProviderLiteLLM }

func (p *openAIChatProvider) Name() ProviderType { return ProviderOpenAI }

func (p *openAIChatProvider) Plan(ctx context.Context, req PlanRequest, onChunk func(string)) (string, types.TokenUsage, error) {
	reqBody := map[string]any{
		"model": req.Model,
		"messages": []map[string]any{
			{"role": "system", "content": req.SystemPrompt},
			{"role": "user", "content": req.UserPrompt},
		},
		"response_format": map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   "planner_decision",
				"strict": true,
				"schema": PlannerDecisionSchema(),
			},
		},
		"stream": true,
		"stream_options": map[string]any{
			"include_usage": true,
		},
	}
	b, err := json.Marshal(reqBody)
	if err != nil {
		return "", types.TokenUsage{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST", req.BaseURL, bytes.NewReader(b))
	if err != nil {
		return "", types.TokenUsage{}, err
	}
	if req.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+req.APIKey)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	return runHTTPPlan(httpReq, req.Client, parseOpenAIChat, onChunk)
}

func parseOpenAIChat(resp *http.Response, onChunk func(string)) (string, types.TokenUsage, error) {
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

// ── Ollama API ────────────────────────────────────────────────────────────────

type ollamaProvider struct{}

func (p *ollamaProvider) Name() ProviderType { return ProviderOllama }

func (p *ollamaProvider) Plan(ctx context.Context, req PlanRequest, onChunk func(string)) (string, types.TokenUsage, error) {
	// Perform a quick health check to verify if the Ollama local service is running and the model is pulled.
	// This check is limited to local loopback URLs to avoid failing unit tests that mock remote endpoints.
	isLocal := strings.Contains(req.BaseURL, "localhost") || strings.Contains(req.BaseURL, "127.0.0.1")
	if isLocal {
		if err := ProbeOllama(ctx, req.BaseURL, req.Model); err != nil {
			return "", types.TokenUsage{}, err
		}
	}

	reqBody := map[string]any{
		"model": req.Model,
		"messages": []map[string]any{
			{"role": "system", "content": req.SystemPrompt},
			{"role": "user", "content": req.UserPrompt},
		},
		"stream": true,
		"format": PlannerDecisionSchema(),
	}
	b, err := json.Marshal(reqBody)
	if err != nil {
		return "", types.TokenUsage{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST", req.BaseURL, bytes.NewReader(b))
	if err != nil {
		return "", types.TokenUsage{}, err
	}
	if req.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+req.APIKey)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	return runHTTPPlan(httpReq, req.Client, parseOllama, onChunk)
}

func parseOllama(resp *http.Response, onChunk func(string)) (string, types.TokenUsage, error) {
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

// lookupProvider returns a registered provider by name, or an error.
func lookupProvider(name ProviderType) (LLMProvider, error) {
	if p, ok := providerRegistry[name]; ok {
		return p, nil
	}
	return nil, fmt.Errorf("unsupported LLM provider %q: register it via planner.RegisterProvider()", name)
}
