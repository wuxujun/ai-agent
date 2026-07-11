package multiagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/telemetry"
	"github.com/wuxujun/ai-agent/internal/types"
	"google.golang.org/genai"
)

// LLMConfig holds the configuration required to call an LLM provider.
// It is intentionally compatible with the existing planner.LLMPlanner config.
type LLMConfig struct {
	Provider planner.ProviderType
	APIKey   string
	Model    string
	BaseURL  string
	Timeout  time.Duration
}

// DefaultLLMConfig builds an LLMConfig from the same environment variables
// used by the main planner, so no additional configuration is needed.
func DefaultLLMConfig() LLMConfig {
	cfg := config.Get()
	provider := planner.ProviderType(cfg.ResolveLLMProvider())
	apiKey := cfg.ResolveLLMAPIKey(string(provider))
	model := cfg.ResolveLLMModel(string(provider))
	baseURL := cfg.ResolveLLMBaseURL(string(provider))
	timeout := time.Duration(cfg.LLM.TimeoutSeconds) * time.Second
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	return LLMConfig{
		Provider: provider,
		APIKey:   apiKey,
		Model:    model,
		BaseURL:  baseURL,
		Timeout:  timeout,
	}
}

// callLLMJSON sends a system+user prompt to the configured LLM and unmarshals
// the JSON response into dest. schema describes the expected JSON structure for
// providers that support structured output (OpenAI json_schema, Ollama format).
// It returns TokenUsage and error.
func callLLMJSON(ctx context.Context, cfg LLMConfig, systemPrompt, userPrompt string, schema map[string]any, dest any) (types.TokenUsage, error) {
	if cfg.Provider == planner.ProviderGemini {
		return callGeminiJSON(ctx, cfg, systemPrompt, userPrompt, dest)
	}
	return callHTTPJSON(ctx, cfg, systemPrompt, userPrompt, schema, dest)
}

// ── Gemini path ──────────────────────────────────────────────────────────────

func callGeminiJSON(ctx context.Context, cfg LLMConfig, systemPrompt, userPrompt string, dest any) (types.TokenUsage, error) {
	var usage types.TokenUsage
	client, err := planner.GetGeminiClient(cfg.APIKey, cfg.BaseURL)
	if err != nil {
		return usage, fmt.Errorf("gemini client: %w", err)
	}

	contents := []*genai.Content{
		{Role: "user", Parts: []*genai.Part{{Text: userPrompt}}},
	}
	genCfg := &genai.GenerateContentConfig{
		SystemInstruction: &genai.Content{
			Parts: []*genai.Part{{Text: systemPrompt}},
		},
		ResponseMIMEType: "application/json",
	}

	resp, err := client.Models.GenerateContent(ctx, cfg.Model, contents, genCfg)
	if err != nil {
		return usage, fmt.Errorf("gemini generate: %w", err)
	}

	if resp.UsageMetadata != nil {
		usage.PromptTokens = int(resp.UsageMetadata.PromptTokenCount)
		usage.CompletionTokens = int(resp.UsageMetadata.CandidatesTokenCount)
		usage.TotalTokens = int(resp.UsageMetadata.TotalTokenCount)
	}

	return usage, parseJSONInto(resp.Text(), dest)
}

// ── HTTP path (OpenAI / Ollama) ──────────────────────────────────────────────

func callHTTPJSON(ctx context.Context, cfg LLMConfig, systemPrompt, userPrompt string, schema map[string]any, dest any) (types.TokenUsage, error) {
	client := telemetry.NewHTTPClient(cfg.Timeout)

	var (
		reqBody     map[string]any
		extractText func([]byte) (string, types.TokenUsage, error)
	)

	switch cfg.Provider {
	case planner.ProviderOpenAIResponses:
		reqBody = map[string]any{
			"model": cfg.Model,
			"input": []map[string]any{
				{"role": "system", "content": []map[string]any{{"type": "input_text", "text": systemPrompt}}},
				{"role": "user", "content": []map[string]any{{"type": "input_text", "text": userPrompt}}},
			},
			"text": map[string]any{
				"format": map[string]any{
					"type":   "json_schema",
					"name":   "response",
					"strict": true,
					"schema": schema,
				},
			},
		}
		extractText = extractResponsesText

	case planner.ProviderOpenAI:
		reqBody = map[string]any{
			"model": cfg.Model,
			"messages": []map[string]any{
				{"role": "system", "content": systemPrompt},
				{"role": "user", "content": userPrompt},
			},
			"response_format": map[string]any{
				"type": "json_schema",
				"json_schema": map[string]any{
					"name":   "response",
					"strict": true,
					"schema": schema,
				},
			},
		}
		extractText = extractChatText

	case planner.ProviderOllama:
		reqBody = map[string]any{
			"model": cfg.Model,
			"messages": []map[string]any{
				{"role": "system", "content": systemPrompt},
				{"role": "user", "content": userPrompt},
			},
			"stream": false,
			"format": schema,
		}
		extractText = extractOllamaText

	default:
		return types.TokenUsage{}, fmt.Errorf("unsupported provider: %s", cfg.Provider)
	}

	b, err := json.Marshal(reqBody)
	if err != nil {
		return types.TokenUsage{}, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", cfg.BaseURL, bytes.NewReader(b))
	if err != nil {
		return types.TokenUsage{}, fmt.Errorf("build request: %w", err)
	}
	if cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return types.TokenUsage{}, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		return types.TokenUsage{}, fmt.Errorf("LLM API status %d: %s", resp.StatusCode, buf.String())
	}

	var rawBody bytes.Buffer
	if _, err := rawBody.ReadFrom(resp.Body); err != nil {
		return types.TokenUsage{}, fmt.Errorf("read body: %w", err)
	}

	text, usage, err := extractText(rawBody.Bytes())
	if err != nil {
		return usage, fmt.Errorf("extract text: %w", err)
	}
	return usage, parseJSONInto(text, dest)
}

// ── response parsers ─────────────────────────────────────────────────────────

func parseJSONInto(text string, dest any) error {
	if err := json.Unmarshal([]byte(text), dest); err == nil {
		return nil
	}
	// Fallback: extract the outermost JSON object or array
	first := strings.Index(text, "{")
	last := strings.LastIndex(text, "}")
	if first != -1 && last > first {
		if err := json.Unmarshal([]byte(text[first:last+1]), dest); err == nil {
			return nil
		}
	}
	return fmt.Errorf("could not parse JSON from LLM response: %q", text)
}

func extractResponsesText(raw []byte) (string, types.TokenUsage, error) {
	var m map[string]any
	var usage types.TokenUsage
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

	output, ok := m["output"].([]any)
	if !ok || len(output) == 0 {
		return "", usage, errors.New("missing output field in responses response")
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
				return txt, usage, nil
			}
		}
	}
	return "", usage, errors.New("text not found in responses output")
}

func extractChatText(raw []byte) (string, types.TokenUsage, error) {
	var m struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	var usage types.TokenUsage
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", usage, err
	}
	usage.PromptTokens = m.Usage.PromptTokens
	usage.CompletionTokens = m.Usage.CompletionTokens
	usage.TotalTokens = m.Usage.TotalTokens

	if len(m.Choices) == 0 {
		return "", usage, errors.New("empty choices in chat response")
	}
	return m.Choices[0].Message.Content, usage, nil
}

func extractOllamaText(raw []byte) (string, types.TokenUsage, error) {
	var m struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		PromptEvalCount int `json:"prompt_eval_count"`
		EvalCount       int `json:"eval_count"`
	}
	var usage types.TokenUsage
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", usage, err
	}
	usage.PromptTokens = m.PromptEvalCount
	usage.CompletionTokens = m.EvalCount
	usage.TotalTokens = m.PromptEvalCount + m.EvalCount

	return m.Message.Content, usage, nil
}
