package llm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/llmprovider"
)

func TestLiteLLMHealthURL(t *testing.T) {
	got, err := liteLLMHealthURL("http://litellm:4000/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	if got != "http://litellm:4000/health/liveliness" {
		t.Fatalf("health URL = %q", got)
	}
}

func TestLiteLLMModelsURL(t *testing.T) {
	got, err := liteLLMModelsURL("http://litellm:4000/v1/embeddings")
	if err != nil {
		t.Fatal(err)
	}
	if got != "http://litellm:4000/v1/models" {
		t.Fatalf("models URL = %q", got)
	}
}

func TestProbeLiteLLMListsAvailableModels(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			return testHTTPResponse(http.StatusUnauthorized, "secret response must not escape"), nil
		}
		switch r.URL.Path {
		case "/health/liveliness":
			return testHTTPResponse(http.StatusOK, ""), nil
		case "/v1/models":
			return testHTTPResponse(http.StatusOK, `{"data":[{"id":"agent-writer"},{"id":"agent-planner"}]}`), nil
		default:
			return testHTTPResponse(http.StatusNotFound, ""), nil
		}
	})}

	probe := probeLiteLLMWithClient(context.Background(), client, "http://litellm/health/liveliness", "http://litellm/v1/models", "test-key")
	if probe.err != nil {
		t.Fatal(probe.err)
	}
	if !probe.gatewayReachable {
		t.Fatal("gateway should be reachable")
	}
	if _, ok := probe.models["agent-writer"]; !ok {
		t.Fatalf("models = %#v, want agent-writer", probe.models)
	}
}

func TestProbeLiteLLMDoesNotExposeErrorBody(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return testHTTPResponse(http.StatusUnauthorized, "secret response must not escape"), nil
	})}

	probe := probeLiteLLMWithClient(context.Background(), client, "http://litellm/health", "http://litellm/models", "bad-key")
	if probe.err == nil || probe.err.Error() != "gateway health: status 401" {
		t.Fatalf("error = %v", probe.err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return fn(req) }

func testHTTPResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

func TestHealthConfigKeyChangesWithSceneEndpoint(t *testing.T) {
	first := &config.Config{}
	first.LLM.Provider = "openai"
	first.LLM.Scenes = map[string]config.LLMEndpointConfig{"writer": {Provider: "litellm", Model: "writer", BaseURL: "http://one/v1/chat/completions"}}
	second := *first
	second.LLM.Scenes = map[string]config.LLMEndpointConfig{"writer": {Provider: "litellm", Model: "writer", BaseURL: "http://two/v1/chat/completions"}}
	if healthConfigKey(first) == healthConfigKey(&second) {
		t.Fatal("health cache key did not change with scene endpoint")
	}
}

func TestHealthConfigKeyChangesWithModelOrCredential(t *testing.T) {
	first := &config.Config{}
	first.LLM.Provider = "litellm"
	first.LLM.Model = "writer-v1"
	first.LLM.BaseURL = "http://litellm/v1/chat/completions"
	first.LLM.APIKey = "key-one"

	changedModel := *first
	changedModel.LLM.Model = "writer-v2"
	if healthConfigKey(first) == healthConfigKey(&changedModel) {
		t.Fatal("health cache key did not change with model")
	}

	changedCredential := *first
	changedCredential.LLM.APIKey = "key-two"
	if healthConfigKey(first) == healthConfigKey(&changedCredential) {
		t.Fatal("health cache key did not change with credential")
	}
	if strings.Contains(healthConfigKey(first), "key-one") {
		t.Fatal("health cache key contains plaintext credential")
	}
}

func TestHealthConfigKeyChangesWithReadinessPolicy(t *testing.T) {
	first := &config.Config{}
	first.LLM.Provider = "openai"
	first.LLM.Model = "gpt-test"
	first.LLM.ReadinessMode = config.LLMReadinessGateway
	first.LLM.ReadinessCacheTTLSeconds = 10
	second := *first
	second.LLM.ReadinessMode = config.LLMReadinessInference
	if healthConfigKey(first) == healthConfigKey(&second) {
		t.Fatal("health cache key did not change with readiness mode")
	}
	second = *first
	second.LLM.ReadinessCacheTTLSeconds = 60
	if healthConfigKey(first) == healthConfigKey(&second) {
		t.Fatal("health cache key did not change with readiness cache TTL")
	}
}

func TestDirectProviderIsConfiguredButUnverified(t *testing.T) {
	cfg := &config.Config{}
	cfg.LLM.Provider = "openai"
	cfg.LLM.Model = "gpt-test"
	cfg.LLM.BaseURL = "https://api.test/v1/chat/completions"
	scenes, healthy := probeConfiguredScenes(context.Background(), cfg)
	if !healthy {
		t.Fatal("configured direct provider should remain ready")
	}
	health := scenes[config.LLMSceneTaskPlanner]
	if !health.Configured || !health.Healthy || health.Verified || health.Status != "configured_unverified" {
		t.Fatalf("health = %+v", health)
	}
	if AllScenesVerified(scenes) {
		t.Fatal("direct provider must not be reported as verified")
	}
}

func TestConfigOnlyReadinessDoesNotProbeLiteLLM(t *testing.T) {
	cfg := &config.Config{}
	cfg.LLM.Provider = "litellm"
	cfg.LLM.Model = "agent-planner"
	cfg.LLM.BaseURL = "://invalid-and-must-not-be-used"
	cfg.LLM.ReadinessMode = config.LLMReadinessConfigOnly
	scenes, healthy := probeConfiguredScenesWithInference(context.Background(), cfg, func(context.Context, string, config.ResolvedLLMConfig) error {
		t.Fatal("config_only readiness called inference probe")
		return nil
	})
	if !healthy {
		t.Fatal("configured LiteLLM scene should pass config_only readiness")
	}
	health := scenes[config.LLMSceneTaskPlanner]
	if health.Verified || health.Status != "configured_unverified" || health.GatewayReachable != nil || health.InferenceReachable != nil {
		t.Fatalf("health = %+v", health)
	}
}

func TestGatewayReadinessDoesNotClaimInferenceVerification(t *testing.T) {
	cfg := &config.Config{}
	cfg.LLM.Provider = "litellm"
	cfg.LLM.Model = "agent-planner"
	cfg.LLM.BaseURL = "http://litellm/v1/chat/completions"
	cfg.LLM.TimeoutSeconds = 3
	cfg.LLM.ReadinessMode = config.LLMReadinessGateway
	scenes, healthy := probeConfiguredScenesWithProbes(context.Background(), cfg, probeSceneInference, func(context.Context, string, string, string, time.Duration) liteLLMProbeResult {
		return liteLLMProbeResult{gatewayReachable: true, models: map[string]struct{}{"agent-planner": {}}}
	})
	if !healthy {
		t.Fatalf("gateway readiness should pass: %+v", scenes)
	}
	health := scenes[config.LLMSceneTaskPlanner]
	if health.Verified || health.Status != "gateway_verified" || health.GatewayReachable == nil || !*health.GatewayReachable || health.ModelAvailable == nil || !*health.ModelAvailable {
		t.Fatalf("health = %+v", health)
	}
}

func TestInferenceReadinessRequiresSuccessfulCall(t *testing.T) {
	cfg := &config.Config{}
	cfg.LLM.Provider = "openai"
	cfg.LLM.Model = "gpt-test"
	cfg.LLM.BaseURL = "https://api.test/v1/chat/completions"
	cfg.LLM.ReadinessMode = config.LLMReadinessInference
	calls := 0
	scenes, healthy := probeConfiguredScenesWithInference(context.Background(), cfg, func(_ context.Context, scene string, resolved config.ResolvedLLMConfig) error {
		calls++
		if scene != config.LLMSceneTaskPlanner || resolved.Model != "gpt-test" {
			t.Fatalf("probe scene=%q config=%+v", scene, resolved)
		}
		return nil
	})
	if !healthy || calls != 1 {
		t.Fatalf("healthy=%t calls=%d scenes=%+v", healthy, calls, scenes)
	}
	health := scenes[config.LLMSceneTaskPlanner]
	if !health.Verified || health.Status != "ready" || health.InferenceReachable == nil || !*health.InferenceReachable {
		t.Fatalf("health = %+v", health)
	}

	scenes, healthy = probeConfiguredScenesWithInference(context.Background(), cfg, func(context.Context, string, config.ResolvedLLMConfig) error {
		return errors.New("probe rejected")
	})
	if healthy {
		t.Fatal("failed inference probe was reported healthy")
	}
	health = scenes[config.LLMSceneTaskPlanner]
	if health.Verified || health.Status != "unhealthy" || health.InferenceReachable == nil || *health.InferenceReachable {
		t.Fatalf("failed health = %+v", health)
	}
}

func TestSafeInferenceProbeErrorOmitsProviderDetail(t *testing.T) {
	err := safeInferenceProbeError(errors.New("upstream echoed secret-token"))
	if err.Error() != "inference probe failed" || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("unsafe readiness error: %q", err)
	}
}

func TestEmbeddingProbeURL(t *testing.T) {
	cases := map[string]string{
		embeddingProbeURL(llmprovider.ProtocolOpenAIChat, "https://api.test/v1/chat/completions"): "https://api.test/v1/embeddings",
		embeddingProbeURL(llmprovider.ProtocolOpenAIResponses, "https://api.test/v1/responses"):   "https://api.test/v1/embeddings",
		embeddingProbeURL(llmprovider.ProtocolOllama, "http://localhost:11434/api/chat"):          "http://localhost:11434/api/embeddings",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("embedding URL = %q, want %q", got, want)
		}
	}
}

func TestAllScenesVerified(t *testing.T) {
	scenes := map[string]SceneHealth{
		"planner": {Healthy: true, Verified: true, Status: "ready"},
		"writer":  {Healthy: true, Verified: true, Status: "ready"},
	}
	if !AllScenesVerified(scenes) {
		t.Fatal("all scenes should be verified")
	}
	scenes["writer"] = SceneHealth{Healthy: true, Status: "configured_unverified"}
	if AllScenesVerified(scenes) {
		t.Fatal("unverified scene was ignored")
	}
}
