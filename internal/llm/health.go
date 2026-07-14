package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/llmprovider"
	"github.com/wuxujun/ai-agent/internal/telemetry"
	"golang.org/x/sync/singleflight"
	"google.golang.org/genai"
)

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
	Provider           string `json:"provider"`
	Model              string `json:"model"`
	Configured         bool   `json:"configured"`
	GatewayReachable   *bool  `json:"gateway_reachable,omitempty"`
	ModelAvailable     *bool  `json:"model_available,omitempty"`
	InferenceReachable *bool  `json:"inference_reachable,omitempty"`
	Healthy            bool   `json:"healthy"`
	Verified           bool   `json:"verified"`
	Status             string `json:"status"`
	Error              string `json:"error,omitempty"`
}

type liteLLMProbeResult struct {
	gatewayReachable bool
	models           map[string]struct{}
	err              error
}

func CheckConfiguredScenes(ctx context.Context) (map[string]SceneHealth, bool) {
	cfg := config.Get()
	cacheKey := healthConfigKey(cfg)
	cacheTTL := time.Duration(cfg.ResolveLLMReadinessCacheTTLSeconds()) * time.Second
	if result, healthy, ok := cachedSceneHealth(cacheKey, cacheTTL); ok {
		return result, healthy
	}

	value, _, _ := healthProbeGroup.Do(cacheKey, func() (any, error) {
		// A caller may have populated the cache while this call waited to join
		// the singleflight group.
		if result, healthy, ok := cachedSceneHealth(cacheKey, cacheTTL); ok {
			return healthCheckResult{scenes: result, healthy: healthy}, nil
		}
		result, healthy := probeConfiguredScenes(ctx, cfg)
		storeSceneHealth(cacheKey, result, healthy)
		return healthCheckResult{scenes: result, healthy: healthy}, nil
	})
	checked := value.(healthCheckResult)
	return cloneSceneHealth(checked.scenes), checked.healthy
}

func cachedSceneHealth(cacheKey string, cacheTTL time.Duration) (map[string]SceneHealth, bool, bool) {
	healthCache.Lock()
	defer healthCache.Unlock()
	if time.Since(healthCache.at) < cacheTTL && healthCache.result != nil && healthCache.key == cacheKey {
		return cloneSceneHealth(healthCache.result), healthCache.healthy, true
	}
	return nil, false, false
}

func probeConfiguredScenes(ctx context.Context, cfg *config.Config) (map[string]SceneHealth, bool) {
	return probeConfiguredScenesWithProbes(ctx, cfg, probeSceneInference, probeLiteLLM)
}

type sceneInferenceProbe func(context.Context, string, config.ResolvedLLMConfig) error
type liteLLMHealthProbe func(context.Context, string, string, string, time.Duration) liteLLMProbeResult

func probeConfiguredScenesWithInference(ctx context.Context, cfg *config.Config, inferenceProbe sceneInferenceProbe) (map[string]SceneHealth, bool) {
	return probeConfiguredScenesWithProbes(ctx, cfg, inferenceProbe, probeLiteLLM)
}

func probeConfiguredScenesWithProbes(ctx context.Context, cfg *config.Config, inferenceProbe sceneInferenceProbe, gatewayProbe liteLLMHealthProbe) (map[string]SceneHealth, bool) {
	result := make(map[string]SceneHealth, len(cfg.LLM.Scenes)+1)
	allHealthy := true
	mode := cfg.ResolveLLMReadinessMode()
	probed := make(map[string]liteLLMProbeResult)
	scenes := map[string]struct{}{config.LLMSceneTaskPlanner: {}}
	for scene := range cfg.LLM.Scenes {
		scenes[scene] = struct{}{}
	}
	for scene := range scenes {
		resolved := cfg.ResolveLLMScene(scene)
		providerSpec, providerRegistered := llmprovider.Lookup(resolved.Provider)
		gatewayDiscovery := providerRegistered && providerSpec.Supports(llmprovider.CapabilityGatewayModelDiscovery)
		health := SceneHealth{
			Provider:   resolved.Provider,
			Model:      resolved.Model,
			Configured: resolved.Provider != "" && resolved.Model != "",
			Healthy:    resolved.Provider != "" && resolved.Model != "",
			Status:     "configured_unverified",
		}
		if health.Configured && mode != config.LLMReadinessConfigOnly && gatewayDiscovery {
			var probeErr error
			healthURL, err := liteLLMHealthURL(resolved.BaseURL)
			modelsURL, modelsURLErr := liteLLMModelsURL(resolved.BaseURL)
			if err != nil {
				probeErr = err
			} else if modelsURLErr != nil {
				probeErr = modelsURLErr
			} else {
				probeKey := healthURL + "|" + credentialFingerprint(resolved.APIKey)
				probe, ok := probed[probeKey]
				if !ok {
					probe = gatewayProbe(ctx, healthURL, modelsURL, resolved.APIKey, time.Duration(resolved.TimeoutSeconds)*time.Second)
					probed[probeKey] = probe
				}
				health.GatewayReachable = boolPtr(probe.gatewayReachable)
				_, available := probe.models[resolved.Model]
				health.ModelAvailable = boolPtr(available)
				probeErr = probe.err
				if probeErr == nil && !available {
					probeErr = fmt.Errorf("model is not available to the configured credential")
				}
			}
			if probeErr != nil {
				health.Healthy = false
				health.Status = "unhealthy"
				health.Error = probeErr.Error()
			} else {
				health.Status = "gateway_verified"
			}
		}
		if health.Configured && health.Healthy && mode == config.LLMReadinessInference {
			probeErr := inferenceProbe(ctx, scene, resolved)
			reachable := probeErr == nil
			health.InferenceReachable = boolPtr(reachable)
			health.Healthy = reachable
			health.Verified = reachable
			if reachable {
				health.Status = "ready"
			} else {
				health.Status = "unhealthy"
				health.Error = probeErr.Error()
			}
		}
		if !health.Configured {
			health.Healthy = false
			health.Verified = false
			health.Status = "unhealthy"
			health.Error = "provider and model must be configured"
		}
		if gatewayDiscovery && !health.Healthy {
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
	fmt.Fprintf(&key, "mode=%s|ttl=%d;", cfg.ResolveLLMReadinessMode(), cfg.ResolveLLMReadinessCacheTTLSeconds())
	for _, scene := range names {
		resolved := cfg.ResolveLLMScene(scene)
		fmt.Fprintf(&key, "%s|%s|%s|%s|%s;", scene, resolved.Provider, resolved.Model, resolved.BaseURL, credentialFingerprint(resolved.APIKey))
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

func probeSceneInference(ctx context.Context, scene string, resolved config.ResolvedLLMConfig) error {
	if scene == config.LLMSceneEmbedding {
		return probeEmbeddingInference(ctx, resolved)
	}

	var output struct {
		OK bool `json:"ok"`
	}
	schema := map[string]any{
		"type":                 "object",
		"properties":           map[string]any{"ok": map[string]any{"type": "boolean"}},
		"required":             []string{"ok"},
		"additionalProperties": false,
	}
	cfg := Config{
		Scene:    scene,
		Provider: resolved.Provider,
		APIKey:   resolved.APIKey,
		Model:    resolved.Model,
		BaseURL:  resolved.BaseURL,
		Timeout:  time.Duration(resolved.TimeoutSeconds) * time.Second,
	}
	var err error
	if scene == config.LLMSceneVisionAnalyzer {
		// A valid 1x1 GIF keeps the probe small while verifying the configured
		// model, rather than only the provider protocol, accepts image input.
		image := VisionInput{MIMEType: "image/gif", Data: []byte("GIF89a\x01\x00\x01\x00\x80\x00\x00\x00\x00\x00\xff\xff\xff!\xf9\x04\x01\x00\x00\x00\x00,\x00\x00\x00\x00\x01\x00\x01\x00\x00\x02\x02D\x01\x00;")}
		_, err = (nativeStructuredCaller{}).CallVisionJSON(ctx, cfg, "This is a readiness probe. Return JSON only.", `Return {"ok":true}.`, image, schema, &output)
	} else {
		_, err = (nativeStructuredCaller{}).CallJSON(ctx, cfg, "This is a readiness probe. Return JSON only.", `Return {"ok":true}.`, schema, &output)
	}
	if err != nil {
		return safeInferenceProbeError(err)
	}
	if !output.OK {
		return fmt.Errorf("inference probe returned an invalid acknowledgement")
	}
	return nil
}

func probeEmbeddingInference(ctx context.Context, resolved config.ResolvedLLMConfig) error {
	providerSpec, registered := llmprovider.Lookup(resolved.Provider)
	if !registered {
		return fmt.Errorf("inference probe provider is unsupported")
	}
	if providerSpec.Protocol == llmprovider.ProtocolGemini {
		client, err := GetGeminiClient(resolved.APIKey, resolved.BaseURL)
		if err != nil {
			return safeInferenceProbeError(err)
		}
		response, err := client.Models.EmbedContent(ctx, resolved.Model, genai.Text("readiness"), nil)
		if err != nil {
			return safeInferenceProbeError(err)
		}
		if len(response.Embeddings) == 0 || len(response.Embeddings[0].Values) == 0 {
			return fmt.Errorf("inference probe returned an empty embedding")
		}
		return nil
	}

	endpoint := embeddingProbeURL(providerSpec.Protocol, resolved.BaseURL)
	payload := map[string]any{"model": resolved.Model, "input": "readiness"}
	if providerSpec.Protocol == llmprovider.ProtocolOllama {
		payload = map[string]any{"model": resolved.Model, "prompt": "readiness"}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("inference probe request encoding failed")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("inference probe endpoint is invalid")
	}
	req.Header.Set("Content-Type", "application/json")
	if resolved.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+resolved.APIKey)
	}
	client := telemetry.NewHTTPClient(time.Duration(resolved.TimeoutSeconds) * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		return safeInferenceProbeError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return safeInferenceProbeError(NewHTTPStatusError(resp.StatusCode, resp.Header, nil))
	}
	var response struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
		Embedding []float32 `json:"embedding"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&response); err != nil {
		return fmt.Errorf("inference probe returned an invalid embedding response")
	}
	if len(response.Embedding) == 0 && (len(response.Data) == 0 || len(response.Data[0].Embedding) == 0) {
		return fmt.Errorf("inference probe returned an empty embedding")
	}
	return nil
}

func embeddingProbeURL(protocol llmprovider.Protocol, baseURL string) string {
	endpoint := strings.TrimSuffix(baseURL, "/")
	if protocol == llmprovider.ProtocolOllama {
		if strings.HasSuffix(endpoint, "/api/chat") {
			return strings.TrimSuffix(endpoint, "/api/chat") + "/api/embeddings"
		}
		if strings.HasSuffix(endpoint, "/chat") {
			return strings.TrimSuffix(endpoint, "/chat") + "/embeddings"
		}
	} else {
		if strings.HasSuffix(endpoint, "/chat/completions") {
			return strings.TrimSuffix(endpoint, "/chat/completions") + "/embeddings"
		}
		if strings.HasSuffix(endpoint, "/responses") {
			return strings.TrimSuffix(endpoint, "/responses") + "/embeddings"
		}
	}
	if !strings.HasSuffix(endpoint, "/embeddings") {
		endpoint += "/embeddings"
	}
	return endpoint
}

func safeInferenceProbeError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("inference probe timed out")
	}
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("inference probe canceled")
	}
	var statusErr *HTTPStatusError
	if errors.As(err, &statusErr) {
		return fmt.Errorf("inference probe returned status %d", statusErr.StatusCode)
	}
	return fmt.Errorf("inference probe failed")
}

func boolPtr(value bool) *bool { return &value }

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
