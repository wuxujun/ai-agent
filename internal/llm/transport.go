package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/wuxujun/ai-agent/internal/telemetry"
	"github.com/wuxujun/ai-agent/internal/types"
	"google.golang.org/genai"
)

type nativeStructuredCaller struct{}

func (nativeStructuredCaller) CallJSON(ctx context.Context, cfg Config, systemPrompt, userPrompt string, schema map[string]any, dest any) (types.TokenUsage, error) {
	if cfg.Provider == "gemini" {
		client, err := GetGeminiClient(cfg.APIKey, cfg.BaseURL)
		if err != nil {
			return types.TokenUsage{}, err
		}
		resp, err := client.Models.GenerateContent(ctx, cfg.Model, []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: userPrompt}}}}, &genai.GenerateContentConfig{SystemInstruction: &genai.Content{Parts: []*genai.Part{{Text: systemPrompt}}}, ResponseMIMEType: "application/json"})
		if err != nil {
			return types.TokenUsage{}, err
		}
		var usage types.TokenUsage
		if resp.UsageMetadata != nil {
			usage = types.TokenUsage{PromptTokens: int(resp.UsageMetadata.PromptTokenCount), CompletionTokens: int(resp.UsageMetadata.CandidatesTokenCount), TotalTokens: int(resp.UsageMetadata.TotalTokenCount)}
		}
		return usage, parseStructuredJSON(resp.Text(), dest)
	}
	body, responseKind, err := structuredRequest(cfg, systemPrompt, userPrompt, schema)
	if err != nil {
		return types.TokenUsage{}, err
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return types.TokenUsage{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.BaseURL, bytes.NewReader(payload))
	if err != nil {
		return types.TokenUsage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}
	resp, err := telemetry.NewHTTPClient(cfg.Timeout).Do(req)
	if err != nil {
		return types.TokenUsage{}, err
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if readErr != nil {
		return types.TokenUsage{}, readErr
	}
	if len(raw) > 4<<20 {
		return types.TokenUsage{}, fmt.Errorf("LLM response exceeds 4 MiB limit")
	}
	if resp.StatusCode >= 300 {
		return types.TokenUsage{}, NewHTTPStatusError(resp.StatusCode, resp.Header, raw)
	}
	text, usage, err := extractStructuredResponse(responseKind, raw)
	if err != nil {
		return usage, err
	}
	return usage, parseStructuredJSON(text, dest)
}

func structuredRequest(cfg Config, systemPrompt, userPrompt string, schema map[string]any) (map[string]any, string, error) {
	switch cfg.Provider {
	case "openai-responses":
		return map[string]any{"model": cfg.Model, "input": []map[string]any{{"role": "system", "content": []map[string]any{{"type": "input_text", "text": systemPrompt}}}, {"role": "user", "content": []map[string]any{{"type": "input_text", "text": userPrompt}}}}, "text": map[string]any{"format": map[string]any{"type": "json_schema", "name": "response", "strict": true, "schema": schema}}}, "responses", nil
	case "openai", "litellm":
		return map[string]any{"model": cfg.Model, "messages": []map[string]any{{"role": "system", "content": systemPrompt}, {"role": "user", "content": userPrompt}}, "response_format": map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "response", "strict": true, "schema": schema}}}, "chat", nil
	case "ollama":
		return map[string]any{"model": cfg.Model, "messages": []map[string]any{{"role": "system", "content": systemPrompt}, {"role": "user", "content": userPrompt}}, "stream": false, "format": schema}, "ollama", nil
	default:
		return nil, "", fmt.Errorf("unsupported provider: %s", cfg.Provider)
	}
}

func extractStructuredResponse(kind string, raw []byte) (string, types.TokenUsage, error) {
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return "", types.TokenUsage{}, err
	}
	usage := usageFromMap(root["usage"])
	if kind == "ollama" {
		usage.PromptTokens = intNumber(root["prompt_eval_count"])
		usage.CompletionTokens = intNumber(root["eval_count"])
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
		if msg, ok := root["message"].(map[string]any); ok {
			if text, ok := msg["content"].(string); ok {
				return text, usage, nil
			}
		}
	}
	if kind == "chat" {
		if choices, ok := root["choices"].([]any); ok && len(choices) > 0 {
			if choice, ok := choices[0].(map[string]any); ok {
				if msg, ok := choice["message"].(map[string]any); ok {
					if text, ok := msg["content"].(string); ok {
						return text, usage, nil
					}
				}
			}
		}
	}
	if output, ok := root["output"].([]any); ok {
		for _, item := range output {
			if obj, ok := item.(map[string]any); ok {
				if content, ok := obj["content"].([]any); ok {
					for _, part := range content {
						if value, ok := part.(map[string]any); ok {
							if text, ok := value["text"].(string); ok {
								return text, usage, nil
							}
						}
					}
				}
			}
		}
	}
	return "", usage, fmt.Errorf("structured response text not found")
}

func intNumber(value any) int {
	if n, ok := value.(float64); ok {
		return int(n)
	}
	return 0
}
func usageFromMap(value any) types.TokenUsage {
	m, _ := value.(map[string]any)
	p := intNumber(m["prompt_tokens"])
	if p == 0 {
		p = intNumber(m["input_tokens"])
	}
	c := intNumber(m["completion_tokens"])
	if c == 0 {
		c = intNumber(m["output_tokens"])
	}
	total := intNumber(m["total_tokens"])
	if total == 0 {
		total = p + c
	}
	return types.TokenUsage{PromptTokens: p, CompletionTokens: c, TotalTokens: total}
}
func parseStructuredJSON(text string, dest any) error {
	if json.Unmarshal([]byte(text), dest) == nil {
		return nil
	}
	first, last := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if first >= 0 && last > first && json.Unmarshal([]byte(text[first:last+1]), dest) == nil {
		return nil
	}
	return fmt.Errorf("could not parse JSON from LLM response")
}
