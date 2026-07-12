package llm

import (
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
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
