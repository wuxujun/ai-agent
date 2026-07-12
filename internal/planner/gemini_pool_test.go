package planner

import "testing"

func TestGeminiProviderIsRegistered(t *testing.T) {
	p, err := lookupProvider(ProviderGemini)
	if err != nil || p.Name() != ProviderGemini {
		t.Fatalf("gemini provider lookup: provider=%v err=%v", p, err)
	}
}

func TestLiteLLMProviderIsRegistered(t *testing.T) {
	p, err := lookupProvider(ProviderLiteLLM)
	if err != nil || p.Name() != ProviderLiteLLM {
		t.Fatalf("litellm provider lookup: provider=%v err=%v", p, err)
	}
}
