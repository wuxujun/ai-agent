package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/wuxujun/ai-agent/internal/telemetry"
)

// OllamaModelList represents the response from Ollama's /api/tags endpoint.
type OllamaModelList struct {
	Models []struct {
		Name string `json:"name"`
	} `json:"models"`
}

// ResolveOllamaBaseURL resolves the base URL of the Ollama service.
// e.g. "http://localhost:11434/api/chat" -> "http://localhost:11434"
func ResolveOllamaBaseURL(rawURL string) string {
	if idx := strings.Index(rawURL, "/api"); idx != -1 {
		return rawURL[:idx]
	}
	return strings.TrimSuffix(rawURL, "/")
}

// ProbeOllama checks if the Ollama service is running and if the specified model is pulled.
func ProbeOllama(ctx context.Context, baseURL string, modelName string) error {
	if modelName == "" {
		return fmt.Errorf("probe ollama: model name is empty")
	}

	base := ResolveOllamaBaseURL(baseURL)
	healthClient := telemetry.NewHTTPClient(3 * time.Second)

	// 1. Check if Ollama service is running
	healthReq, err := http.NewRequestWithContext(ctx, "GET", base, nil)
	if err != nil {
		return fmt.Errorf("failed to create health check request: %w", err)
	}

	resp, err := healthClient.Do(healthReq)
	if err != nil {
		return fmt.Errorf("Ollama local service is not running or unreachable at %s. Please start Ollama first (error: %w)", base, err)
	}
	resp.Body.Close()

	// 2. Fetch pulled models
	tagsReq, err := http.NewRequestWithContext(ctx, "GET", base+"/api/tags", nil)
	if err != nil {
		return fmt.Errorf("failed to create tags request: %w", err)
	}

	tagsResp, err := healthClient.Do(tagsReq)
	if err != nil {
		return fmt.Errorf("failed to query Ollama models list: %w", err)
	}
	defer tagsResp.Body.Close()

	if tagsResp.StatusCode >= 300 {
		return fmt.Errorf("Ollama returned status %d when listing models", tagsResp.StatusCode)
	}

	var list OllamaModelList
	if err := json.NewDecoder(tagsResp.Body).Decode(&list); err != nil {
		return fmt.Errorf("failed to decode Ollama models list: %w", err)
	}

	// 3. Verify if modelName is pulled
	targetLower := strings.ToLower(modelName)
	isPulled := false
	var pulledNames []string

	for _, m := range list.Models {
		pulledNames = append(pulledNames, m.Name)
		mLower := strings.ToLower(m.Name)
		if mLower == targetLower {
			isPulled = true
			break
		}
		// Match target model names without tags (defaults to :latest)
		if !strings.Contains(targetLower, ":") && mLower == targetLower+":latest" {
			isPulled = true
			break
		}
		// Match target model names with :latest tag when returned model lacks tag
		if strings.HasSuffix(targetLower, ":latest") && mLower == strings.TrimSuffix(targetLower, ":latest") {
			isPulled = true
			break
		}
	}

	if !isPulled {
		return fmt.Errorf("model %q has not been pulled in Ollama. Available models: %v. Please run \"ollama pull %s\" first",
			modelName, pulledNames, modelName)
	}

	return nil
}
