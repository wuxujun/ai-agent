package planner

import (
	"context"
	"testing"

	"github.com/wuxujun/ai-agent/internal/llmprovider"
	"github.com/wuxujun/ai-agent/internal/types"
)

type registeredTestProvider struct{ name ProviderType }

func (p *registeredTestProvider) Name() ProviderType { return p.name }
func (p *registeredTestProvider) Plan(context.Context, PlanRequest, func(string)) (string, types.TokenUsage, error) {
	return `{}`, types.TokenUsage{}, nil
}

func TestRegisterProviderWithSpec(t *testing.T) {
	const name = ProviderType("test-registered-provider")
	provider := &registeredTestProvider{name: name}
	if _, exists := llmprovider.Lookup(string(name)); !exists {
		err := RegisterProviderWithSpec(provider, llmprovider.Specification{
			Name:            string(name),
			DefaultModel:    "test-model",
			DefaultBaseURL:  "https://provider.test/v1/chat/completions",
			Protocol:        llmprovider.ProtocolOpenAIChat,
			Capabilities:    llmprovider.CapabilityStructuredOutput | llmprovider.CapabilityEmbedding,
			RequiresAPIKey:  true,
			RequiresBaseURL: true,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := lookupProvider(name)
	if err != nil || got.Name() != name {
		t.Fatalf("provider=%v err=%v", got, err)
	}
	spec, ok := llmprovider.Lookup(string(name))
	if !ok || spec.Protocol != llmprovider.ProtocolOpenAIChat || !spec.Supports(llmprovider.CapabilityEmbedding) {
		t.Fatalf("specification=%+v found=%t", spec, ok)
	}
}
