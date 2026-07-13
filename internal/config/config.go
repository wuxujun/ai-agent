package config

import (
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"

	"github.com/fsnotify/fsnotify"
	"github.com/spf13/viper"
	"github.com/wuxujun/ai-agent/internal/logger"
)

// Config holds all application configuration loaded from the config file and
// environment variables. Fields are grouped by subsystem.
type Config struct {
	API struct {
		Addr   string `mapstructure:"addr"`
		APIKey string `mapstructure:"api_key"`
	} `mapstructure:"api"`

	Store struct {
		Type string `mapstructure:"type"`
		DSN  string `mapstructure:"dsn"`
		// VectorSearch controls how Store.QueryMemories ranks embeddings.
		// "in_process" keeps the existing JSON load + Go cosine ranking path.
		// "pgvector" enables PostgreSQL pgvector ranking when store.type is
		// postgres. Other backends ignore this setting.
		VectorSearch string `mapstructure:"vector_search"`
		// PGVectorDimensions optionally enables a pgvector HNSW expression
		// index for embeddings with this exact dimension. 0 keeps pgvector in
		// exact-scan mode, which is still useful for avoiding JSON
		// deserialization in Go but does not provide ANN indexing.
		PGVectorDimensions int `mapstructure:"pgvector_dimensions"`
		// MemoryCandidateLimit caps how many recent memory rows each Store
		// backend loads from disk before in-process cosine/keyword ranking.
		// Without a cap, large memory tables would scan the full table on
		// every RAG prefetch; with the default of 200, only the most recent
		// 200 rows are ever considered. Raise this if recall on older memories
		// matters more than scan latency. 0 falls back to the package default
		// (200).
		MemoryCandidateLimit int `mapstructure:"memory_candidate_limit"`
		// MemoryDecayRate is the rate at which memories decay over time.
		// A decay rate of 0.0 disables time decay. A positive rate (e.g. 0.01 per hour)
		// will reduce the score of older memories exponentially.
		MemoryDecayRate float64 `mapstructure:"memory_decay_rate"`
	} `mapstructure:"store"`

	Orchestrator struct {
		Mode               string `mapstructure:"mode"`
		MaxConcurrentTasks int    `mapstructure:"max_concurrent_tasks"`
		// RunAllTimeoutSeconds caps the wall-clock budget of a single
		// background run-all goroutine (the one launched by POST /api/tasks/:id/run-all).
		// 0 falls back to the package default (600s). Long multiagent tasks may
		// need this raised; strict SLA deployments can lower it.
		RunAllTimeoutSeconds int `mapstructure:"run_all_timeout_seconds"`
	} `mapstructure:"orchestrator"`

	LLM struct {
		Provider                         string                       `mapstructure:"provider"`
		APIKey                           string                       `mapstructure:"api_key"`
		OpenAIAPIKey                     string                       `mapstructure:"openai_api_key"`
		GeminiAPIKey                     string                       `mapstructure:"gemini_api_key"`
		GoogleAPIKey                     string                       `mapstructure:"google_api_key"`
		Model                            string                       `mapstructure:"model"`
		BaseURL                          string                       `mapstructure:"base_url"`
		TimeoutSeconds                   int                          `mapstructure:"timeout_seconds"`
		ContextCompressionTraceThreshold int                          `mapstructure:"context_compression_trace_threshold"`
		// ContextCompressionTokenThreshold triggers compression when the sum of
		// all TotalTokens across task.Trace exceeds this value, regardless of
		// step count. 0 disables the token-based trigger.
		ContextCompressionTokenThreshold int                          `mapstructure:"context_compression_token_threshold"`
		Gateway                          LLMEndpointConfig            `mapstructure:"gateway"`
		Scenes                           map[string]LLMEndpointConfig `mapstructure:"scenes"`
	} `mapstructure:"llm"`

	Embedding struct {
		Model string `mapstructure:"model"`
	} `mapstructure:"embedding"`

	RAG struct {
		SearchURL     string `mapstructure:"search_url"`
		SearchMethod  string `mapstructure:"search_method"`
		Authorization string `mapstructure:"authorization"`
		ToolName      string `mapstructure:"tool_name"`
	} `mapstructure:"rag"`

	Search struct {
		URL    string `mapstructure:"url"`
		APIKey string `mapstructure:"api_key"`
	} `mapstructure:"search"`

	Log struct {
		Level string `mapstructure:"level"`
	} `mapstructure:"log"`

	Telemetry struct {
		Enabled     bool   `mapstructure:"enabled"`
		Endpoint    string `mapstructure:"endpoint"`
		Environment string `mapstructure:"environment"`
		Exporter    string `mapstructure:"exporter"`
	} `mapstructure:"telemetry"`

	Tool struct {
		TimeoutSeconds int `mapstructure:"timeout_seconds"`
	} `mapstructure:"tool"`

	Skill struct {
		// Root is the directory scanned for <name>/SKILL.md skill packages,
		// relative to the server's working directory unless absolute.
		Root string `mapstructure:"root"`
	} `mapstructure:"skill"`
}

// LLMEndpointConfig is a provider/model profile used by a specific LLM scene.
// Empty fields inherit from llm.gateway and then from the legacy llm settings.
type LLMEndpointConfig struct {
	Provider           string `mapstructure:"provider"`
	APIKey             string `mapstructure:"api_key"`
	Model              string `mapstructure:"model"`
	BaseURL            string `mapstructure:"base_url"`
	TimeoutSeconds     int    `mapstructure:"timeout_seconds"`
	FallbackScene      string `mapstructure:"fallback_scene"`
	MaxRetries         int    `mapstructure:"max_retries"`
	MinRemainingTokens int    `mapstructure:"min_remaining_tokens"`
}

type ResolvedLLMConfig struct {
	Provider           string
	APIKey             string
	Model              string
	BaseURL            string
	TimeoutSeconds     int
	FallbackScene      string
	MaxRetries         int
	MinRemainingTokens int
}

// ResolveLLMProviderConfig returns provider-specific defaults without carrying
// a model or URL explicitly selected for another provider.
func (c *Config) ResolveLLMProviderConfig(provider string) ResolvedLLMConfig {
	timeout := c.LLM.TimeoutSeconds
	if timeout <= 0 {
		timeout = 30
	}
	return ResolvedLLMConfig{
		Provider:       provider,
		APIKey:         c.ResolveLLMAPIKey(provider),
		Model:          defaultLLMModel(provider),
		BaseURL:        defaultLLMBaseURL(provider),
		TimeoutSeconds: timeout,
	}
}

const (
	LLMSceneTaskPlanner         = "task_planner"
	LLMSceneTaskFinalizer       = "task_finalizer"
	LLMSceneContextCompressor   = "context_compressor"
	LLMSceneAnswerVerifier      = "answer_verifier"
	LLMSceneMemorySummarizer    = "memory_summarizer"
	LLMSceneRAGQueryRewriter    = "rag_query_rewriter"
	LLMSceneRAGReranker         = "rag_reranker"
	LLMSceneMultiAgentPlanner   = "multiagent_planner"
	LLMSceneMultiAgentReplanner = "multiagent_replanner"
	LLMSceneMultiAgentWriter    = "multiagent_writer"
	LLMSceneEmbedding           = "embedding"
	LLMSceneADK                 = "adk"
)

// mu guards globalConfig for concurrent reads/writes.
// Hot path: RLock for Get(); Cold path: Lock for Reload().
var (
	mu           sync.RWMutex
	globalConfig *Config
)

// setupViper registers file paths, env-var bindings, and default values on the
// package-level viper instance. Idempotent and safe to call multiple times.
func setupViper() {
	if os.Getenv("TEST_NO_CONFIG") == "true" {
		viper.SetConfigName("non_existent_config_for_testing")
	} else {
		viper.SetConfigName("config")
	}
	viper.SetConfigType("yaml")
	viper.AddConfigPath(".")
	viper.AddConfigPath("../../")

	// Set Environment Variable Prefix and replace . with _
	viper.SetEnvPrefix("AI_AGENT")
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	viper.AutomaticEnv()

	// Default values
	viper.SetDefault("api.addr", "127.0.0.1:8080")
	viper.SetDefault("api.api_key", "")
	viper.SetDefault("store.type", "sqlite")
	viper.SetDefault("store.dsn", "data/agent.db")
	viper.SetDefault("store.vector_search", "in_process")
	viper.SetDefault("store.pgvector_dimensions", 0)
	viper.SetDefault("store.memory_candidate_limit", 200)
	viper.SetDefault("store.memory_decay_rate", 0.0)
	viper.SetDefault("orchestrator.mode", "eino")
	viper.SetDefault("orchestrator.max_concurrent_tasks", 10)
	viper.SetDefault("orchestrator.run_all_timeout_seconds", 600)
	viper.SetDefault("llm.provider", "openai-responses")
	viper.SetDefault("llm.timeout_seconds", 30)
	viper.SetDefault("llm.context_compression_trace_threshold", 8)
	// context_compression_token_threshold: 0 = disabled by default
	viper.SetDefault("llm.context_compression_token_threshold", 0)
	viper.SetDefault("log.level", "info")
	viper.SetDefault("telemetry.enabled", true)
	viper.SetDefault("telemetry.endpoint", "127.0.0.1:4318")
	viper.SetDefault("telemetry.environment", "dev")
	viper.SetDefault("telemetry.exporter", "otlp")
	viper.SetDefault("tool.timeout_seconds", 120)
	viper.SetDefault("skill.root", "skills")
	viper.SetDefault("rag.authorization", "")
	viper.SetDefault("rag.tool_name", "search")
	viper.SetDefault("search.url", "https://api.firecrawl.dev/v1/search")
	viper.SetDefault("search.api_key", "")

	// Explicit bindings for standard env variables
	_ = viper.BindEnv("api.addr", "AI_AGENT_API_ADDR")
	_ = viper.BindEnv("api.api_key", "AI_AGENT_API_KEY")
	_ = viper.BindEnv("llm.openai_api_key", "OPENAI_API_KEY")
	_ = viper.BindEnv("llm.gemini_api_key", "GEMINI_API_KEY")
	_ = viper.BindEnv("llm.google_api_key", "GOOGLE_API_KEY")
	_ = viper.BindEnv("rag.tool_name", "AI_AGENT_RAG_TOOL_NAME")
	_ = viper.BindEnv("rag.authorization", "AI_AGENT_RAG_AUTHORIZATION")
	_ = viper.BindEnv("search.url", "AI_AGENT_SEARCH_URL")
	_ = viper.BindEnv("search.api_key", "FIRECRAWL_API_KEY")
}

// unmarshalConfig reads the current viper state into a fresh Config struct.
// Returns an error if unmarshalling fails; does NOT update globalConfig.
func unmarshalConfig() (*Config, error) {
	var c Config
	if err := viper.Unmarshal(&c); err != nil {
		return nil, fmt.Errorf("config unmarshal failed: %w", err)
	}
	if c.API.APIKey == "" {
		if envKey := os.Getenv("AI_AGENT_API_KEY"); envKey != "" {
			c.API.APIKey = envKey
		}
	}
	if c.API.Addr == "" {
		if envAddr := os.Getenv("AI_AGENT_API_ADDR"); envAddr != "" {
			c.API.Addr = envAddr
		}
	}
	if c.Search.APIKey == "" {
		if envKey := os.Getenv("AI_AGENT_SEARCH_API_KEY"); envKey != "" {
			c.Search.APIKey = envKey
		}
	}
	if c.RAG.Authorization == "" {
		if envAuth := os.Getenv("AI_AGENT_RAG_AUTHORIZATION"); envAuth != "" {
			c.RAG.Authorization = envAuth
		}
	}
	return &c, nil
}

// LoadConfig loads the configuration once and caches it. Subsequent calls
// return the cached copy. Use Reload() to force a refresh.
func LoadConfig() *Config {
	// Fast path: already initialised.
	mu.RLock()
	if globalConfig != nil {
		defer mu.RUnlock()
		return globalConfig
	}
	mu.RUnlock()

	// Slow path: first load.
	mu.Lock()
	defer mu.Unlock()

	// Double-check after acquiring write lock.
	if globalConfig != nil {
		return globalConfig
	}

	setupViper()

	if err := viper.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); ok {
			logger.Warn("no config file found, using defaults and environment variables")
		} else {
			logger.Error("error reading config file", "error", err)
			panic(fmt.Sprintf("fatal config error: %v", err))
		}
	}

	c, err := unmarshalConfig()
	if err != nil {
		logger.Error("config unmarshal failed", "error", err)
		panic(fmt.Sprintf("fatal config error: %v", err))
	}
	if err := c.Validate(); err != nil {
		panic(fmt.Sprintf("fatal config validation error: %v", err))
	}

	globalConfig = c
	return globalConfig
}

// Get returns the current (possibly hot-updated) configuration snapshot.
// Safe for concurrent use; callers should NOT cache the returned pointer across
// calls — always call Get() at the point of use to pick up live updates.
func Get() *Config {
	return LoadConfig()
}

// Reload re-reads the configuration file and environment variables, atomically
// replaces the global config, and prints a redacted diff of what changed.
//
// This is the hot-reload path triggered by SIGHUP or the /api/config/reload
// endpoint. On error the existing configuration is preserved unchanged.
func Reload() (*Config, []string, error) {
	mu.Lock()
	defer mu.Unlock()

	// Re-read the config file (picks up any on-disk changes).
	if err := viper.ReadInConfig(); err != nil {
		if _, notFound := err.(viper.ConfigFileNotFoundError); !notFound {
			return nil, nil, fmt.Errorf("config reload: read file failed: %w", err)
		}
		// No config file is not fatal; env-vars still apply.
	}

	newCfg, err := unmarshalConfig()
	if err != nil {
		return nil, nil, fmt.Errorf("config reload: %w", err)
	}
	if err := newCfg.Validate(); err != nil {
		return nil, nil, fmt.Errorf("config reload: validation failed: %w", err)
	}

	// Build a human-readable, redacted diff before swapping.
	changes := diffConfigs(globalConfig, newCfg)
	globalConfig = newCfg

	if len(changes) == 0 {
		logger.Info("reload complete, no changes detected")
	} else {
		logger.Info("reload complete", "change_count", len(changes))
		for _, c := range changes {
			logger.Info("config change", "diff", c)
		}
	}
	return globalConfig, changes, nil
}

// Watch registers a viper OnConfigChange hook so that the config is
// automatically hot-reloaded whenever the config file is modified on disk.
// It also starts viper's filesystem watcher goroutine.
//
// Call Watch() once from main() after the first LoadConfig(). Requires that a
// config file was found (viper.ConfigFileUsed() != "").
func Watch() {
	if viper.ConfigFileUsed() == "" {
		logger.Warn("Watch: no config file in use; filesystem watch not started")
		return
	}

	viper.OnConfigChange(func(e fsnotify.Event) {
		logger.Info("config file changed, triggering hot reload")
		if _, changes, err := Reload(); err != nil {
			logger.Error("hot reload failed, keeping previous config", "error", err)
		} else if len(changes) > 0 {
			logger.Info("hot reload applied changes", "change_count", len(changes))
		}
	})
	viper.WatchConfig()
	logger.Info("watching config file for changes", "file", viper.ConfigFileUsed())
}

// ── Diff helper ───────────────────────────────────────────────────────────────

// diffConfigs returns a slice of human-readable change descriptions comparing
// old to new. API Keys are redacted to "***" so they never appear in logs.
func diffConfigs(old, new *Config) []string {
	if old == nil {
		return nil
	}
	var changes []string

	addIf := func(field, o, n string) {
		if o != n {
			// Redact fields whose names suggest they contain secrets.
			if looksLikeSecret(field) {
				o = redact(o)
				n = redact(n)
			}
			changes = append(changes, fmt.Sprintf("%s: %q → %q", field, o, n))
		}
	}
	addIfInt := func(field string, o, n int) {
		if o != n {
			changes = append(changes, fmt.Sprintf("%s: %d → %d", field, o, n))
		}
	}

	// API
	addIf("api.addr", old.API.Addr, new.API.Addr)
	addIf("api.api_key", old.API.APIKey, new.API.APIKey)

	// LLM
	addIf("llm.provider", old.LLM.Provider, new.LLM.Provider)
	addIf("llm.api_key", old.LLM.APIKey, new.LLM.APIKey)
	addIf("llm.openai_api_key", old.LLM.OpenAIAPIKey, new.LLM.OpenAIAPIKey)
	addIf("llm.gemini_api_key", old.LLM.GeminiAPIKey, new.LLM.GeminiAPIKey)
	addIf("llm.google_api_key", old.LLM.GoogleAPIKey, new.LLM.GoogleAPIKey)
	addIf("llm.model", old.LLM.Model, new.LLM.Model)
	addIf("llm.base_url", old.LLM.BaseURL, new.LLM.BaseURL)
	addIfInt("llm.timeout_seconds", old.LLM.TimeoutSeconds, new.LLM.TimeoutSeconds)
	addIfInt("llm.context_compression_trace_threshold", old.LLM.ContextCompressionTraceThreshold, new.LLM.ContextCompressionTraceThreshold)
	addIfInt("llm.context_compression_token_threshold", old.LLM.ContextCompressionTokenThreshold, new.LLM.ContextCompressionTokenThreshold)
	addIf("llm.gateway.provider", old.LLM.Gateway.Provider, new.LLM.Gateway.Provider)
	addIf("llm.gateway.api_key", old.LLM.Gateway.APIKey, new.LLM.Gateway.APIKey)
	addIf("llm.gateway.model", old.LLM.Gateway.Model, new.LLM.Gateway.Model)
	addIf("llm.gateway.base_url", old.LLM.Gateway.BaseURL, new.LLM.Gateway.BaseURL)
	addIfInt("llm.gateway.timeout_seconds", old.LLM.Gateway.TimeoutSeconds, new.LLM.Gateway.TimeoutSeconds)
	if !reflect.DeepEqual(old.LLM.Scenes, new.LLM.Scenes) {
		changes = append(changes, "llm.scenes: changed")
	}

	// Store (DSN may contain a password)
	addIf("store.type", old.Store.Type, new.Store.Type)
	addIf("store.dsn", old.Store.DSN, new.Store.DSN)
	addIf("store.vector_search", old.Store.VectorSearch, new.Store.VectorSearch)
	addIfInt("store.pgvector_dimensions", old.Store.PGVectorDimensions, new.Store.PGVectorDimensions)
	addIfInt("store.memory_candidate_limit", old.Store.MemoryCandidateLimit, new.Store.MemoryCandidateLimit)
	if old.Store.MemoryDecayRate != new.Store.MemoryDecayRate {
		changes = append(changes, fmt.Sprintf("store.memory_decay_rate: %g → %g", old.Store.MemoryDecayRate, new.Store.MemoryDecayRate))
	}

	// Orchestrator
	addIf("orchestrator.mode", old.Orchestrator.Mode, new.Orchestrator.Mode)
	addIfInt("orchestrator.max_concurrent_tasks", old.Orchestrator.MaxConcurrentTasks, new.Orchestrator.MaxConcurrentTasks)
	addIfInt("orchestrator.run_all_timeout_seconds", old.Orchestrator.RunAllTimeoutSeconds, new.Orchestrator.RunAllTimeoutSeconds)

	// RAG
	addIf("rag.search_url", old.RAG.SearchURL, new.RAG.SearchURL)
	addIf("rag.search_method", old.RAG.SearchMethod, new.RAG.SearchMethod)
	addIf("rag.authorization", old.RAG.Authorization, new.RAG.Authorization)
	addIf("rag.tool_name", old.RAG.ToolName, new.RAG.ToolName)

	// Search
	addIf("search.url", old.Search.URL, new.Search.URL)
	addIf("search.api_key", old.Search.APIKey, new.Search.APIKey)

	// Embedding
	addIf("embedding.model", old.Embedding.Model, new.Embedding.Model)

	// Tool / Log / Skill / Telemetry
	addIfInt("tool.timeout_seconds", old.Tool.TimeoutSeconds, new.Tool.TimeoutSeconds)
	addIf("log.level", old.Log.Level, new.Log.Level)
	addIf("skill.root", old.Skill.Root, new.Skill.Root)
	if old.Telemetry.Enabled != new.Telemetry.Enabled {
		changes = append(changes, fmt.Sprintf("telemetry.enabled: %t → %t", old.Telemetry.Enabled, new.Telemetry.Enabled))
	}
	addIf("telemetry.endpoint", old.Telemetry.Endpoint, new.Telemetry.Endpoint)
	addIf("telemetry.environment", old.Telemetry.Environment, new.Telemetry.Environment)
	addIf("telemetry.exporter", old.Telemetry.Exporter, new.Telemetry.Exporter)

	return changes
}

func looksLikeSecret(field string) bool {
	lower := strings.ToLower(field)
	return strings.Contains(lower, "key") ||
		strings.Contains(lower, "secret") ||
		strings.Contains(lower, "password") ||
		strings.Contains(lower, "dsn") ||
		strings.Contains(lower, "token") ||
		strings.Contains(lower, "auth") ||
		strings.Contains(lower, "authorization")
}

func redact(v string) string {
	if v == "" {
		return ""
	}
	return "***"
}

// ── Helper methods ────────────────────────────────────────────────────────────

// Helper methods to resolve dynamic fallback logic for API Keys and Providers
func (c *Config) ResolveLLMProvider() string {
	provider := c.LLM.Provider
	if provider == "" || provider == "openai-responses" { // if not explicitly overridden by env
		if c.LLM.OpenAIAPIKey != "" {
			return "openai-responses"
		}
		if c.LLM.GeminiAPIKey != "" || c.LLM.GoogleAPIKey != "" {
			return "gemini"
		}
	}
	return provider
}

func (c *Config) ResolveLLMAPIKey(provider string) string {
	switch provider {
	case "openai", "openai-responses":
		if c.LLM.OpenAIAPIKey != "" {
			return c.LLM.OpenAIAPIKey
		}
		return c.LLM.APIKey
	case "gemini":
		if c.LLM.GeminiAPIKey != "" {
			return c.LLM.GeminiAPIKey
		}
		if c.LLM.GoogleAPIKey != "" {
			return c.LLM.GoogleAPIKey
		}
		return c.LLM.APIKey
	default:
		return c.LLM.APIKey
	}
}

func (c *Config) ResolveLLMModel(provider string) string {
	if c.LLM.Model != "" {
		return c.LLM.Model
	}
	return defaultLLMModel(provider)
}

func defaultLLMModel(provider string) string {
	switch provider {
	case "openai-responses":
		return "gpt-4.1-mini"
	case "openai":
		return "gpt-4.1-mini"
	case "gemini":
		return "gemini-2.5-flash"
	case "ollama":
		return "llama3"
	default:
		return "gpt-4.1-mini"
	}
}

func (c *Config) ResolveLLMBaseURL(provider string) string {
	if c.LLM.BaseURL != "" {
		return c.LLM.BaseURL
	}
	return defaultLLMBaseURL(provider)
}

func defaultLLMBaseURL(provider string) string {
	switch provider {
	case "openai-responses":
		return "https://api.openai.com/v1/responses"
	case "openai":
		return "https://api.openai.com/v1/chat/completions"
	case "ollama":
		return "http://localhost:11434/api/chat"
	default:
		return ""
	}
}

// ResolveLLMScene resolves a call-site profile while preserving compatibility
// with the legacy flat llm configuration.
func (c *Config) ResolveLLMScene(scene string) ResolvedLLMConfig {
	provider := c.ResolveLLMProvider()
	apiKey := c.ResolveLLMAPIKey(provider)
	model := c.ResolveLLMModel(provider)
	baseURL := c.ResolveLLMBaseURL(provider)
	timeout := c.LLM.TimeoutSeconds
	fallbackScene := ""
	maxRetries := 0
	minRemainingTokens := 0

	apply := func(v LLMEndpointConfig) {
		providerChanged := v.Provider != "" && v.Provider != provider
		if v.Provider != "" {
			provider = v.Provider
		}
		if v.APIKey != "" {
			apiKey = v.APIKey
		} else if providerChanged {
			apiKey = c.ResolveLLMAPIKey(provider)
		}
		if v.Model != "" {
			model = v.Model
		} else if providerChanged {
			model = defaultLLMModel(provider)
		}
		if v.BaseURL != "" {
			baseURL = v.BaseURL
		} else if providerChanged {
			baseURL = defaultLLMBaseURL(provider)
		}
		if v.TimeoutSeconds > 0 {
			timeout = v.TimeoutSeconds
		}
		if v.FallbackScene != "" {
			fallbackScene = v.FallbackScene
		}
		if v.MaxRetries > 0 {
			maxRetries = v.MaxRetries
		}
		if v.MinRemainingTokens > 0 {
			minRemainingTokens = v.MinRemainingTokens
		}
	}

	apply(c.LLM.Gateway)
	if v, ok := c.LLM.Scenes[scene]; ok {
		apply(v)
	}
	if scene == LLMSceneEmbedding && c.Embedding.Model != "" {
		model = c.Embedding.Model
	}
	if timeout <= 0 {
		timeout = 30
	}
	return ResolvedLLMConfig{Provider: provider, APIKey: apiKey, Model: model, BaseURL: baseURL, TimeoutSeconds: timeout, FallbackScene: fallbackScene, MaxRetries: maxRetries, MinRemainingTokens: minRemainingTokens}
}

// Validate rejects configuration that would otherwise fail only on the first
// LLM request. API keys are intentionally not required here because Ollama and
// LiteLLM may run without authentication.
func (c *Config) Validate() error {
	if c.LLM.ContextCompressionTraceThreshold < 0 {
		return fmt.Errorf("llm.context_compression_trace_threshold must be >= 0")
	}
	if c.LLM.ContextCompressionTokenThreshold < 0 {
		return fmt.Errorf("llm.context_compression_token_threshold must be >= 0")
	}
	validProvider := func(provider string) bool {
		switch provider {
		case "openai-responses", "openai", "gemini", "ollama", "litellm":
			return true
		default:
			return false
		}
	}
	check := func(scene string) error {
		resolved := c.ResolveLLMScene(scene)
		if !validProvider(resolved.Provider) {
			return fmt.Errorf("llm scene %q has unsupported provider %q", scene, resolved.Provider)
		}
		if strings.TrimSpace(resolved.Model) == "" {
			return fmt.Errorf("llm scene %q has empty model", scene)
		}
		if resolved.Provider == "litellm" && strings.TrimSpace(resolved.BaseURL) == "" {
			return fmt.Errorf("llm scene %q uses litellm but has empty base_url", scene)
		}
		if resolved.TimeoutSeconds <= 0 {
			return fmt.Errorf("llm scene %q timeout_seconds must be > 0", scene)
		}
		return nil
	}
	if err := check(LLMSceneTaskPlanner); err != nil {
		return err
	}
	for scene := range c.LLM.Scenes {
		raw := c.LLM.Scenes[scene]
		if raw.MaxRetries < 0 || raw.MinRemainingTokens < 0 {
			return fmt.Errorf("llm scene %q retry and token policy values must be >= 0", scene)
		}
		if err := check(scene); err != nil {
			return err
		}
	}
	for scene := range c.LLM.Scenes {
		seen := map[string]bool{scene: true}
		current := scene
		for {
			fallback := c.ResolveLLMScene(current).FallbackScene
			if fallback == "" {
				break
			}
			if _, ok := c.LLM.Scenes[fallback]; !ok {
				return fmt.Errorf("llm scene %q references unknown fallback_scene %q", current, fallback)
			}
			if seen[fallback] {
				return fmt.Errorf("llm fallback cycle detected at scene %q", fallback)
			}
			seen[fallback] = true
			current = fallback
		}
	}
	if _, ok := c.LLM.Scenes[LLMSceneADK]; ok {
		if provider := c.ResolveLLMScene(LLMSceneADK).Provider; provider != "gemini" {
			return fmt.Errorf("llm scene %q only supports gemini provider, got %q", LLMSceneADK, provider)
		}
	}
	return nil
}
