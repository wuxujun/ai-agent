package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/logger"
	"github.com/wuxujun/ai-agent/internal/multiagent"
	"github.com/wuxujun/ai-agent/internal/planner"
	"google.golang.org/genai"
)

var log = logger.Component("memory")

// CosineSimilarity calculates the cosine similarity between two float vectors.
func CosineSimilarity(v1, v2 []float32) float32 {
	if len(v1) != len(v2) || len(v1) == 0 {
		return 0
	}
	var dotProduct, norm1, norm2 float32
	for i := range v1 {
		dotProduct += v1[i] * v2[i]
		norm1 += v1[i] * v1[i]
		norm2 += v2[i] * v2[i]
	}
	if norm1 == 0 || norm2 == 0 {
		return 0
	}
	return dotProduct / float32(math.Sqrt(float64(norm1)*float64(norm2)))
}

// GetEmbedding calculates the vector embedding for the given text.
// If the LLM provider has an API key, it calls the provider's API.
// Otherwise, or if the API call fails, it falls back to a deterministic local embedding.
func GetEmbedding(ctx context.Context, text string) ([]float32, error) {
	cfg := multiagent.DefaultLLMConfig()

	// If no API key is set, go straight to local fallback to avoid timeouts
	if cfg.APIKey == "" && cfg.Provider != planner.ProviderOllama {
		return localEmbedding(text), nil
	}

	// Set a reasonable timeout for embedding generation
	embCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	var vec []float32
	var err error

	switch cfg.Provider {
	case planner.ProviderGemini:
		vec, err = getGeminiEmbedding(embCtx, text, cfg)
	case planner.ProviderOpenAI, planner.ProviderOpenAIResponses:
		vec, err = getOpenAIEmbedding(embCtx, text, cfg)
	case planner.ProviderOllama:
		vec, err = getOllamaEmbedding(embCtx, text, cfg)
	default:
		vec = localEmbedding(text)
	}

	if err != nil {
		log.Warn("Remote embedding failed (falling back to local)", "error", err)
		return localEmbedding(text), nil
	}

	return vec, nil
}

func getGeminiEmbedding(ctx context.Context, text string, cfg multiagent.LLMConfig) ([]float32, error) {
	client, err := planner.GetGeminiClient(cfg.APIKey, cfg.BaseURL)
	if err != nil {
		return nil, err
	}

	model := config.Get().Embedding.Model
	if model == "" {
		model = "text-embedding-004"
	}

	resp, err := client.Models.EmbedContent(ctx, model, genai.Text(text), nil)
	if err != nil {
		return nil, err
	}

	if len(resp.Embeddings) == 0 || len(resp.Embeddings[0].Values) == 0 {
		return nil, fmt.Errorf("empty embedding returned from Gemini")
	}

	return resp.Embeddings[0].Values, nil
}

func getOpenAIEmbedding(ctx context.Context, text string, cfg multiagent.LLMConfig) ([]float32, error) {
	model := config.Get().Embedding.Model
	if model == "" {
		model = "text-embedding-3-small"
	}

	// OpenAI embedding endpoint is usually v1/embeddings
	url := cfg.BaseURL
	if strings.HasSuffix(url, "/chat/completions") {
		url = strings.Replace(url, "/chat/completions", "/embeddings", 1)
	} else if strings.HasSuffix(url, "/responses") {
		url = strings.Replace(url, "/responses", "/embeddings", 1)
	} else if !strings.Contains(url, "/embeddings") {
		// Guess based on base URL
		url = strings.TrimSuffix(url, "/") + "/embeddings"
	}

	reqBody := map[string]any{
		"model": model,
		"input": text,
	}

	b, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, buf.String())
	}

	var m struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, err
	}

	if len(m.Data) == 0 {
		return nil, fmt.Errorf("empty embedding returned from OpenAI")
	}

	return m.Data[0].Embedding, nil
}

func getOllamaEmbedding(ctx context.Context, text string, cfg multiagent.LLMConfig) ([]float32, error) {
	model := config.Get().Embedding.Model
	if model == "" {
		model = "nomic-embed-text"
	}

	// Ollama embedding endpoint is usually v1/embeddings or /api/embeddings
	url := cfg.BaseURL
	if strings.HasSuffix(url, "/chat") {
		url = strings.Replace(url, "/chat", "/embeddings", 1)
	} else if !strings.Contains(url, "/embeddings") {
		url = strings.TrimSuffix(url, "/") + "/embeddings"
	}

	reqBody := map[string]any{
		"model":  model,
		"prompt": text,
	}

	b, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, buf.String())
	}

	var m struct {
		Embedding []float32 `json:"embedding"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, err
	}

	return m.Embedding, nil
}

func localEmbedding(text string) []float32 {
	vec := make([]float32, 128)
	words := strings.Fields(strings.ToLower(text))
	if len(words) == 0 {
		return vec
	}
	for _, word := range words {
		word = strings.Trim(word, ".,!?;:()[]{}'\"-")
		if len(word) == 0 {
			continue
		}
		// FNV-1a hash
		h := uint32(2166136261)
		for i := 0; i < len(word); i++ {
			h ^= uint32(word[i])
			h *= 16777619
		}
		idx := h % 128
		vec[idx] += 1.0
	}
	// Normalize to unit length
	var sumSq float32
	for _, val := range vec {
		sumSq += val * val
	}
	if sumSq > 0 {
		norm := float32(1.0 / math.Sqrt(float64(sumSq)))
		for i := range vec {
			vec[i] *= norm
		}
	}
	return vec
}
