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
func callLLMJSON(ctx context.Context, cfg LLMConfig, systemPrompt, userPrompt string, schema map[string]any, dest any) error {
	if cfg.Provider == planner.ProviderGemini {
		return callGeminiJSON(ctx, cfg, systemPrompt, userPrompt, dest)
	}
	return callHTTPJSON(ctx, cfg, systemPrompt, userPrompt, schema, dest)
}

// ── Gemini path ──────────────────────────────────────────────────────────────

func callGeminiJSON(ctx context.Context, cfg LLMConfig, systemPrompt, userPrompt string, dest any) error {
	client, err := planner.GetGeminiClient(cfg.APIKey, cfg.BaseURL)
	if err != nil {
		return fmt.Errorf("gemini client: %w", err)
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
		return fmt.Errorf("gemini generate: %w", err)
	}
	return parseJSONInto(resp.Text(), dest)
}

// ── HTTP path (OpenAI / Ollama) ──────────────────────────────────────────────

func callHTTPJSON(ctx context.Context, cfg LLMConfig, systemPrompt, userPrompt string, schema map[string]any, dest any) error {
	client := &http.Client{Timeout: cfg.Timeout}

	var (
		reqBody     map[string]any
		extractText func([]byte) (string, error)
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
		return fmt.Errorf("unsupported provider: %s", cfg.Provider)
	}

	b, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", cfg.BaseURL, bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		return fmt.Errorf("LLM API status %d: %s", resp.StatusCode, buf.String())
	}

	var rawBody bytes.Buffer
	if _, err := rawBody.ReadFrom(resp.Body); err != nil {
		return fmt.Errorf("read body: %w", err)
	}

	text, err := extractText(rawBody.Bytes())
	if err != nil {
		return fmt.Errorf("extract text: %w", err)
	}
	return parseJSONInto(text, dest)
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

func extractResponsesText(raw []byte) (string, error) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", err
	}
	output, ok := m["output"].([]any)
	if !ok || len(output) == 0 {
		return "", errors.New("missing output field in responses response")
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
	return "", errors.New("text not found in responses output")
}

func extractChatText(raw []byte) (string, error) {
	var m struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", err
	}
	if len(m.Choices) == 0 {
		return "", errors.New("empty choices in chat response")
	}
	return m.Choices[0].Message.Content, nil
}

func extractOllamaText(raw []byte) (string, error) {
	var m struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", err
	}
	return m.Message.Content, nil
}
