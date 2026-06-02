package config

import (
	"log"
	"strings"

	"github.com/spf13/viper"
)

type Config struct {
	Store struct {
		Type string `mapstructure:"type"`
		DSN  string `mapstructure:"dsn"`
	} `mapstructure:"store"`

	Orchestrator struct {
		Mode                string `mapstructure:"mode"`
		MaxConcurrentTasks int    `mapstructure:"max_concurrent_tasks"`
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
		SearchURL    string `mapstructure:"search_url"`
		SearchMethod string `mapstructure:"search_method"`
	} `mapstructure:"rag"`

	Log struct {
		Level string `mapstructure:"level"`
	} `mapstructure:"log"`

	Tool struct {
		TimeoutSeconds int `mapstructure:"timeout_seconds"`
	} `mapstructure:"tool"`
}

var globalConfig *Config

func LoadConfig() *Config {
	if globalConfig != nil {
		return globalConfig
	}

	viper.SetConfigName("config")
	viper.SetConfigType("yaml")
	viper.AddConfigPath(".")
	viper.AddConfigPath("../../")

	// Set Environment Variable Prefix and replace . with _
	viper.SetEnvPrefix("AI_AGENT")
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	viper.AutomaticEnv()

	// Default values
	viper.SetDefault("store.type", "sqlite")
	viper.SetDefault("store.dsn", "data/agent.db")
	viper.SetDefault("orchestrator.mode", "eino")
	viper.SetDefault("orchestrator.max_concurrent_tasks", 10)
	viper.SetDefault("llm.provider", "openai-responses")
	viper.SetDefault("llm.timeout_seconds", 30)
	viper.SetDefault("log.level", "info")
	viper.SetDefault("tool.timeout_seconds", 120)

	// Explicit bindings for standard env variables
	_ = viper.BindEnv("llm.openai_api_key", "OPENAI_API_KEY")
	_ = viper.BindEnv("llm.gemini_api_key", "GEMINI_API_KEY")
	_ = viper.BindEnv("llm.google_api_key", "GOOGLE_API_KEY")

	if err := viper.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); ok {
			log.Println("[Config] No config file found. Using default values and environment variables.")
		} else {
			log.Fatalf("[Config] Error reading config file: %v", err)
		}
	}

	var c Config
	if err := viper.Unmarshal(&c); err != nil {
		log.Fatalf("[Config] Unable to decode into struct: %v", err)
	}

	globalConfig = &c
	return globalConfig
}

func Get() *Config {
	return LoadConfig()
}

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
