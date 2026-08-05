package llm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/wuxujun/ai-agent/internal/llmprovider"
	"github.com/wuxujun/ai-agent/internal/telemetry"
	"github.com/wuxujun/ai-agent/internal/types"
	"google.golang.org/genai"
)

type nativeStructuredCaller struct{}

func (nativeStructuredCaller) CallJSON(ctx context.Context, cfg Config, systemPrompt, userPrompt string, schema map[string]any, dest any) (types.TokenUsage, error) {
	spec, ok := llmprovider.Lookup(cfg.Provider)
	if !ok {
		return types.TokenUsage{}, fmt.Errorf("unsupported provider: %s", cfg.Provider)
	}
	if !spec.Supports(llmprovider.CapabilityStructuredOutput) {
		return types.TokenUsage{}, fmt.Errorf("provider %s does not support structured output", cfg.Provider)
	}
	if spec.Protocol == llmprovider.ProtocolGemini {
		client, err := GetGeminiClient(cfg.APIKey, cfg.BaseURL)
		if err != nil {
			return types.TokenUsage{}, err
		}
		responseSchema, err := geminiSchemaFromJSON(schema)
		if err != nil {
			return types.TokenUsage{}, fmt.Errorf("invalid Gemini response schema: %w", err)
		}
		resp, err := client.Models.GenerateContent(ctx, cfg.Model, []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: userPrompt}}}}, &genai.GenerateContentConfig{SystemInstruction: &genai.Content{Parts: []*genai.Part{{Text: systemPrompt}}}, ResponseMIMEType: "application/json", ResponseSchema: responseSchema})
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
	return callStructuredHTTP(ctx, cfg, body, responseKind, dest)
}

func (nativeStructuredCaller) CallVisionJSON(ctx context.Context, cfg Config, systemPrompt, userPrompt string, image VisionInput, schema map[string]any, dest any) (types.TokenUsage, error) {
	spec, ok := llmprovider.Lookup(cfg.Provider)
	if !ok {
		return types.TokenUsage{}, fmt.Errorf("unsupported provider: %s", cfg.Provider)
	}
	if !spec.Supports(llmprovider.CapabilityStructuredOutput) || !spec.Supports(llmprovider.CapabilityVision) {
		return types.TokenUsage{}, fmt.Errorf("provider %s does not support structured vision input", cfg.Provider)
	}
	if image.MIMEType == "" || len(image.Data) == 0 {
		return types.TokenUsage{}, fmt.Errorf("vision input requires MIME type and image data")
	}
	if spec.Protocol == llmprovider.ProtocolGemini {
		client, err := GetGeminiClient(cfg.APIKey, cfg.BaseURL)
		if err != nil {
			return types.TokenUsage{}, err
		}
		responseSchema, err := geminiSchemaFromJSON(schema)
		if err != nil {
			return types.TokenUsage{}, fmt.Errorf("invalid Gemini response schema: %w", err)
		}
		parts := []*genai.Part{genai.NewPartFromBytes(image.Data, image.MIMEType), genai.NewPartFromText(userPrompt)}
		resp, err := client.Models.GenerateContent(ctx, cfg.Model, []*genai.Content{{Role: "user", Parts: parts}}, &genai.GenerateContentConfig{SystemInstruction: &genai.Content{Parts: []*genai.Part{{Text: systemPrompt}}}, ResponseMIMEType: "application/json", ResponseSchema: responseSchema})
		if err != nil {
			return types.TokenUsage{}, err
		}
		var usage types.TokenUsage
		if resp.UsageMetadata != nil {
			usage = types.TokenUsage{PromptTokens: int(resp.UsageMetadata.PromptTokenCount), CompletionTokens: int(resp.UsageMetadata.CandidatesTokenCount), TotalTokens: int(resp.UsageMetadata.TotalTokenCount)}
		}
		return usage, parseStructuredJSON(resp.Text(), dest)
	}
	body, responseKind, err := visionStructuredRequest(cfg, systemPrompt, userPrompt, image, schema)
	if err != nil {
		return types.TokenUsage{}, err
	}
	return callStructuredHTTP(ctx, cfg, body, responseKind, dest)
}

func callStructuredHTTP(ctx context.Context, cfg Config, body map[string]any, responseKind string, dest any) (types.TokenUsage, error) {
	if metadata := liteLLMMetadata(ctx, cfg); len(metadata) > 0 {
		body["metadata"] = metadata
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

func visionStructuredRequest(cfg Config, systemPrompt, userPrompt string, image VisionInput, schema map[string]any) (map[string]any, string, error) {
	spec, ok := llmprovider.Lookup(cfg.Provider)
	if !ok {
		return nil, "", fmt.Errorf("unsupported provider: %s", cfg.Provider)
	}
	encoded := base64.StdEncoding.EncodeToString(image.Data)
	dataURL := "data:" + image.MIMEType + ";base64," + encoded
	switch spec.Protocol {
	case llmprovider.ProtocolOpenAIResponses:
		return map[string]any{"model": cfg.Model, "input": []map[string]any{{"role": "system", "content": []map[string]any{{"type": "input_text", "text": systemPrompt}}}, {"role": "user", "content": []map[string]any{{"type": "input_text", "text": userPrompt}, {"type": "input_image", "image_url": dataURL}}}}, "text": map[string]any{"format": map[string]any{"type": "json_schema", "name": "response", "strict": true, "schema": schema}}}, "responses", nil
	case llmprovider.ProtocolOpenAIChat:
		systemPrompt, err := WithJSONSchemaInstruction(systemPrompt, schema)
		if err != nil {
			return nil, "", err
		}
		content := []map[string]any{{"type": "text", "text": userPrompt}, {"type": "image_url", "image_url": map[string]any{"url": dataURL}}}
		return map[string]any{"model": cfg.Model, "messages": []map[string]any{{"role": "system", "content": systemPrompt}, {"role": "user", "content": content}}, "response_format": map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "response", "strict": true, "schema": schema}}}, "chat", nil
	case llmprovider.ProtocolOllama:
		return map[string]any{"model": cfg.Model, "messages": []map[string]any{{"role": "system", "content": systemPrompt}, {"role": "user", "content": userPrompt, "images": []string{encoded}}}, "stream": false, "format": schema}, "ollama", nil
	default:
		return nil, "", fmt.Errorf("provider %s protocol %s is not supported by HTTP vision transport", cfg.Provider, spec.Protocol)
	}
}

func structuredRequest(cfg Config, systemPrompt, userPrompt string, schema map[string]any) (map[string]any, string, error) {
	spec, ok := llmprovider.Lookup(cfg.Provider)
	if !ok {
		return nil, "", fmt.Errorf("unsupported provider: %s", cfg.Provider)
	}
	switch spec.Protocol {
	case llmprovider.ProtocolOpenAIResponses:
		return map[string]any{"model": cfg.Model, "input": []map[string]any{{"role": "system", "content": []map[string]any{{"type": "input_text", "text": systemPrompt}}}, {"role": "user", "content": []map[string]any{{"type": "input_text", "text": userPrompt}}}}, "text": map[string]any{"format": map[string]any{"type": "json_schema", "name": "response", "strict": true, "schema": schema}}}, "responses", nil
	case llmprovider.ProtocolOpenAIChat:
		systemPrompt, err := WithJSONSchemaInstruction(systemPrompt, schema)
		if err != nil {
			return nil, "", err
		}
		return map[string]any{"model": cfg.Model, "messages": []map[string]any{{"role": "system", "content": systemPrompt}, {"role": "user", "content": userPrompt}}, "response_format": map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "response", "strict": true, "schema": schema}}}, "chat", nil
	case llmprovider.ProtocolOllama:
		return map[string]any{"model": cfg.Model, "messages": []map[string]any{{"role": "system", "content": systemPrompt}, {"role": "user", "content": userPrompt}}, "stream": false, "format": schema}, "ollama", nil
	default:
		return nil, "", fmt.Errorf("provider %s protocol %s is not supported by HTTP structured transport", cfg.Provider, spec.Protocol)
	}
}

// WithJSONSchemaInstruction preserves the schema contract when an
// OpenAI-compatible gateway downgrades json_schema to json_object. DashScope
// also requires a message to explicitly mention JSON in that mode.
func WithJSONSchemaInstruction(systemPrompt string, schema map[string]any) (string, error) {
	rawSchema, err := json.Marshal(schema)
	if err != nil {
		return "", fmt.Errorf("marshal JSON response schema: %w", err)
	}
	instruction := "Return valid JSON only. The output must match this JSON Schema:\n" + string(rawSchema)
	if systemPrompt == "" {
		return instruction, nil
	}
	return systemPrompt + "\n\n" + instruction, nil
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

// geminiSchemaFromJSON converts the JSON-Schema subset used by structured LLM
// scenes into the native Gemini response schema. The generic Gemini transport
// previously discarded this schema and sent only application/json, allowing a
// syntactically valid object with different or empty fields to deserialize into
// a zero-valued destination without an error.
func geminiSchemaFromJSON(value any) (*genai.Schema, error) {
	if value == nil {
		return nil, nil
	}
	m, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("schema must be an object, got %T", value)
	}
	out := &genai.Schema{}
	switch kind, _ := m["type"].(string); kind {
	case "":
		// Schemas containing only anyOf intentionally omit a root type.
	case "string":
		out.Type = genai.TypeString
	case "object":
		out.Type = genai.TypeObject
	case "array":
		out.Type = genai.TypeArray
	case "boolean":
		out.Type = genai.TypeBoolean
	case "integer":
		out.Type = genai.TypeInteger
	case "number":
		out.Type = genai.TypeNumber
	default:
		return nil, fmt.Errorf("unsupported schema type %q", kind)
	}
	out.Description, _ = m["description"].(string)
	out.Title, _ = m["title"].(string)
	out.Format, _ = m["format"].(string)
	out.Pattern, _ = m["pattern"].(string)
	out.Required = stringSlice(m["required"])
	out.Enum = stringSlice(m["enum"])
	out.MinLength = int64Pointer(m["minLength"])
	out.MaxLength = int64Pointer(m["maxLength"])
	out.MinItems = int64Pointer(m["minItems"])
	out.MaxItems = int64Pointer(m["maxItems"])
	out.Minimum = float64Pointer(m["minimum"])
	out.Maximum = float64Pointer(m["maximum"])
	if nullable, ok := m["nullable"].(bool); ok {
		out.Nullable = &nullable
	}
	if rawItems, exists := m["items"]; exists {
		items, err := geminiSchemaFromJSON(rawItems)
		if err != nil {
			return nil, fmt.Errorf("items: %w", err)
		}
		out.Items = items
	}
	if rawProperties, ok := m["properties"].(map[string]any); ok {
		out.Properties = make(map[string]*genai.Schema, len(rawProperties))
		for name, raw := range rawProperties {
			property, err := geminiSchemaFromJSON(raw)
			if err != nil {
				return nil, fmt.Errorf("property %q: %w", name, err)
			}
			out.Properties[name] = property
		}
	}
	if rawVariants, ok := m["anyOf"].([]any); ok {
		out.AnyOf = make([]*genai.Schema, 0, len(rawVariants))
		for i, raw := range rawVariants {
			variant, err := geminiSchemaFromJSON(raw)
			if err != nil {
				return nil, fmt.Errorf("anyOf[%d]: %w", i, err)
			}
			out.AnyOf = append(out.AnyOf, variant)
		}
	}
	return out, nil
}

func stringSlice(value any) []string {
	switch values := value.(type) {
	case []string:
		return append([]string(nil), values...)
	case []any:
		result := make([]string, 0, len(values))
		for _, value := range values {
			if text, ok := value.(string); ok {
				result = append(result, text)
			}
		}
		return result
	default:
		return nil
	}
}

func int64Pointer(value any) *int64 {
	var result int64
	switch number := value.(type) {
	case int:
		result = int64(number)
	case int64:
		result = number
	case float64:
		result = int64(number)
	default:
		return nil
	}
	return &result
}

func float64Pointer(value any) *float64 {
	var result float64
	switch number := value.(type) {
	case int:
		result = float64(number)
	case int64:
		result = float64(number)
	case float64:
		result = number
	default:
		return nil
	}
	return &result
}
