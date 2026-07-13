package llm

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/telemetry"
	"golang.org/x/sync/singleflight"
)

const healthCacheTTL = 10 * time.Second

var healthCache struct {
	sync.Mutex
	at      time.Time
	result  map[string]SceneHealth
	healthy bool
	key     string
}

var healthProbeGroup singleflight.Group

type healthCheckResult struct {
	scenes  map[string]SceneHealth
	healthy bool
}

type SceneHealth struct {
	Provider string `json:"provider"`
	Healthy  bool   `json:"healthy"`
	Error    string `json:"error,omitempty"`
}

func CheckConfiguredScenes(ctx context.Context) (map[string]SceneHealth, bool) {
	cfg := config.Get()
	cacheKey := healthConfigKey(cfg)
	if result, healthy, ok := cachedSceneHealth(cacheKey); ok {
		return result, healthy
	}

	value, _, _ := healthProbeGroup.Do(cacheKey, func() (any, error) {
		// A caller may have populated the cache while this call waited to join
		// the singleflight group.
		if result, healthy, ok := cachedSceneHealth(cacheKey); ok {
			return healthCheckResult{scenes: result, healthy: healthy}, nil
		}
		result, healthy := probeConfiguredScenes(ctx, cfg)
		storeSceneHealth(cacheKey, result, healthy)
		return healthCheckResult{scenes: result, healthy: healthy}, nil
	})
	checked := value.(healthCheckResult)
	return cloneSceneHealth(checked.scenes), checked.healthy
}

func cachedSceneHealth(cacheKey string) (map[string]SceneHealth, bool, bool) {
	healthCache.Lock()
	defer healthCache.Unlock()
	if time.Since(healthCache.at) < healthCacheTTL && healthCache.result != nil && healthCache.key == cacheKey {
		return cloneSceneHealth(healthCache.result), healthCache.healthy, true
	}
	return nil, false, false
}

func probeConfiguredScenes(ctx context.Context, cfg *config.Config) (map[string]SceneHealth, bool) {
	result := make(map[string]SceneHealth, len(cfg.LLM.Scenes)+1)
	allHealthy := true
	probed := make(map[string]SceneHealth)
	scenes := map[string]struct{}{config.LLMSceneTaskPlanner: {}}
	for scene := range cfg.LLM.Scenes {
		scenes[scene] = struct{}{}
	}
	for scene := range scenes {
		resolved := cfg.ResolveLLMScene(scene)
		health := SceneHealth{Provider: resolved.Provider, Healthy: true}
		if resolved.Provider == "litellm" {
			healthURL, err := liteLLMHealthURL(resolved.BaseURL)
			if cached, ok := probed[healthURL]; ok && err == nil {
				health = cached
				result[scene] = health
				if !health.Healthy {
					allHealthy = false
				}
				continue
			}
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
			probed[healthURL] = health
		}
		result[scene] = health
	}
	return result, allHealthy
}

func storeSceneHealth(cacheKey string, result map[string]SceneHealth, healthy bool) {
	healthCache.Lock()
	defer healthCache.Unlock()
	healthCache.at = time.Now()
	healthCache.result = cloneSceneHealth(result)
	healthCache.healthy = healthy
	healthCache.key = cacheKey
}

func healthConfigKey(cfg *config.Config) string {
	names := make([]string, 0, len(cfg.LLM.Scenes)+1)
	names = append(names, config.LLMSceneTaskPlanner)
	for scene := range cfg.LLM.Scenes {
		if scene != config.LLMSceneTaskPlanner {
			names = append(names, scene)
		}
	}
	sort.Strings(names)
	var key strings.Builder
	for _, scene := range names {
		resolved := cfg.ResolveLLMScene(scene)
		fmt.Fprintf(&key, "%s|%s|%s;", scene, resolved.Provider, resolved.BaseURL)
	}
	return key.String()
}

func cloneSceneHealth(source map[string]SceneHealth) map[string]SceneHealth {
	result := make(map[string]SceneHealth, len(source))
	for scene, health := range source {
		result[scene] = health
	}
	return result
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
	parsed.Path = strings.TrimSuffix(path, "/") + "/health/liveliness"
	parsed.RawQuery = ""
	return parsed.String(), nil
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
