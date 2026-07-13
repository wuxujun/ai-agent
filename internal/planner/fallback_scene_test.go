package planner

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/types"
)

func TestPlannerSceneRetriesPrimaryAndFallback(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.LLM.Scenes = map[string]config.LLMEndpointConfig{
			"primary":  {Provider: "openai-responses", Model: "primary", BaseURL: "http://primary.test/v1/responses", MaxRetries: plannerTestPtr(1), FallbackScene: plannerTestPtr("fallback")},
			"fallback": {Provider: "openai-responses", Model: "fallback", BaseURL: "http://fallback.test/v1/responses", MaxRetries: plannerTestPtr(1)},
		}
	}))
	originalTransport := http.DefaultTransport
	primaryCalls, fallbackCalls := 0, 0
	http.DefaultTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		status, body := http.StatusInternalServerError, `{"error":"temporary"}`
		if req.URL.Host == "primary.test" {
			primaryCalls++
		} else {
			fallbackCalls++
			if fallbackCalls == 2 {
				status = http.StatusOK
				body = `{"output":[{"content":[{"text":"{\"thought_summary\":\"fallback\",\"stop\":true,\"final_answer\":\"recovered\",\"actions\":[{\"action\":\"none\",\"parameters\":{}}]}"}]}]}`
			}
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = originalTransport })

	decision, err := NewLLMPlannerForScene("primary").PlanNext(context.Background(), &types.Task{ID: "fallback", Goal: "goal", MaxSteps: 1, ToolBudget: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if decision.FinalAnswer != "recovered" || primaryCalls != 2 || fallbackCalls != 2 {
		t.Fatalf("decision=%+v primary_calls=%d fallback_calls=%d", decision, primaryCalls, fallbackCalls)
	}
}

func plannerTestPtr[T any](value T) *T { return &value }
