package llm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
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
	Provider         string `json:"provider"`
	Model            string `json:"model"`
	Configured       bool   `json:"configured"`
	GatewayReachable *bool  `json:"gateway_reachable,omitempty"`
	ModelAvailable   *bool  `json:"model_available,omitempty"`
	Healthy          bool   `json:"healthy"`
	Verified         bool   `json:"verified"`
	Status           string `json:"status"`
	Error            string `json:"error,omitempty"`
}

type liteLLMProbeResult struct {
	gatewayReachable bool
	models           map[string]struct{}
	err              error
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
	probed := make(map[string]liteLLMProbeResult)
	scenes := map[string]struct{}{config.LLMSceneTaskPlanner: {}}
	for scene := range cfg.LLM.Scenes {
		scenes[scene] = struct{}{}
	}
	for scene := range scenes {
		resolved := cfg.ResolveLLMScene(scene)
		health := SceneHealth{
			Provider:   resolved.Provider,
			Model:      resolved.Model,
			Configured: resolved.Provider != "" && resolved.Model != "",
			Healthy:    resolved.Provider != "" && resolved.Model != "",
			Status:     "configured_unverified",
		}
		if resolved.Provider == "litellm" {
			healthURL, err := liteLLMHealthURL(resolved.BaseURL)
			modelsURL, modelsURLErr := liteLLMModelsURL(resolved.BaseURL)
			if err == nil {
				err = modelsURLErr
			}
			if err == nil {
				probeKey := healthURL + "|" + apiKeyFingerprint(resolved.APIKey)
				probe, ok := probed[probeKey]
				if !ok {
					probe = probeLiteLLM(ctx, healthURL, modelsURL, resolved.APIKey, time.Duration(resolved.TimeoutSeconds)*time.Second)
					probed[probeKey] = probe
				}
				health.GatewayReachable = boolPtr(probe.gatewayReachable)
				_, available := probe.models[resolved.Model]
				health.ModelAvailable = boolPtr(available)
				health.Healthy = health.Configured && probe.gatewayReachable && available
				health.Verified = health.Healthy
				if health.Healthy {
					health.Status = "ready"
				}
				err = probe.err
			}
			if err != nil {
				health.Healthy = false
				health.Verified = false
				health.Status = "unhealthy"
				health.Error = err.Error()
			}
		}
		if !health.Configured {
			health.Healthy = false
			health.Verified = false
			health.Status = "unhealthy"
			health.Error = "provider and model must be configured"
		}
		if resolved.Provider == "litellm" && !health.Healthy {
			health.Status = "unhealthy"
		}
		if !health.Healthy {
			allHealthy = false
		}
		result[scene] = health
	}
	return result, allHealthy
}

func AllScenesVerified(scenes map[string]SceneHealth) bool {
	if len(scenes) == 0 {
		return false
	}
	for _, health := range scenes {
		if !health.Verified {
			return false
		}
	}
	return true
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
		fmt.Fprintf(&key, "%s|%s|%s|%s|%s;", scene, resolved.Provider, resolved.Model, resolved.BaseURL, apiKeyFingerprint(resolved.APIKey))
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
	return liteLLMRootURL(baseURL, "/health/liveliness")
}

func liteLLMModelsURL(baseURL string) (string, error) {
	return liteLLMRootURL(baseURL, "/v1/models")
}

func liteLLMRootURL(baseURL, endpoint string) (string, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid LiteLLM base URL %q", baseURL)
	}
	path := strings.TrimSuffix(parsed.Path, "/")
	if index := strings.Index(path, "/v1/"); index >= 0 {
		path = path[:index]
	}
	parsed.Path = strings.TrimSuffix(path, "/") + endpoint
	parsed.RawQuery = ""
	return parsed.String(), nil
}

func probeLiteLLM(ctx context.Context, healthURL, modelsURL, apiKey string, timeout time.Duration) liteLLMProbeResult {
	client := telemetry.NewHTTPClient(minDuration(timeout, 3*time.Second))
	return probeLiteLLMWithClient(ctx, client, healthURL, modelsURL, apiKey)
}

func probeLiteLLMWithClient(ctx context.Context, client *http.Client, healthURL, modelsURL, apiKey string) liteLLMProbeResult {
	if err := getLiteLLM(ctx, client, healthURL, apiKey, nil); err != nil {
		return liteLLMProbeResult{err: fmt.Errorf("gateway health: %w", err)}
	}
	var response struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := getLiteLLM(ctx, client, modelsURL, apiKey, &response); err != nil {
		return liteLLMProbeResult{gatewayReachable: true, err: fmt.Errorf("model listing: %w", err)}
	}
	models := make(map[string]struct{}, len(response.Data))
	for _, model := range response.Data {
		models[model.ID] = struct{}{}
	}
	return liteLLMProbeResult{gatewayReachable: true, models: models}
}

func getLiteLLM(ctx context.Context, client *http.Client, endpoint, apiKey string, output any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	if output == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(output); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func apiKeyFingerprint(apiKey string) string {
	sum := sha256.Sum256([]byte(apiKey))
	return hex.EncodeToString(sum[:8])
}

func boolPtr(value bool) *bool { return &value }

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
