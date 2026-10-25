package planner

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wuxujun/ai-agent/internal/types"
)

func TestGeminiPlannerStreamsCompleteUsage(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if !strings.Contains(r.URL.Path, ":streamGenerateContent") {
			t.Errorf("unexpected Gemini route")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"hello \"}]}}],\"usageMetadata\":{\"promptTokenCount\":5,\"toolUsePromptTokenCount\":1,\"candidatesTokenCount\":2,\"thoughtsTokenCount\":4,\"totalTokenCount\":12}}\n\n")
		_, _ = io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"world\"}]}}],\"usageMetadata\":{\"promptTokenCount\":5,\"cachedContentTokenCount\":2,\"toolUsePromptTokenCount\":3,\"candidatesTokenCount\":7,\"thoughtsTokenCount\":11,\"totalTokenCount\":26}}\n\n")
		_, _ = io.WriteString(w, "data: {\"candidates\":[]}\n\n")
	}))
	defer server.Close()
	var chunks []string
	text, usage, err := (&geminiProvider{}).Plan(t.Context(), PlanRequest{APIKey: "synthetic-usage-key", Model: "gemini-test", BaseURL: server.URL, SystemPrompt: "system", UserPrompt: "user"}, func(chunk string) { chunks = append(chunks, chunk) })
	if err != nil || text != "hello world" || strings.Join(chunks, "") != text || requests.Load() != 1 || usage != (types.TokenUsage{PromptTokens: 8, CompletionTokens: 18, TotalTokens: 26}) {
		t.Fatalf("requests=%d text=%q usage=%+v err=%v", requests.Load(), text, usage, err)
	}
}
