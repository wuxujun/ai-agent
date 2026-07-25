package llm_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	_ "github.com/wuxujun/ai-agent/internal/multiagent"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestLiteLLMStructuredOutputContract(t *testing.T) {
	originalTransport := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			return nil, err
		}
		if body["model"] != "agent-writer" || body["response_format"] == nil {
			t.Errorf("unexpected request body: %+v", body)
		}
		messages, ok := body["messages"].([]any)
		if !ok || len(messages) == 0 {
			t.Errorf("missing request messages: %+v", body)
		} else {
			rawMessages, _ := json.Marshal(messages)
			if !strings.Contains(strings.ToLower(string(rawMessages)), "json") {
				t.Errorf("structured LiteLLM request messages must mention JSON: %s", rawMessages)
			}
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"{\"answer\":\"ok\"}"}}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`)), Request: r}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = originalTransport })

	var output struct {
		Answer string `json:"answer"`
	}
	schema := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"answer": map[string]any{"type": "string"}}, "required": []string{"answer"}}
	usage, err := llmcore.CallJSON(context.Background(), llmcore.Config{Scene: "contract", Provider: "litellm", Model: "agent-writer", BaseURL: "http://litellm.test/v1/chat/completions", Timeout: time.Second}, "system", "user", schema, &output)
	if err != nil {
		t.Fatal(err)
	}
	if output.Answer != "ok" || usage.TotalTokens != 7 {
		t.Fatalf("output=%+v usage=%+v", output, usage)
	}
}
