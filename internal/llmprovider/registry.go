package llmprovider

import (
	"fmt"
	"sort"
	"sync"
)

const (
	OpenAIResponses = "openai-responses"
	OpenAI          = "openai"
	Gemini          = "gemini"
	Ollama          = "ollama"
	LiteLLM         = "litellm"
)

type Protocol string

const (
	ProtocolOpenAIResponses Protocol = "openai-responses"
	ProtocolOpenAIChat      Protocol = "openai-chat"
	ProtocolGemini          Protocol = "gemini"
	ProtocolOllama          Protocol = "ollama"
)

type Capability uint64

const (
	CapabilityStructuredOutput Capability = 1 << iota
	CapabilityStreaming
	CapabilityEmbedding
	CapabilityToolCalling
	CapabilityVision
	CapabilityGatewayModelDiscovery
)

type CredentialFamily string

const (
	CredentialGeneric CredentialFamily = "generic"
	CredentialOpenAI  CredentialFamily = "openai"
	CredentialGemini  CredentialFamily = "gemini"
)

type Specification struct {
	Name                  string
	DefaultModel          string
	DefaultEmbeddingModel string
	DefaultBaseURL        string
	Protocol              Protocol
	CredentialFamily      CredentialFamily
	Capabilities          Capability
	RequiresAPIKey        bool
	RequiresBaseURL       bool
}

func (s Specification) Supports(capability Capability) bool {
	return s.Capabilities&capability != 0
}

var registry = struct {
	sync.RWMutex
	providers map[string]Specification
}{providers: make(map[string]Specification)}

func init() {
	MustRegister(Specification{Name: OpenAIResponses, DefaultModel: "gpt-4.1-mini", DefaultEmbeddingModel: "text-embedding-3-small", DefaultBaseURL: "https://api.openai.com/v1/responses", Protocol: ProtocolOpenAIResponses, CredentialFamily: CredentialOpenAI, Capabilities: CapabilityStructuredOutput | CapabilityStreaming | CapabilityEmbedding | CapabilityVision, RequiresAPIKey: true, RequiresBaseURL: true})
	MustRegister(Specification{Name: OpenAI, DefaultModel: "gpt-4.1-mini", DefaultEmbeddingModel: "text-embedding-3-small", DefaultBaseURL: "https://api.openai.com/v1/chat/completions", Protocol: ProtocolOpenAIChat, CredentialFamily: CredentialOpenAI, Capabilities: CapabilityStructuredOutput | CapabilityStreaming | CapabilityEmbedding | CapabilityVision, RequiresAPIKey: true, RequiresBaseURL: true})
	MustRegister(Specification{Name: Gemini, DefaultModel: "gemini-2.5-flash", DefaultEmbeddingModel: "gemini-embedding-001", Protocol: ProtocolGemini, CredentialFamily: CredentialGemini, Capabilities: CapabilityStructuredOutput | CapabilityStreaming | CapabilityEmbedding | CapabilityVision, RequiresAPIKey: true})
	MustRegister(Specification{Name: Ollama, DefaultModel: "llama3", DefaultEmbeddingModel: "nomic-embed-text", DefaultBaseURL: "http://localhost:11434/api/chat", Protocol: ProtocolOllama, CredentialFamily: CredentialGeneric, Capabilities: CapabilityStructuredOutput | CapabilityStreaming | CapabilityEmbedding | CapabilityVision, RequiresBaseURL: true})
	MustRegister(Specification{Name: LiteLLM, DefaultModel: "gpt-4.1-mini", DefaultEmbeddingModel: "text-embedding-3-small", Protocol: ProtocolOpenAIChat, CredentialFamily: CredentialGeneric, Capabilities: CapabilityStructuredOutput | CapabilityStreaming | CapabilityEmbedding | CapabilityVision | CapabilityGatewayModelDiscovery, RequiresBaseURL: true})
}

func Register(spec Specification) error {
	if spec.Name == "" {
		return fmt.Errorf("LLM provider name is required")
	}
	if spec.Protocol == "" {
		return fmt.Errorf("LLM provider %q protocol is required", spec.Name)
	}
	if spec.CredentialFamily == "" {
		spec.CredentialFamily = CredentialGeneric
	}
	registry.Lock()
	defer registry.Unlock()
	if _, exists := registry.providers[spec.Name]; exists {
		return fmt.Errorf("LLM provider %q is already registered", spec.Name)
	}
	registry.providers[spec.Name] = spec
	return nil
}

func MustRegister(spec Specification) {
	if err := Register(spec); err != nil {
		panic(err)
	}
}

func Lookup(name string) (Specification, bool) {
	registry.RLock()
	defer registry.RUnlock()
	spec, ok := registry.providers[name]
	return spec, ok
}

func List() []Specification {
	registry.RLock()
	result := make([]Specification, 0, len(registry.providers))
	for _, spec := range registry.providers {
		result = append(result, spec)
	}
	registry.RUnlock()
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}
