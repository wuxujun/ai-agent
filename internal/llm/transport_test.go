package llm

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/wuxujun/ai-agent/internal/llmprovider"
)

func TestExtractStructuredResponses(t *testing.T) {
	text, usage, err := extractStructuredResponse("responses", []byte(`{"output":[{"content":[{"text":"{\"answer\":\"ok\"}"}]}],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`))
	if err != nil || text != `{"answer":"ok"}` || usage.TotalTokens != 15 {
		t.Fatalf("text=%q usage=%+v err=%v", text, usage, err)
	}
}

func TestVisionStructuredRequestUsesProtocolSpecificImageShape(t *testing.T) {
	image := VisionInput{MIMEType: "image/png", Data: []byte("png")}
	tests := []struct{ provider, kind, contains string }{
		{llmprovider.OpenAIResponses, "responses", `"type":"input_image"`},
		{llmprovider.OpenAI, "chat", `"type":"image_url"`},
		{llmprovider.LiteLLM, "chat", `"type":"image_url"`},
		{llmprovider.Ollama, "ollama", `"images":["cG5n"]`},
	}
	for _, tc := range tests {
		t.Run(tc.provider, func(t *testing.T) {
			body, kind, err := visionStructuredRequest(Config{Provider: tc.provider, Model: "vision"}, "system", "user", image, map[string]any{"type": "object"})
			if err != nil || kind != tc.kind {
				t.Fatalf("kind=%q body=%+v err=%v", kind, body, err)
			}
			raw, err := json.Marshal(body)
			if err != nil || !strings.Contains(string(raw), tc.contains) {
				t.Fatalf("request=%s contains=%q err=%v", raw, tc.contains, err)
			}
		})
	}
}

func TestExtractStructuredChatAndOllama(t *testing.T) {
	text, usage, err := extractStructuredResponse("chat", []byte(`{"choices":[{"message":{"content":"hello"}}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`))
	if err != nil || text != "hello" || usage.TotalTokens != 7 {
		t.Fatalf("chat text=%q usage=%+v err=%v", text, usage, err)
	}
	text, usage, err = extractStructuredResponse("ollama", []byte(`{"message":{"content":"answer"},"prompt_eval_count":8,"eval_count":2}`))
	if err != nil || text != "answer" || usage.TotalTokens != 10 {
		t.Fatalf("ollama text=%q usage=%+v err=%v", text, usage, err)
	}
}

func TestParseStructuredJSONFallback(t *testing.T) {
	var output struct {
		Answer string `json:"answer"`
	}
	if err := parseStructuredJSON("result: ```json\n{\"answer\":\"42\"}\n```", &output); err != nil || output.Answer != "42" {
		t.Fatalf("output=%+v err=%v", output, err)
	}
	if err := parseStructuredJSON("no object", &output); err == nil {
		t.Fatal("expected unparseable response error")
	}
}

func TestStructuredRequestUsesRegisteredProtocol(t *testing.T) {
	const providerName = "test-openai-compatible"
	if _, exists := llmprovider.Lookup(providerName); !exists {
		if err := llmprovider.Register(llmprovider.Specification{
			Name:         providerName,
			DefaultModel: "test-model",
			Protocol:     llmprovider.ProtocolOpenAIChat,
			Capabilities: llmprovider.CapabilityStructuredOutput,
		}); err != nil {
			t.Fatal(err)
		}
	}
	body, kind, err := structuredRequest(Config{Provider: providerName, Model: "custom-model"}, "system", "user", map[string]any{"type": "object"})
	if err != nil || kind != "chat" {
		t.Fatalf("kind=%q body=%+v err=%v", kind, body, err)
	}
	if body["model"] != "custom-model" {
		t.Fatalf("request body = %+v", body)
	}
}
