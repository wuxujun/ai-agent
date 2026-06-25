package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/viper"
)

func resetConfig() {
	mu.Lock()
	globalConfig = nil
	mu.Unlock()
	viper.Reset()
}

func TestDefaultConfig(t *testing.T) {
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
	if cfg.Orchestrator.Mode != "eino" {
		t.Errorf("expected orchestrator.mode default to be eino, got %q", cfg.Orchestrator.Mode)
	}
	if cfg.LLM.Provider != "openai-responses" {
		t.Errorf("expected llm.provider default to be openai-responses, got %q", cfg.LLM.Provider)
	}
}

func TestConfigEnvOverrides(t *testing.T) {
	resetConfig()
	defer resetConfig()

	// Set environment variables
	os.Setenv("AI_AGENT_API_ADDR", "0.0.0.0:9090")
	os.Setenv("AI_AGENT_STORE_TYPE", "postgres")
	os.Setenv("AI_AGENT_STORE_DSN", "postgresql://localhost:5432/test")
	os.Setenv("OPENAI_API_KEY", "test-openai-key")
	defer func() {
		os.Unsetenv("AI_AGENT_API_ADDR")
		os.Unsetenv("AI_AGENT_STORE_TYPE")
		os.Unsetenv("AI_AGENT_STORE_DSN")
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
	if cfg.LLM.OpenAIAPIKey != "test-openai-key" {
		t.Errorf("expected llm.openai_api_key override to be test-openai-key, got %q", cfg.LLM.OpenAIAPIKey)
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
}
