package config

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
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
	if cfg.API.Auth.Mode != "api_key" || cfg.API.Auth.Bearer.ValidationMode != "jwks" || cfg.API.Auth.JWT.TenantClaim != "code" || len(cfg.API.Auth.JWT.AllowedAlgorithms) != 1 || cfg.API.Auth.JWT.AllowedAlgorithms[0] != "RS256" || !cfg.API.Auth.JWT.RequireKnownTenant || cfg.API.Auth.Introspection.TenantClaim != "code" || cfg.API.Auth.Introspection.ActiveClaim != "active" || !cfg.API.Auth.Introspection.RequireKnownTenant {
		t.Errorf("unexpected API authentication defaults: %+v", cfg.API.Auth)
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
	if cfg.Store.ParadeDBCandidateMultiplier != 4 || cfg.Store.ParadeDBRRFK != 60 || cfg.Store.ParadeDBSlowQueryThresholdMS != 250 {
		t.Errorf("unexpected ParadeDB ranking defaults: %+v", cfg.Store)
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
	if !cfg.Log.Console || !cfg.Log.FileEnabled || !cfg.Log.AccessEnabled || cfg.Log.Directory != "logs" || cfg.Log.RetentionDays != 30 {
		t.Errorf("unexpected log defaults: %+v", cfg.Log)
	}
	if cfg.RAG.MaxPromptMemories != 3 || cfg.RAG.MaxMemoryBytes != 2500 || cfg.RAG.MaxMemoryPromptBytes != 8000 || cfg.RAG.MaxRawFallbackBytes != 4000 {
		t.Errorf("unexpected RAG prompt budget defaults: %+v", cfg.RAG)
	}
	if cfg.RAG.ContextMode != "jit" || cfg.RAG.JITSearchMaxCalls != 2 || cfg.RAG.JITRetrievalMaxCycles != 2 || cfg.RAG.JITFetchMaxItems != 3 || cfg.RAG.JITRAGFetchMaxBytes != 6000 || cfg.RAG.JITMemoryFetchMaxBytes != 2000 {
		t.Errorf("unexpected RAG JIT defaults: %+v", cfg.RAG)
	}
	if cfg.Wiki.URL != "" || cfg.Wiki.TimeoutSeconds != 15 || cfg.Wiki.SearchTopK != 5 || cfg.Wiki.FetchMaxItems != 3 || cfg.Wiki.FetchMaxBytes != 12000 || cfg.Wiki.AllowPrivateNetwork || cfg.Wiki.Required {
		t.Errorf("unexpected Wiki defaults: %+v", cfg.Wiki)
	}
	if cfg.LLM.PlannerTraceMaxItems != 4 || cfg.LLM.PlannerObservationMaxChars != 800 || cfg.LLM.PlannerEvidenceMaxItems != 8 || cfg.LLM.PlannerEvidenceLineMaxChars != 300 || cfg.LLM.PlannerTraceMaxChars != 5000 {
		t.Errorf("unexpected planner trace budget defaults: %+v", cfg.LLM)
	}
	if cfg.Langfuse.BootstrapMissingPrompts || cfg.Langfuse.BootstrapFailurePolicy != "fail" || cfg.Langfuse.BootstrapTimeoutSeconds != 15 {
		t.Errorf("unexpected Langfuse bootstrap defaults: %+v", cfg.Langfuse)
	}
}

func TestValidateWikiSettings(t *testing.T) {
	valid := func() *Config {
		cfg := &Config{}
		cfg.LLM.Provider = "openai-responses"
		cfg.LLM.TimeoutSeconds = 30
		return cfg
	}
	cfg := valid()
	cfg.Wiki.Required = true
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "wiki.url") {
		t.Fatalf("required Wiki without URL accepted: %v", err)
	}
	for _, mutate := range []func(*Config){
		func(cfg *Config) { cfg.Wiki.TimeoutSeconds = -1 },
		func(cfg *Config) { cfg.Wiki.SearchTopK = 11 },
		func(cfg *Config) { cfg.Wiki.FetchMaxItems = 11 },
		func(cfg *Config) { cfg.Wiki.FetchMaxBytes = -1 },
	} {
		cfg = valid()
		mutate(cfg)
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "wiki") {
			t.Fatalf("invalid Wiki settings accepted: %+v", cfg.Wiki)
		}
	}
}

func TestMultiAgentEnvironmentOverrides(t *testing.T) {
	resetConfig()
	defer resetConfig()
	t.Setenv("TEST_NO_CONFIG", "true")
	t.Setenv("AI_AGENT_MULTIAGENT_TEAM", "software")
	t.Setenv("AI_AGENT_MULTIAGENT_RUNTIME", "legacy")
	t.Setenv("AI_AGENT_MULTIAGENT_DAG_CANARY_PERCENT", "5")

	cfg := LoadConfig()
	if cfg.MultiAgent.Team != "software" || cfg.MultiAgent.Runtime != "legacy" || cfg.MultiAgent.DAGCanaryPercent != 5 {
		t.Fatalf("multiagent env overrides = %+v", cfg.MultiAgent)
	}
}

func TestValidateMultiAgentRolloutSettings(t *testing.T) {
	validConfig := func() *Config {
		cfg := &Config{}
		cfg.LLM.Provider = "openai"
		cfg.LLM.TimeoutSeconds = 30
		return cfg
	}
	for _, runtime := range []string{"", "legacy", "dag", " DAG "} {
		cfg := validConfig()
		cfg.MultiAgent.Runtime = runtime
		cfg.MultiAgent.DAGCanaryPercent = 5
		if err := cfg.Validate(); err != nil {
			t.Fatalf("multiagent runtime %q rejected: %v", runtime, err)
		}
	}
	for _, mutate := range []func(*Config){
		func(cfg *Config) { cfg.MultiAgent.Runtime = "graph" },
		func(cfg *Config) { cfg.MultiAgent.DAGCanaryPercent = -1 },
		func(cfg *Config) { cfg.MultiAgent.DAGCanaryPercent = 101 },
	} {
		cfg := validConfig()
		mutate(cfg)
		if err := cfg.Validate(); err == nil {
			t.Fatalf("invalid multiagent rollout accepted: %+v", cfg.MultiAgent)
		}
	}
}

func TestValidateOrchestratorMode(t *testing.T) {
	for _, mode := range []string{"", "eino", "legacy", "adk", "step", "multiagent", " MULTIAGENT "} {
		cfg := &Config{}
		cfg.Orchestrator.Mode = mode
		cfg.LLM.Provider = "openai-responses"
		cfg.LLM.TimeoutSeconds = 30
		if err := cfg.Validate(); err != nil {
			t.Errorf("valid orchestrator mode %q rejected: %v", mode, err)
		}
	}

	cfg := &Config{}
	cfg.Orchestrator.Mode = "not-a-mode"
	cfg.LLM.Provider = "openai-responses"
	cfg.LLM.TimeoutSeconds = 30
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "orchestrator.mode") {
		t.Fatalf("invalid orchestrator mode was not rejected: %v", err)
	}
}

func TestValidateApprovalTTL(t *testing.T) {
	cfg := *Get()
	cfg.Approval.TTLSeconds = -1
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "approval.ttl_seconds") {
		t.Fatalf("Validate() = %v, want approval TTL error", err)
	}
	cfg = *Get()
	cfg.Approval.RetentionDays = -1
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "approval.retention_days") {
		t.Fatalf("Validate() = %v, want approval retention error", err)
	}
}

func TestRevisionAdvancesOnOverride(t *testing.T) {
	resetConfig()
	defer resetConfig()
	_ = Get()
	before := Revision()
	restore := OverrideForTesting(func(cfg *Config) {
		cfg.Orchestrator.MaxConcurrentTasks++
	})
	if after := Revision(); after <= before {
		t.Fatalf("revision did not advance after override: before=%d after=%d", before, after)
	}
	restore()
}

func TestApplyReloadedConfigAdvancesRevisionOnlyForChanges(t *testing.T) {
	resetConfig()
	defer resetConfig()
	previousRevision := configRevision
	defer func() { configRevision = previousRevision }()
	globalConfig = &Config{}
	configRevision = 10

	if changes := applyReloadedConfig(&Config{}); len(changes) != 0 || configRevision != 10 {
		t.Fatalf("no-op reload changed revision or diff: revision=%d changes=%v", configRevision, changes)
	}
	changed := &Config{}
	changed.Store.ParadeDBSlowQueryThresholdMS = 250
	changes := applyReloadedConfig(changed)
	if configRevision != 11 || len(changes) == 0 {
		t.Fatalf("effective reload did not advance revision: revision=%d changes=%v", configRevision, changes)
	}
}

func TestConcreteTaskFinalizerSceneIsLoaded(t *testing.T) {
	resetConfig()
	defer resetConfig()
	setupViper()
	viper.SetConfigType("yaml")
	if err := viper.ReadConfig(strings.NewReader("llm:\n  scenes:\n    task_finalizer:\n      timeout_seconds: 30\n")); err != nil {
		t.Fatalf("read config: %v", err)
	}
	cfg, err := unmarshalConfig()
	if err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	scene, ok := cfg.LLM.Scenes[LLMSceneTaskFinalizer]
	if !ok || scene.TimeoutSeconds != 30 {
		t.Fatalf("task_finalizer scene not loaded: %+v", cfg.LLM.Scenes)
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
	os.Setenv("LANGFUSE_ENABLED", "true")
	os.Setenv("LANGFUSE_PUBLIC_KEY", "pk-langfuse")
	os.Setenv("LANGFUSE_SECRET_KEY", "sk-langfuse")
	os.Setenv("LANGFUSE_BASE_URL", "https://langfuse.test")
	os.Setenv("LANGFUSE_BOOTSTRAP_MISSING_PROMPTS", "true")
	os.Setenv("LANGFUSE_BOOTSTRAP_FAILURE_POLICY", "warn")
	os.Setenv("LANGFUSE_BOOTSTRAP_TIMEOUT_SECONDS", "25")
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
		os.Unsetenv("LANGFUSE_ENABLED")
		os.Unsetenv("LANGFUSE_PUBLIC_KEY")
		os.Unsetenv("LANGFUSE_SECRET_KEY")
		os.Unsetenv("LANGFUSE_BASE_URL")
		os.Unsetenv("LANGFUSE_BOOTSTRAP_MISSING_PROMPTS")
		os.Unsetenv("LANGFUSE_BOOTSTRAP_FAILURE_POLICY")
		os.Unsetenv("LANGFUSE_BOOTSTRAP_TIMEOUT_SECONDS")
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
	if !cfg.Langfuse.Enabled || cfg.Langfuse.PublicKey != "pk-langfuse" || cfg.Langfuse.SecretKey != "sk-langfuse" || cfg.Langfuse.Host != "https://langfuse.test" {
		t.Errorf("unexpected Langfuse env overrides: %+v", cfg.Langfuse)
	}
	if !cfg.Langfuse.BootstrapMissingPrompts || cfg.Langfuse.BootstrapFailurePolicy != "warn" || cfg.Langfuse.BootstrapTimeoutSeconds != 25 {
		t.Errorf("unexpected Langfuse bootstrap env overrides: %+v", cfg.Langfuse)
	}
}

func TestValidateLangfuseBootstrap(t *testing.T) {
	cfg := &Config{}
	cfg.LLM.Provider = "openai-responses"
	cfg.LLM.TimeoutSeconds = 30
	cfg.Langfuse.BootstrapFailurePolicy = "warn"
	cfg.Langfuse.BootstrapTimeoutSeconds = 5
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid Langfuse bootstrap config rejected: %v", err)
	}
	cfg.Langfuse.BootstrapFailurePolicy = "ignore"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "bootstrap_failure_policy") {
		t.Fatalf("expected invalid failure policy, got %v", err)
	}
	cfg.Langfuse.BootstrapFailurePolicy = "fail"
	cfg.Langfuse.BootstrapTimeoutSeconds = -1
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "bootstrap_timeout_seconds") {
		t.Fatalf("expected invalid timeout, got %v", err)
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

func TestResolveEmbeddingSceneUsesProviderEmbeddingDefault(t *testing.T) {
	tests := []struct {
		provider string
		want     string
	}{
		{provider: llmprovider.Gemini, want: "gemini-embedding-001"},
		{provider: llmprovider.OpenAI, want: "text-embedding-3-small"},
	}
	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			cfg := &Config{}
			cfg.LLM.Provider = tt.provider
			cfg.LLM.TimeoutSeconds = 30

			resolved := cfg.ResolveLLMScene(LLMSceneEmbedding)
			if resolved.Model != tt.want {
				t.Fatalf("embedding model = %q, want %q", resolved.Model, tt.want)
			}
		})
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

func TestValidateRAGContextModeAndLimits(t *testing.T) {
	cfg := &Config{}
	cfg.LLM.Provider = "openai-responses"
	cfg.LLM.TimeoutSeconds = 30
	for _, mode := range []string{"", "jit", "prefetch", " JIT "} {
		cfg.RAG.ContextMode = mode
		if err := cfg.Validate(); err != nil {
			t.Fatalf("valid context mode %q rejected: %v", mode, err)
		}
	}
	cfg.RAG.ContextMode = "automatic"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected invalid context mode error")
	}
	cfg.RAG.ContextMode = "jit"
	cfg.RAG.JITSearchMaxCalls = -1
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected negative JIT search limit error")
	}
	cfg.RAG.JITSearchMaxCalls = 1
	cfg.RAG.JITRetrievalMaxCycles = -1
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected negative JIT retrieval cycle limit error")
	}
	cfg.RAG.JITRetrievalMaxCycles = 1
	cfg.RAG.JITFetchMaxItems = -1
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected negative JIT fetch limit error")
	}
	cfg.RAG.JITFetchMaxItems = 1
	cfg.RAG.JITRAGFetchMaxBytes = -1
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected negative JIT RAG byte limit error")
	}
	cfg.RAG.JITRAGFetchMaxBytes = 1
	cfg.RAG.JITMemoryFetchMaxBytes = -1
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected negative JIT memory byte limit error")
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
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "requires input or output pricing") || !strings.Contains(err.Error(), "llm.gateway") || !strings.Contains(err.Error(), "llm_cost_budget_usd to 0") {
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
	cfg.API.Auth.RequireTenantWorkspaceRoot = true
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "workspace_root is required") {
		t.Fatalf("missing strict tenant workspace root error = %v", err)
	}
	tenantA := cfg.API.Tenants["tenant-a"]
	tenantA.WorkspaceRoot = "./workspace/tenants/tenant-a"
	cfg.API.Tenants["tenant-a"] = tenantA
	if err := cfg.Validate(); err != nil {
		t.Fatalf("tenant workspace root rejected: %v", err)
	}
	cfg.API.Auth.RequireTenantWorkspaceRoot = false
	cfg.API.Tenants["tenant-b"] = APITenantConfig{APIKey: "tenant-key"}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "same api_key") {
		t.Fatalf("duplicate tenant key error = %v", err)
	}
	cfg.API.Tenants["tenant-b"] = APITenantConfig{APIKey: "other-key", DailyLLMCallBudget: -1}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "budgets must be") {
		t.Fatalf("negative tenant budget error = %v", err)
	}
	cfg.API.Tenants["tenant-b"] = APITenantConfig{APIKey: "other-key", AnswerPipelineEnforcement: "blocking"}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "answer_pipeline_enforcement") {
		t.Fatalf("invalid tenant enforcement error = %v", err)
	}
	cfg.API.Tenants["tenant-b"] = APITenantConfig{APIKey: "other-key", AnswerPipelineRequiredStages: []string{"unknown"}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "unknown answer pipeline stage") {
		t.Fatalf("invalid tenant stage error = %v", err)
	}
}

func TestValidateJWTAuthentication(t *testing.T) {
	cfg := &Config{}
	cfg.LLM.Provider = "openai"
	cfg.LLM.TimeoutSeconds = 30
	cfg.API.Auth = APIAuthConfig{Mode: "hybrid", JWT: APIJWTConfig{
		Issuer:                    "https://issuer.example",
		Audience:                  "ai-agent",
		JWKSURL:                   "https://issuer.example/.well-known/jwks.json",
		TenantClaim:               "code",
		AllowedAlgorithms:         []string{"RS256"},
		RequireKnownTenant:        true,
		ClockSkewSeconds:          30,
		JWKSCacheTTLSeconds:       300,
		JWKSRequestTimeoutSeconds: 5,
	}}
	cfg.API.Tenants = map[string]APITenantConfig{"tenant-a": {DailyLLMCallBudget: 10}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid JWT authentication rejected: %v", err)
	}

	cfg.API.Auth.JWT.AllowedAlgorithms = []string{"none"}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "unsupported algorithm") {
		t.Fatalf("unsafe JWT algorithm error = %v", err)
	}
	cfg.API.Auth.JWT.AllowedAlgorithms = []string{"RS256"}
	cfg.API.Auth.JWT.Audience = ""
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "audience") {
		t.Fatalf("missing JWT audience error = %v", err)
	}
}

func TestValidateIntrospectionAuthentication(t *testing.T) {
	cfg := &Config{}
	cfg.LLM.Provider = "openai"
	cfg.LLM.TimeoutSeconds = 30
	cfg.API.Auth = APIAuthConfig{Mode: "introspection", Introspection: APIIntrospectionConfig{
		URL:                "https://auth.example/oauth/introspect",
		TenantClaim:        "code",
		ActiveClaim:        "active",
		RequireKnownTenant: true,
		TimeoutSeconds:     3,
		CacheTTLSeconds:    10,
	}}
	cfg.API.Tenants = map[string]APITenantConfig{"tenant-a": {DailyLLMCallBudget: 10}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid introspection authentication rejected: %v", err)
	}

	cfg.API.Auth.Introspection.URL = "http://auth.example/oauth/introspect"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "absolute https") {
		t.Fatalf("insecure introspection URL error = %v", err)
	}
	cfg.API.Auth.Introspection.URL = "https://auth.example/oauth/introspect"
	cfg.API.Auth.Introspection.TimeoutSeconds = 0
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "timeout_seconds") {
		t.Fatalf("invalid introspection timeout error = %v", err)
	}
}

func TestCloneConfigDetachesTenantPipelineStages(t *testing.T) {
	source := &Config{}
	source.API.Auth.JWT.AllowedAlgorithms = []string{"RS256"}
	source.API.Tenants = map[string]APITenantConfig{
		"tenant-a": {APIKey: "key", AnswerPipelineRequiredStages: []string{"safety_guard_output"}},
	}
	cloned := cloneConfig(source)
	tenant := cloned.API.Tenants["tenant-a"]
	tenant.AnswerPipelineRequiredStages[0] = "fact_freshness_check"
	cloned.API.Tenants["tenant-a"] = tenant
	if got := source.API.Tenants["tenant-a"].AnswerPipelineRequiredStages[0]; got != "safety_guard_output" {
		t.Fatalf("tenant pipeline stages share backing array: %q", got)
	}
	cloned.API.Auth.JWT.AllowedAlgorithms[0] = "RS512"
	if got := source.API.Auth.JWT.AllowedAlgorithms[0]; got != "RS256" {
		t.Fatalf("JWT allowed algorithms share backing array: %q", got)
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

func TestValidateStoreSearchSettings(t *testing.T) {
	validConfig := func() *Config {
		cfg := &Config{}
		cfg.LLM.Provider = "openai"
		cfg.LLM.TimeoutSeconds = 30
		return cfg
	}
	for _, mode := range []string{"", "in_process", "pgvector", "paradedb", " PARADEDB "} {
		cfg := validConfig()
		cfg.Store.VectorSearch = mode
		if err := cfg.Validate(); err != nil {
			t.Fatalf("store vector_search mode %q rejected: %v", mode, err)
		}
	}
	for _, mutate := range []func(*Config){
		func(cfg *Config) { cfg.Store.VectorSearch = "elastic" },
		func(cfg *Config) { cfg.Store.PGVectorDimensions = -1 },
		func(cfg *Config) { cfg.Store.ParadeDBCandidateMultiplier = -1 },
		func(cfg *Config) { cfg.Store.ParadeDBRRFK = -1 },
		func(cfg *Config) { cfg.Store.ParadeDBSlowQueryThresholdMS = -1 },
		func(cfg *Config) { cfg.Store.MemoryCandidateLimit = -1 },
		func(cfg *Config) { cfg.Store.MemoryDecayRate = -0.1 },
	} {
		cfg := validConfig()
		mutate(cfg)
		if err := cfg.Validate(); err == nil {
			t.Fatalf("invalid store settings accepted: %+v", cfg.Store)
		}
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
	newCfg.RAG.MaxMemoryPromptBytes = 8000
	newCfg.Log.AccessEnabled = true

	changes := diffConfigs(oldCfg, newCfg)
	var foundEmbeddingModel, foundTokenThreshold, foundMemoryBudget, foundAccessEnabled bool
	for _, change := range changes {
		if change == "embedding.model: \"\" → \"new-embedding-model\"" {
			foundEmbeddingModel = true
		}
		if change == "llm.context_compression_token_threshold: 0 → 50000" {
			foundTokenThreshold = true
		}
		if change == "rag.max_memory_prompt_bytes: 0 → 8000" {
			foundMemoryBudget = true
		}
		if change == "log.access_enabled: false → true" {
			foundAccessEnabled = true
		}
	}

	if !foundEmbeddingModel {
		t.Errorf("diffConfigs did not report change in embedding.model; changes: %v", changes)
	}
	if !foundTokenThreshold {
		t.Errorf("diffConfigs did not report change in llm.context_compression_token_threshold; changes: %v", changes)
	}
	if !foundMemoryBudget {
		t.Errorf("diffConfigs did not report change in rag.max_memory_prompt_bytes; changes: %v", changes)
	}
	if !foundAccessEnabled {
		t.Errorf("diffConfigs did not report change in log.access_enabled; changes: %v", changes)
	}
}

func TestMCPServersDecodeCloneAndValidate(t *testing.T) {
	resetConfig()
	defer resetConfig()
	setupViper()
	viper.SetConfigType("yaml")
	if err := viper.ReadConfig(strings.NewReader(`
mcp:
  servers:
    - name: github
      url: https://mcp.example.test/mcp
      authorization_env: GITHUB_MCP_AUTHORIZATION
      tool_prefix: corp_github
      risk_level: high
      timeout_seconds: 12
      required: true
`)); err != nil {
		t.Fatal(err)
	}
	cfg, err := unmarshalConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(cfg.MCP.Servers) != 1 {
		t.Fatalf("MCP servers = %+v", cfg.MCP.Servers)
	}
	server := cfg.MCP.Servers[0]
	if server.Name != "github" || server.AuthorizationEnv != "GITHUB_MCP_AUTHORIZATION" || server.ToolPrefix != "corp_github" || !server.Required || server.TimeoutSeconds != 12 {
		t.Fatalf("decoded MCP server = %+v", server)
	}

	cloned := cloneConfig(cfg)
	cloned.MCP.Servers[0].Name = "changed"
	if cfg.MCP.Servers[0].Name != "github" {
		t.Fatal("cloneConfig shared MCP server backing array")
	}
}

func TestMCPServerValidation(t *testing.T) {
	tests := []struct {
		name    string
		servers []MCPServerConfig
	}{
		{name: "missing name", servers: []MCPServerConfig{{URL: "https://example.test/mcp"}}},
		{name: "invalid name", servers: []MCPServerConfig{{Name: "bad name", URL: "https://example.test/mcp"}}},
		{name: "duplicate name", servers: []MCPServerConfig{{Name: "one", URL: "https://one.test/mcp"}, {Name: "ONE", URL: "https://two.test/mcp"}}},
		{name: "missing url", servers: []MCPServerConfig{{Name: "one"}}},
		{name: "negative timeout", servers: []MCPServerConfig{{Name: "one", URL: "https://one.test/mcp", TimeoutSeconds: -1}}},
		{name: "negative max tools", servers: []MCPServerConfig{{Name: "one", URL: "https://one.test/mcp", MaxTools: -1}}},
		{name: "invalid risk", servers: []MCPServerConfig{{Name: "one", URL: "https://one.test/mcp", RiskLevel: "medium"}}},
		{name: "invalid prefix", servers: []MCPServerConfig{{Name: "one", URL: "https://one.test/mcp", ToolPrefix: "bad prefix"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := &Config{}
			cfg.MCP.Servers = test.servers
			if err := cfg.Validate(); err == nil {
				t.Fatal("Validate succeeded, want error")
			}
		})
	}
}

func TestLocalizedConfigMatchesPrimaryConfig(t *testing.T) {
	readSettings := func(name string) map[string]any {
		t.Helper()
		content, err := os.ReadFile(filepath.Join("..", "..", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		parser := viper.New()
		parser.SetConfigType("yaml")
		if err := parser.ReadConfig(bytes.NewReader(content)); err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		return parser.AllSettings()
	}

	primary := readSettings("config.yaml")
	localized := readSettings("config_zh.yml")
	if !reflect.DeepEqual(primary, localized) {
		t.Fatal("config_zh.yml keys or values differ from config.yaml")
	}
}
