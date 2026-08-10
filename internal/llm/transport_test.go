package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wuxujun/ai-agent/internal/llmprovider"
)

func TestNativeStructuredCallerStreamsOpenAIChat(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		raw, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(raw), `"stream":true`) || !strings.Contains(string(raw), `"include_usage":true`) {
			t.Errorf("request does not enable usage streaming: %s", raw)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"{\\\"answer\\\":\\\"hel\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"lo\\\"}\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":8,\"completion_tokens\":2,\"total_tokens\":10}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	var dest struct {
		Answer string `json:"answer"`
	}
	var chunks []string
	usage, err := (nativeStructuredCaller{}).CallJSONStream(context.Background(), Config{Provider: llmprovider.LiteLLM, Model: "test", BaseURL: server.URL}, "system", "user", map[string]any{"type": "object"}, &dest, func(chunk string) {
		chunks = append(chunks, chunk)
	})
	if err != nil {
		t.Fatal(err)
	}
	if dest.Answer != "hello" || strings.Join(chunks, "") != `{"answer":"hello"}` {
		t.Fatalf("dest=%+v chunks=%#v", dest, chunks)
	}
	if usage.TotalTokens != 10 {
		t.Fatalf("usage=%+v", usage)
	}
}

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
	messages, ok := body["messages"].([]map[string]any)
	if !ok || len(messages) == 0 {
		t.Fatalf("structured request messages missing: %+v", body["messages"])
	}
	systemPrompt, _ := messages[0]["content"].(string)
	if !strings.Contains(systemPrompt, `"type":"object"`) || !strings.Contains(systemPrompt, "JSON Schema") {
		t.Fatalf("structured request system prompt must include JSON schema: %q", systemPrompt)
	}
}

func TestVisionStructuredChatRequestMentionsJSON(t *testing.T) {
	body, kind, err := visionStructuredRequest(
		Config{Provider: llmprovider.LiteLLM, Model: "agent-vision"},
		"Analyze the image.",
		"Describe what is visible.",
		VisionInput{MIMEType: "image/png", Data: []byte("png")},
		map[string]any{"type": "object"},
	)
	if err != nil || kind != "chat" {
		t.Fatalf("kind=%q body=%+v err=%v", kind, body, err)
	}
	messages, ok := body["messages"].([]map[string]any)
	if !ok || len(messages) == 0 {
		t.Fatalf("structured vision request messages missing: %+v", body["messages"])
	}
	systemPrompt, _ := messages[0]["content"].(string)
	if !strings.Contains(systemPrompt, `"type":"object"`) || !strings.Contains(systemPrompt, "JSON Schema") {
		t.Fatalf("structured vision request system prompt must include JSON schema: %q", systemPrompt)
	}
}
