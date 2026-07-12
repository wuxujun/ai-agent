package llm

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/telemetry"
)

type SceneHealth struct {
	Provider string `json:"provider"`
	Healthy  bool   `json:"healthy"`
	Error    string `json:"error,omitempty"`
}

func CheckConfiguredScenes(ctx context.Context) (map[string]SceneHealth, bool) {
	cfg := config.Get()
	result := make(map[string]SceneHealth, len(cfg.LLM.Scenes)+1)
	allHealthy := true
	scenes := map[string]struct{}{config.LLMSceneTaskPlanner: {}}
	for scene := range cfg.LLM.Scenes {
		scenes[scene] = struct{}{}
	}
	for scene := range scenes {
		resolved := cfg.ResolveLLMScene(scene)
		health := SceneHealth{Provider: resolved.Provider, Healthy: true}
		if resolved.Provider == "litellm" {
			healthURL, err := liteLLMHealthURL(resolved.BaseURL)
			if err == nil {
				req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
				if reqErr == nil {
					if resolved.APIKey != "" {
						req.Header.Set("Authorization", "Bearer "+resolved.APIKey)
					}
					client := telemetry.NewHTTPClient(minDuration(time.Duration(resolved.TimeoutSeconds)*time.Second, 3*time.Second))
					resp, callErr := client.Do(req)
					if callErr == nil {
						_ = resp.Body.Close()
						if resp.StatusCode >= 300 {
							callErr = fmt.Errorf("health status %d", resp.StatusCode)
						}
					}
					err = callErr
				} else {
					err = reqErr
				}
			}
			if err != nil {
				health.Healthy = false
				health.Error = err.Error()
				allHealthy = false
			}
		}
		result[scene] = health
	}
	return result, allHealthy
}

func liteLLMHealthURL(baseURL string) (string, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid LiteLLM base URL %q", baseURL)
	}
	path := strings.TrimSuffix(parsed.Path, "/")
	if index := strings.Index(path, "/v1/"); index >= 0 {
		path = path[:index]
	}
	parsed.Path = strings.TrimSuffix(path, "/") + "/health"
	parsed.RawQuery = ""
	return parsed.String(), nil
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
