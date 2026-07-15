package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/viper"
	"github.com/wuxujun/ai-agent/internal/llmprovider"
)

func resetConfig() {
	mu.Lock()
	globalConfig = nil
	mu.Unlock()
	viper.Reset()
}

func TestDefaultConfig(t *testing.T) {
	t.Setenv("TEST_NO_CONFIG", "true")
	resetConfig()
	defer resetConfig()

	// Ensure no config file is found during test by clearing path
	viper.Reset()
	setupViper()

	cfg := LoadConfig()
	if cfg == nil {
		t.Fatal("expected non-nil config")
	}

	if cfg.API.Addr != "127.0.0.1:8080" {
		t.Errorf("expected api.addr default to be 127.0.0.1:8080, got %q", cfg.API.Addr)
	}
	if cfg.Store.Type != "sqlite" {
		t.Errorf("expected store.type default to be sqlite, got %q", cfg.Store.Type)
	}
	if cfg.Store.VectorSearch != "in_process" {
		t.Errorf("expected store.vector_search default to be in_process, got %q", cfg.Store.VectorSearch)
	}
	if cfg.Store.PGVectorDimensions != 0 {
		t.Errorf("expected store.pgvector_dimensions default to be 0, got %d", cfg.Store.PGVectorDimensions)
	}
	if cfg.Orchestrator.Mode != "eino" {
		t.Errorf("expected orchestrator.mode default to be eino, got %q", cfg.Orchestrator.Mode)
	}
	if cfg.LLM.Provider != "openai-responses" {
		t.Errorf("expected llm.provider default to be openai-responses, got %q", cfg.LLM.Provider)
	}
	if !cfg.Telemetry.Enabled {
		t.Errorf("expected telemetry.enabled default to be true")
	}
	if cfg.Telemetry.Endpoint != "127.0.0.1:4318" {
		t.Errorf("expected telemetry.endpoint default to be 127.0.0.1:4318, got %q", cfg.Telemetry.Endpoint)
	}
	if cfg.Telemetry.Environment != "dev" {
		t.Errorf("expected telemetry.environment default to be dev, got %q", cfg.Telemetry.Environment)
	}
	if cfg.Telemetry.Exporter != "otlp" {
		t.Errorf("expected telemetry.exporter default to be otlp, got %q", cfg.Telemetry.Exporter)
	}
	if !cfg.Log.Console || !cfg.Log.FileEnabled || cfg.Log.Directory != "logs" || cfg.Log.RetentionDays != 30 {
		t.Errorf("unexpected log defaults: %+v", cfg.Log)
	}
}

func TestConfigEnvOverrides(t *testing.T) {
	t.Setenv("TEST_NO_CONFIG", "true")
	resetConfig()
	defer resetConfig()

	// Set environment variables
	os.Setenv("AI_AGENT_API_ADDR", "0.0.0.0:9090")
	os.Setenv("AI_AGENT_STORE_TYPE", "postgres")
	os.Setenv("AI_AGENT_STORE_DSN", "postgresql://localhost:5432/test")
	os.Setenv("AI_AGENT_STORE_VECTOR_SEARCH", "pgvector")
	os.Setenv("AI_AGENT_STORE_PGVECTOR_DIMENSIONS", "128")
	os.Setenv("AI_AGENT_TELEMETRY_ENABLED", "false")
	os.Setenv("AI_AGENT_TELEMETRY_ENDPOINT", "http://otel.test:4318")
	os.Setenv("AI_AGENT_TELEMETRY_ENVIRONMENT", "test")
	os.Setenv("AI_AGENT_TELEMETRY_EXPORTER", "stdout")
	os.Setenv("OPENAI_API_KEY", "test-openai-key")
	defer func() {
		os.Unsetenv("AI_AGENT_API_ADDR")
		os.Unsetenv("AI_AGENT_STORE_TYPE")
		os.Unsetenv("AI_AGENT_STORE_DSN")
		os.Unsetenv("AI_AGENT_STORE_VECTOR_SEARCH")
		os.Unsetenv("AI_AGENT_STORE_PGVECTOR_DIMENSIONS")
		os.Unsetenv("AI_AGENT_TELEMETRY_ENABLED")
		os.Unsetenv("AI_AGENT_TELEMETRY_ENDPOINT")
		os.Unsetenv("AI_AGENT_TELEMETRY_ENVIRONMENT")
		os.Unsetenv("AI_AGENT_TELEMETRY_EXPORTER")
		os.Unsetenv("OPENAI_API_KEY")
	}()

	setupViper()
	cfg := LoadConfig()
	if cfg == nil {
		t.Fatal("expected non-nil config")
	}

	if cfg.API.Addr != "0.0.0.0:9090" {
		t.Errorf("expected api.addr override to be 0.0.0.0:9090, got %q", cfg.API.Addr)
	}
	if cfg.Store.Type != "postgres" {
		t.Errorf("expected store.type override to be postgres, got %q", cfg.Store.Type)
	}
	if cfg.Store.DSN != "postgresql://localhost:5432/test" {
		t.Errorf("expected store.dsn override, got %q", cfg.Store.DSN)
	}
	if cfg.Store.VectorSearch != "pgvector" {
		t.Errorf("expected store.vector_search override to be pgvector, got %q", cfg.Store.VectorSearch)
	}
	if cfg.Store.PGVectorDimensions != 128 {
		t.Errorf("expected store.pgvector_dimensions override to be 128, got %d", cfg.Store.PGVectorDimensions)
	}
	if cfg.LLM.OpenAIAPIKey != "test-openai-key" {
		t.Errorf("expected llm.openai_api_key override to be test-openai-key, got %q", cfg.LLM.OpenAIAPIKey)
	}
	if cfg.Telemetry.Enabled {
		t.Errorf("expected telemetry.enabled override to be false")
	}
	if cfg.Telemetry.Endpoint != "http://otel.test:4318" {
		t.Errorf("expected telemetry.endpoint override, got %q", cfg.Telemetry.Endpoint)
	}
	if cfg.Telemetry.Environment != "test" {
		t.Errorf("expected telemetry.environment override to be test, got %q", cfg.Telemetry.Environment)
	}
	if cfg.Telemetry.Exporter != "stdout" {
		t.Errorf("expected telemetry.exporter override to be stdout, got %q", cfg.Telemetry.Exporter)
	}
}

func TestResolveLLMHelpers(t *testing.T) {
	tests := []struct {
		name             string
		setup            func(c *Config)
		provider         string
		expectedProvider string
		expectedKey      string
		expectedModel    string
		expectedBaseURL  string
	}{
		{
			name: "openai default",
			setup: func(c *Config) {
				c.LLM.Provider = "openai-responses"
				c.LLM.OpenAIAPIKey = "sk-openai"
			},
			provider:         "openai-responses",
			expectedProvider: "openai-responses",
			expectedKey:      "sk-openai",
			expectedModel:    "gpt-4.1-mini",
			expectedBaseURL:  "https://api.openai.com/v1/responses",
		},
		{
			name: "gemini custom model and url",
			setup: func(c *Config) {
				c.LLM.Provider = "gemini"
				c.LLM.GeminiAPIKey = "gemini-key"
				c.LLM.Model = "gemini-custom"
				c.LLM.BaseURL = "https://custom.gemini"
			},
			provider:         "gemini",
			expectedProvider: "gemini",
			expectedKey:      "gemini-key",
			expectedModel:    "gemini-custom",
			expectedBaseURL:  "https://custom.gemini",
		},
		{
			name: "provider auto-resolve to gemini",
			setup: func(c *Config) {
				c.LLM.Provider = "openai-responses" // default/unset
				c.LLM.GeminiAPIKey = "gemini-key"
			},
			provider:         "gemini",
			expectedProvider: "gemini",
			expectedKey:      "gemini-key",
			expectedModel:    "gemini-2.5-flash",
			expectedBaseURL:  "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{}
			tc.setup(cfg)

			resolvedProvider := cfg.ResolveLLMProvider()
			if resolvedProvider != tc.expectedProvider {
				t.Errorf("ResolveLLMProvider() = %q; want %q", resolvedProvider, tc.expectedProvider)
			}

			key := cfg.ResolveLLMAPIKey(tc.provider)
			if key != tc.expectedKey {
				t.Errorf("ResolveLLMAPIKey(%q) = %q; want %q", tc.provider, key, tc.expectedKey)
			}

			model := cfg.ResolveLLMModel(tc.provider)
			if model != tc.expectedModel {
				t.Errorf("ResolveLLMModel(%q) = %q; want %q", tc.provider, model, tc.expectedModel)
			}

			baseURL := cfg.ResolveLLMBaseURL(tc.provider)
			if baseURL != tc.expectedBaseURL {
				t.Errorf("ResolveLLMBaseURL(%q) = %q; want %q", tc.provider, baseURL, tc.expectedBaseURL)
			}
		})
	}
}

func TestResolveLLMScene(t *testing.T) {
	cfg := &Config{}
	cfg.LLM.Provider = "openai-responses"
	cfg.LLM.OpenAIAPIKey = "openai-key"
	cfg.LLM.TimeoutSeconds = 30
	cfg.LLM.Gateway = LLMEndpointConfig{
		Provider:                "litellm",
		APIKey:                  "gateway-key",
		BaseURL:                 "http://litellm:4000/v1/chat/completions",
		FallbackScene:           testPtr("gateway-fallback"),
		MaxRetries:              testPtr(3),
		MinRemainingTokens:      testPtr(2000),
		InputCostPerMillionUSD:  testPtr(2.0),
		OutputCostPerMillionUSD: testPtr(8.0),
	}
	cfg.LLM.Scenes = map[string]LLMEndpointConfig{
		LLMSceneMultiAgentWriter: {
			Model:                   "agent-writer",
			TimeoutSeconds:          60,
			FallbackScene:           testPtr(""),
			MaxRetries:              testPtr(0),
			MinRemainingTokens:      testPtr(0),
			OutputCostPerMillionUSD: testPtr(0.0),
		},
		LLMSceneEmbedding: {Model: "agent-embedding", BaseURL: "http://litellm:4000/v1/embeddings"},
	}

	writer := cfg.ResolveLLMScene(LLMSceneMultiAgentWriter)
	if writer.Provider != "litellm" || writer.Model != "agent-writer" || writer.APIKey != "gateway-key" || writer.TimeoutSeconds != 60 {
		t.Fatalf("unexpected writer scene: %+v", writer)
	}
	if writer.FallbackScene != "" || writer.MaxRetries != 0 || writer.MinRemainingTokens != 0 {
		t.Fatalf("scene did not clear gateway policy: %+v", writer)
	}
	if writer.InputCostPerMillionUSD != 2 || writer.OutputCostPerMillionUSD != 0 {
		t.Fatalf("scene did not inherit or clear cost policy: %+v", writer)
	}
	embedding := cfg.ResolveLLMScene(LLMSceneEmbedding)
	if embedding.Model != "agent-embedding" || embedding.BaseURL != "http://litellm:4000/v1/embeddings" {
		t.Fatalf("unexpected embedding scene: %+v", embedding)
	}
}

func TestOverrideForTestingUsesIsolatedSnapshot(t *testing.T) {
	original := Get()
	retryCount := 2
	restore := OverrideForTesting(func(cfg *Config) {
		cfg.API.APIKey = "override-key"
		cfg.LLM.Scenes = map[string]LLMEndpointConfig{
			"isolated": {Model: "test-model", MaxRetries: &retryCount},
		}
	})
	t.Cleanup(restore)

	overridden := Get()
	if overridden == original || overridden.API.APIKey != "override-key" {
		t.Fatalf("override snapshot = %+v", overridden)
	}
	endpoint := overridden.LLM.Scenes["isolated"]
	*endpoint.MaxRetries = 5
	if _, exists := original.LLM.Scenes["isolated"]; exists {
		t.Fatal("override mutated original scene map")
	}

	restore()
	if Get() != original {
		t.Fatal("restore did not reinstate original snapshot")
	}
}

func TestCloneConfigCopiesEndpointPolicyPointers(t *testing.T) {
	retries := 2
	inputCost := 2.0
	source := &Config{}
	source.LLM.Scenes = map[string]LLMEndpointConfig{"scene": {MaxRetries: &retries, InputCostPerMillionUSD: &inputCost}}
	cloned := cloneConfig(source)
	endpoint := cloned.LLM.Scenes["scene"]
	*endpoint.MaxRetries = 5
	*endpoint.InputCostPerMillionUSD = 7
	if *source.LLM.Scenes["scene"].MaxRetries != 2 {
		t.Fatal("clone shares endpoint policy pointer with source")
	}
	if *source.LLM.Scenes["scene"].InputCostPerMillionUSD != 2 {
		t.Fatal("clone shares endpoint cost pointer with source")
	}
}

func TestResolveLLMRoutedSceneUsesFirstMatchingRule(t *testing.T) {
	cfg := &Config{}
	cfg.LLM.Scenes = map[string]LLMEndpointConfig{
		"planner": {
			Routes: []LLMRouteRule{
				{TargetScene: "economy", MaxRemainingTokens: testPtr(1000)},
				{TargetScene: "late_steps", MinStepCount: testPtr(5)},
				{TargetScene: "quality", Intents: []string{"coding"}, Complexities: []string{"high"}, CostTiers: []string{"unconstrained"}, LatencyTiers: []string{"flexible"}, QualityTiers: []string{"quality"}},
			},
		},
		"economy":    {Model: "economy-model"},
		"late_steps": {Model: "late-model"},
		"quality":    {Model: "quality-model"},
	}

	if got := cfg.ResolveLLMRoutedScene("planner", LLMRoutingHints{HasRemainingTokens: true, RemainingTokens: 800, StepCount: 8}); got != "economy" {
		t.Fatalf("routed scene = %q, want economy", got)
	}
	if got := cfg.ResolveLLMRoutedScene("planner", LLMRoutingHints{StepCount: 8}); got != "late_steps" {
		t.Fatalf("routed scene without token budget = %q, want late_steps", got)
	}
	if got := cfg.ResolveLLMRoutedScene("planner", LLMRoutingHints{HasRemainingTokens: true, RemainingTokens: 2000, StepCount: 1}); got != "planner" {
		t.Fatalf("unmatched scene = %q, want planner", got)
	}
	if got := cfg.ResolveLLMRoutedScene("planner", LLMRoutingHints{Intent: "coding", Complexity: "high", CostTier: "unconstrained", LatencyTier: "flexible", QualityTier: "quality"}); got != "quality" {
		t.Fatalf("intent-routed scene = %q, want quality", got)
	}
}

func TestCloneConfigCopiesRoutePointers(t *testing.T) {
	source := &Config{}
	source.LLM.Scenes = map[string]LLMEndpointConfig{
		"planner": {Routes: []LLMRouteRule{{TargetScene: "economy", MaxRemainingTokens: testPtr(1000), Intents: []string{"coding"}, CostTiers: []string{"economy"}, LatencyTiers: []string{"fast"}}}},
	}
	cloned := cloneConfig(source)
	*cloned.LLM.Scenes["planner"].Routes[0].MaxRemainingTokens = 500
	if got := *source.LLM.Scenes["planner"].Routes[0].MaxRemainingTokens; got != 1000 {
		t.Fatalf("clone shares route pointer with source: %d", got)
	}
	cloned.LLM.Scenes["planner"].Routes[0].Intents[0] = "writing"
	if got := source.LLM.Scenes["planner"].Routes[0].Intents[0]; got != "coding" {
		t.Fatalf("clone shares route intent slice: %q", got)
	}
	cloned.LLM.Scenes["planner"].Routes[0].CostTiers[0] = "balanced"
	cloned.LLM.Scenes["planner"].Routes[0].LatencyTiers[0] = "balanced"
	if source.LLM.Scenes["planner"].Routes[0].CostTiers[0] != "economy" || source.LLM.Scenes["planner"].Routes[0].LatencyTiers[0] != "fast" {
		t.Fatal("clone shares cost or latency route slices")
	}
}

func TestValidateLLMRoutes(t *testing.T) {
	validBase := func() *Config {
		cfg := &Config{}
		cfg.LLM.Provider = "openai-responses"
		cfg.LLM.TimeoutSeconds = 30
		cfg.LLM.Scenes = map[string]LLMEndpointConfig{
			"planner": {Model: "planner", Routes: []LLMRouteRule{{TargetScene: "economy", MaxRemainingTokens: testPtr(1000)}}},
			"economy": {Model: "economy"},
		}
		return cfg
	}
	if err := validBase().Validate(); err != nil {
		t.Fatalf("valid routes rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{"missing target", func(cfg *Config) {
			cfg.LLM.Scenes["planner"] = LLMEndpointConfig{Model: "planner", Routes: []LLMRouteRule{{TargetScene: "missing", MaxStepCount: testPtr(2)}}}
		}},
		{"missing condition", func(cfg *Config) {
			cfg.LLM.Scenes["planner"] = LLMEndpointConfig{Model: "planner", Routes: []LLMRouteRule{{TargetScene: "economy"}}}
		}},
		{"invalid range", func(cfg *Config) {
			cfg.LLM.Scenes["planner"] = LLMEndpointConfig{Model: "planner", Routes: []LLMRouteRule{{TargetScene: "economy", MinStepCount: testPtr(3), MaxStepCount: testPtr(2)}}}
		}},
		{"cycle", func(cfg *Config) {
			cfg.LLM.Scenes["economy"] = LLMEndpointConfig{Model: "economy", Routes: []LLMRouteRule{{TargetScene: "planner", MinStepCount: testPtr(1)}}}
		}},
		{"unsupported embedding", func(cfg *Config) {
			cfg.LLM.Scenes[LLMSceneEmbedding] = LLMEndpointConfig{Model: "embedding", Routes: []LLMRouteRule{{TargetScene: "economy", MinStepCount: testPtr(1)}}}
		}},
		{"unsupported adk", func(cfg *Config) {
			cfg.LLM.Scenes[LLMSceneADK] = LLMEndpointConfig{Provider: "gemini", Model: "gemini", Routes: []LLMRouteRule{{TargetScene: "economy", MinStepCount: testPtr(1)}}}
		}},
		{"unsupported intent", func(cfg *Config) {
			cfg.LLM.Scenes["planner"] = LLMEndpointConfig{Model: "planner", Routes: []LLMRouteRule{{TargetScene: "economy", Intents: []string{"unknown"}}}}
		}},
		{"duplicate quality tier", func(cfg *Config) {
			cfg.LLM.Scenes["planner"] = LLMEndpointConfig{Model: "planner", Routes: []LLMRouteRule{{TargetScene: "economy", QualityTiers: []string{"quality", "quality"}}}}
		}},
		{"unsupported latency tier", func(cfg *Config) {
			cfg.LLM.Scenes["planner"] = LLMEndpointConfig{Model: "planner", Routes: []LLMRouteRule{{TargetScene: "economy", LatencyTiers: []string{"instant"}}}}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validBase()
			tc.mutate(cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("expected route validation error")
			}
		})
	}
}

func TestValidateLLMScenes(t *testing.T) {
	cfg := &Config{}
	cfg.LLM.Provider = "openai-responses"
	cfg.LLM.TimeoutSeconds = 30
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config invalid: %v", err)
	}
	cfg.LLM.Scenes = map[string]LLMEndpointConfig{"writer": {Provider: "litellm", Model: "writer"}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected empty LiteLLM base URL validation error")
	}
	cfg.LLM.Scenes["writer"] = LLMEndpointConfig{Provider: "litellm", Model: "writer", BaseURL: "http://litellm/v1/chat/completions"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid LiteLLM scene rejected: %v", err)
	}
	cfg.LLM.Scenes["writer"] = LLMEndpointConfig{Model: "writer", FallbackScene: testPtr("missing")}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected unknown fallback scene validation error")
	}
	cfg.LLM.Scenes = map[string]LLMEndpointConfig{
		"a": {Model: "a", FallbackScene: testPtr("b")},
		"b": {Model: "b", FallbackScene: testPtr("a")},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected fallback cycle validation error")
	}
	cfg.LLM.Scenes = nil
	cfg.LLM.Gateway.MaxRetries = testPtr(-1)
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected negative gateway retry validation error")
	}
	cfg.LLM.Gateway.MaxRetries = nil
	cfg.LLM.Gateway.InputCostPerMillionUSD = testPtr(-1.0)
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected negative gateway cost validation error")
	}
}

func TestValidateUsesProviderCapabilities(t *testing.T) {
	const providerName = "test-embedding-only-provider"
	if _, exists := llmprovider.Lookup(providerName); !exists {
		if err := llmprovider.Register(llmprovider.Specification{
			Name:            providerName,
			DefaultModel:    "embedding-model",
			DefaultBaseURL:  "https://provider.test/v1/embeddings",
			Protocol:        llmprovider.ProtocolOpenAIChat,
			Capabilities:    llmprovider.CapabilityEmbedding,
			RequiresBaseURL: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &Config{}
	cfg.LLM.Provider = llmprovider.OpenAI
	cfg.LLM.TimeoutSeconds = 30
	cfg.LLM.Scenes = map[string]LLMEndpointConfig{
		LLMSceneEmbedding: {Provider: providerName},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("embedding-capable provider rejected: %v", err)
	}
	cfg.LLM.Scenes[LLMSceneTaskFinalizer] = LLMEndpointConfig{Provider: providerName}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "does not support structured output") {
		t.Fatalf("structured scene validation error = %v", err)
	}
}

func TestValidateVisionSceneRequiresVisionCapability(t *testing.T) {
	const providerName = "test-structured-without-vision"
	if _, exists := llmprovider.Lookup(providerName); !exists {
		if err := llmprovider.Register(llmprovider.Specification{Name: providerName, DefaultModel: "text", Protocol: llmprovider.ProtocolOpenAIChat, Capabilities: llmprovider.CapabilityStructuredOutput}); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &Config{}
	cfg.LLM.Provider = llmprovider.OpenAI
	cfg.LLM.TimeoutSeconds = 30
	cfg.LLM.Scenes = map[string]LLMEndpointConfig{LLMSceneVisionAnalyzer: {Provider: providerName, Model: "text"}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "does not support vision input") {
		t.Fatalf("expected vision capability error, got %v", err)
	}
	cfg.LLM.Scenes[LLMSceneVisionAnalyzer] = LLMEndpointConfig{Provider: llmprovider.OpenAI, Model: "vision"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("vision-capable provider rejected: %v", err)
	}
	cfg.LLM.Scenes = map[string]LLMEndpointConfig{
		LLMSceneVisionAnalyzer: {Provider: llmprovider.OpenAI, Model: "vision", Routes: []LLMRouteRule{{TargetScene: "vision-economy", MinStepCount: testPtr(1)}}},
		"vision-economy":       {Provider: providerName, Model: "text"},
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "does not support vision input") {
		t.Fatalf("expected routed vision capability error, got %v", err)
	}
}

func TestValidateRejectsNegativeLLMResilienceValues(t *testing.T) {
	cfg := &Config{}
	cfg.LLM.Provider = "openai"
	cfg.LLM.TimeoutSeconds = 30
	cfg.LLM.CircuitBreakerFailureThreshold = -1
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected negative circuit breaker validation error")
	}
	cfg.LLM.CircuitBreakerFailureThreshold = 0
	cfg.LLM.RetryBudgetPerMinute = -1
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected negative retry budget validation error")
	}
	cfg.LLM.RetryBudgetPerMinute = 0
	cfg.LLM.MaxCallsPerTask = -1
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected negative task call budget validation error")
	}
}

func TestValidateLLMCostBudgetRequiresPricing(t *testing.T) {
	cfg := &Config{}
	cfg.LLM.Provider = "openai"
	cfg.LLM.TimeoutSeconds = 30
	cfg.LLM.MaxEstimatedCostUSDPerTask = 1
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "requires input or output pricing") {
		t.Fatalf("missing pricing validation error = %v", err)
	}
	cfg.LLM.Gateway.InputCostPerMillionUSD = testPtr(1.0)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("priced task cost budget rejected: %v", err)
	}
}

func TestValidateAPITenants(t *testing.T) {
	cfg := &Config{}
	cfg.LLM.Provider = "openai"
	cfg.LLM.TimeoutSeconds = 30
	cfg.API.APIKey = "admin-key"
	cfg.API.Tenants = map[string]APITenantConfig{"tenant-a": {APIKey: "tenant-key", DailyLLMCallBudget: 10}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid tenant rejected: %v", err)
	}
	cfg.API.Tenants["tenant-b"] = APITenantConfig{APIKey: "tenant-key"}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "same api_key") {
		t.Fatalf("duplicate tenant key error = %v", err)
	}
	cfg.API.Tenants["tenant-b"] = APITenantConfig{APIKey: "other-key", DailyLLMCallBudget: -1}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "budgets must be") {
		t.Fatalf("negative tenant budget error = %v", err)
	}
}

func TestValidateLLMReadinessSettings(t *testing.T) {
	cfg := &Config{}
	cfg.LLM.Provider = "openai"
	cfg.LLM.TimeoutSeconds = 30
	for _, mode := range []string{"", LLMReadinessConfigOnly, LLMReadinessGateway, LLMReadinessInference, " INFERENCE "} {
		cfg.LLM.ReadinessMode = mode
		cfg.LLM.ReadinessCacheTTLSeconds = 10
		if err := cfg.Validate(); err != nil {
			t.Fatalf("mode %q rejected: %v", mode, err)
		}
	}
	cfg.LLM.ReadinessMode = "deep"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected unsupported readiness mode validation error")
	}
	cfg.LLM.ReadinessMode = LLMReadinessGateway
	cfg.LLM.ReadinessCacheTTLSeconds = -1
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected negative readiness cache TTL validation error")
	}
}

func testPtr[T any](value T) *T { return &value }

func TestConfigFileProviderWinsOverCredentialAutoDetection(t *testing.T) {
	resetConfig()
	defer resetConfig()
	t.Setenv("OPENAI_API_KEY", "openai-test-key")
	t.Setenv("GEMINI_API_KEY", "gemini-test-key")
	viper.Reset()
	setupViper()
	viper.SetConfigFile("../../deploy/e2e/config.yaml")
	if err := viper.ReadInConfig(); err != nil {
		t.Fatal(err)
	}
	cfg, err := unmarshalConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLM.Provider != "litellm" {
		t.Fatalf("configured provider = %q, want litellm", cfg.LLM.Provider)
	}
}

func TestConfigFileLoading(t *testing.T) {
	resetConfig()
	defer resetConfig()

	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	yamlContent := []byte(`
api:
  addr: "127.0.0.1:9999"
store:
  type: "memory"
llm:
  provider: "litellm"
  model: "default-model"
  base_url: "http://litellm/v1/chat/completions"
  gateway:
    fallback_scene: "backup"
    max_retries: 3
    min_remaining_tokens: 2000
  scenes:
    writer:
      model: "writer"
      fallback_scene: ""
      max_retries: 0
      min_remaining_tokens: 0
      routes:
        - target_scene: "writer-economy"
          max_remaining_tokens: 1000
          intents: ["writing"]
          complexities: ["medium"]
          cost_tiers: ["balanced"]
          latency_tiers: ["balanced"]
          quality_tiers: ["balanced"]
    writer-economy:
      model: "writer-fast"
`)
	if err := os.WriteFile(configPath, yamlContent, 0644); err != nil {
		t.Fatal(err)
	}

	// Change dir or set viper config paths to tmpDir
	viper.Reset()
	viper.SetConfigName("config")
	viper.SetConfigType("yaml")
	viper.AddConfigPath(tmpDir)

	if err := viper.ReadInConfig(); err != nil {
		t.Fatalf("failed to read config file: %v", err)
	}

	cfg, err := unmarshalConfig()
	if err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if cfg.API.Addr != "127.0.0.1:9999" {
		t.Errorf("expected api.addr = 127.0.0.1:9999, got %q", cfg.API.Addr)
	}
	if cfg.Store.Type != "memory" {
		t.Errorf("expected store.type = memory, got %q", cfg.Store.Type)
	}
	writer := cfg.ResolveLLMScene("writer")
	if writer.FallbackScene != "" || writer.MaxRetries != 0 || writer.MinRemainingTokens != 0 {
		t.Fatalf("YAML explicit zero values did not clear gateway policy: %+v", writer)
	}
	routes := cfg.LLM.Scenes["writer"].Routes
	if len(routes) != 1 || routes[0].TargetScene != "writer-economy" || routes[0].MaxRemainingTokens == nil || *routes[0].MaxRemainingTokens != 1000 || len(routes[0].Intents) != 1 || routes[0].Intents[0] != "writing" || len(routes[0].Complexities) != 1 || routes[0].Complexities[0] != "medium" || len(routes[0].CostTiers) != 1 || routes[0].CostTiers[0] != "balanced" || len(routes[0].LatencyTiers) != 1 || routes[0].LatencyTiers[0] != "balanced" || len(routes[0].QualityTiers) != 1 || routes[0].QualityTiers[0] != "balanced" {
		t.Fatalf("YAML routes not decoded: %+v", routes)
	}
}

func TestDiffConfigs_IncludesNewFields(t *testing.T) {
	oldCfg := &Config{}
	newCfg := &Config{}

	newCfg.Embedding.Model = "new-embedding-model"
	newCfg.LLM.ContextCompressionTokenThreshold = 50000

	changes := diffConfigs(oldCfg, newCfg)
	var foundEmbeddingModel, foundTokenThreshold bool
	for _, change := range changes {
		if change == "embedding.model: \"\" → \"new-embedding-model\"" {
			foundEmbeddingModel = true
		}
		if change == "llm.context_compression_token_threshold: 0 → 50000" {
			foundTokenThreshold = true
		}
	}

	if !foundEmbeddingModel {
		t.Errorf("diffConfigs did not report change in embedding.model; changes: %v", changes)
	}
	if !foundTokenThreshold {
		t.Errorf("diffConfigs did not report change in llm.context_compression_token_threshold; changes: %v", changes)
	}
}
