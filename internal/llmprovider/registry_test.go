package llmprovider

import "testing"

func TestBuiltinProviderCapabilities(t *testing.T) {
	for _, name := range []string{OpenAIResponses, OpenAI, Gemini, Ollama, LiteLLM} {
		spec, ok := Lookup(name)
		if !ok || !spec.Supports(CapabilityStructuredOutput) || spec.DefaultModel == "" {
			t.Fatalf("provider %q specification = %+v, found=%t", name, spec, ok)
		}
	}
	liteLLM, _ := Lookup(LiteLLM)
	if !liteLLM.Supports(CapabilityGatewayModelDiscovery) || liteLLM.Protocol != ProtocolOpenAIChat {
		t.Fatalf("LiteLLM specification = %+v", liteLLM)
	}
}

func TestListIsSorted(t *testing.T) {
	providers := List()
	for i := 1; i < len(providers); i++ {
		if providers[i-1].Name > providers[i].Name {
			t.Fatalf("providers are not sorted: %+v", providers)
		}
	}
}
