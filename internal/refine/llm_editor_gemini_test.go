package refine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/types"
)

func TestBudgetedEditorGeminiThinkingUsage(t *testing.T) {
	for _, tc := range []struct {
		name            string
		thoughts, total int
		ok              bool
	}{
		{"thinking within reservation", 11, 23, true},
		{"thinking exceeds output reservation", 4096, 4108, false},
		{"inconsistent provider total", 11, 12, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var request struct {
					GenerationConfig struct {
						MaxOutputTokens int `json:"maxOutputTokens"`
					} `json:"generationConfig"`
				}
				if json.NewDecoder(r.Body).Decode(&request) != nil || request.GenerationConfig.MaxOutputTokens != 4096 {
					t.Error("budgeted output limit did not reach Gemini transport")
				}
				proposal, _ := json.Marshal(llmProposal{General: []llmEdit{{Target: "teams.software.planner.model", Op: "set", Before: "m1", After: "m2", Reason: "fixture"}}, Domain: []llmEdit{}})
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"candidates":    []any{map[string]any{"content": map[string]any{"role": "model", "parts": []any{map[string]any{"text": string(proposal)}}}}},
					"usageMetadata": map[string]any{"promptTokenCount": 5, "candidatesTokenCount": 7, "thoughtsTokenCount": tc.thoughts, "totalTokenCount": tc.total},
				})
			}))
			defer server.Close()
			editor := LLMEditor{MaxCostUSD: 1, SystemPrompt: "system", LLMConfig: &llm.Config{Provider: "gemini", Model: "gemini-test", APIKey: "synthetic-editor-usage-key", BaseURL: server.URL, InputCostPerMillionUSD: 1, OutputCostPerMillionUSD: 2}}
			ctx := llm.WithRuntime(t.Context(), llm.NewDefaultRuntime(nil))
			general, domain, usage, err := editor.Propose(ctx, testBase(), "software", 1, nil)
			if (err == nil) != tc.ok || calls.Load() != 1 || usage != (types.TokenUsage{PromptTokens: 5, CompletionTokens: 7 + tc.thoughts, TotalTokens: tc.total}) {
				t.Fatalf("calls=%d usage=%+v err=%v", calls.Load(), usage, err)
			}
			if tc.ok && (len(general) != 1 || general[0].Edits[0].After != "m2") {
				t.Fatal("valid thinking proposal rejected")
			}
			if !tc.ok && len(general)+len(domain) != 0 {
				t.Fatal("invalid usage proposal returned candidates")
			}
		})
	}
}
