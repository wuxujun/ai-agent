package config

import (
	"fmt"
	"os"
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
		// MemoryCandidateLimit caps how many recent memory rows each Store
		// backend loads from disk before in-process cosine/keyword ranking.
		// Without a cap, large memory tables would scan the full table on
		// every RAG prefetch; with the default of 200, only the most recent
		// 200 rows are ever considered. Raise this if recall on older memories
		// matters more than scan latency. 0 falls back to the package default
		// (200).
		MemoryCandidateLimit int `mapstructure:"memory_candidate_limit"`
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
		Provider       string `mapstructure:"provider"`
		APIKey         string `mapstructure:"api_key"`
		OpenAIAPIKey   string `mapstructure:"openai_api_key"`
		GeminiAPIKey   string `mapstructure:"gemini_api_key"`
		GoogleAPIKey   string `mapstructure:"google_api_key"`
		Model          string `mapstructure:"model"`
		BaseURL        string `mapstructure:"base_url"`
		TimeoutSeconds int    `mapstructure:"timeout_seconds"`
	} `mapstructure:"llm"`

	Embedding struct {
		Model string `mapstructure:"model"`
	} `mapstructure:"embedding"`

	RAG struct {
		SearchURL     string `mapstructure:"search_url"`
		SearchMethod  string `mapstructure:"search_method"`
		Authorization string `mapstructure:"authorization"`
	} `mapstructure:"rag"`

	Log struct {
		Level string `mapstructure:"level"`
	} `mapstructure:"log"`

	Tool struct {
		TimeoutSeconds int `mapstructure:"timeout_seconds"`
	} `mapstructure:"tool"`

	Skill struct {
		// Root is the directory scanned for <name>/SKILL.md skill packages,
		// relative to the server's working directory unless absolute.
		Root string `mapstructure:"root"`
	} `mapstructure:"skill"`
}

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
	viper.SetDefault("store.memory_candidate_limit", 200)
	viper.SetDefault("orchestrator.mode", "eino")
	viper.SetDefault("orchestrator.max_concurrent_tasks", 10)
	viper.SetDefault("orchestrator.run_all_timeout_seconds", 600)
	viper.SetDefault("llm.provider", "openai-responses")
	viper.SetDefault("llm.timeout_seconds", 30)
	viper.SetDefault("log.level", "info")
	viper.SetDefault("tool.timeout_seconds", 120)
	viper.SetDefault("skill.root", "skills")
	viper.SetDefault("rag.authorization", "")

	// Explicit bindings for standard env variables
	_ = viper.BindEnv("api.addr", "AI_AGENT_API_ADDR")
	_ = viper.BindEnv("api.api_key", "AI_AGENT_API_KEY")
	_ = viper.BindEnv("llm.openai_api_key", "OPENAI_API_KEY")
	_ = viper.BindEnv("llm.gemini_api_key", "GEMINI_API_KEY")
	_ = viper.BindEnv("llm.google_api_key", "GOOGLE_API_KEY")
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

	// Store (DSN may contain a password)
	addIf("store.type", old.Store.Type, new.Store.Type)
	addIf("store.dsn", old.Store.DSN, new.Store.DSN)
	addIfInt("store.memory_candidate_limit", old.Store.MemoryCandidateLimit, new.Store.MemoryCandidateLimit)

	// Orchestrator
	addIf("orchestrator.mode", old.Orchestrator.Mode, new.Orchestrator.Mode)
	addIfInt("orchestrator.max_concurrent_tasks", old.Orchestrator.MaxConcurrentTasks, new.Orchestrator.MaxConcurrentTasks)
	addIfInt("orchestrator.run_all_timeout_seconds", old.Orchestrator.RunAllTimeoutSeconds, new.Orchestrator.RunAllTimeoutSeconds)

	// RAG
	addIf("rag.search_url", old.RAG.SearchURL, new.RAG.SearchURL)
	addIf("rag.search_method", old.RAG.SearchMethod, new.RAG.SearchMethod)
	addIf("rag.authorization", old.RAG.Authorization, new.RAG.Authorization)

	// Tool / Log / Skill
	addIfInt("tool.timeout_seconds", old.Tool.TimeoutSeconds, new.Tool.TimeoutSeconds)
	addIf("log.level", old.Log.Level, new.Log.Level)
	addIf("skill.root", old.Skill.Root, new.Skill.Root)

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
