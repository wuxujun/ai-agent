package planner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wuxujun/ai-agent/internal/types"
)

func TestLLMPlannerProviders(t *testing.T) {
	// Sample task
	task := &types.Task{
		ID:         "test-task-1",
		Goal:       "find hello file",
		Status:     types.StatusCreated,
		MaxSteps:   5,
		ToolBudget: 5,
	}

	expectedDecision := PlanDecision{
		ThoughtSummary: "Finding files",
		Stop:           false,
		FinalAnswer:    "",
		Actions: []ActionCall{
			{
				Action: "find_files",
				Parameters: map[string]any{
					"pattern": "*",
				},
			},
		},
	}

	expectedJSONBytes, _ := json.Marshal(expectedDecision)
	expectedJSONStr := string(expectedJSONBytes)

	t.Run("openai-responses", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Mock OpenAI responses API format
			resp := map[string]any{
				"output": []any{
					map[string]any{
						"content": []any{
							map[string]any{
								"text": expectedJSONStr,
							},
						},
					},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
		}))
		defer server.Close()

		planner := NewLLMPlannerWithProvider(ProviderOpenAIResponses, "test-key", "gpt-4.1", server.URL)
		decision, err := planner.PlanNext(context.Background(), task, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if len(decision.Actions) == 0 || decision.Actions[0].Action != expectedDecision.Actions[0].Action {
			t.Errorf("expected action %q, got %+v", expectedDecision.Actions[0].Action, decision.Actions)
		}
	})

	t.Run("openai", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Mock OpenAI chat completions format
			resp := map[string]any{
				"choices": []any{
					map[string]any{
						"delta": map[string]any{
							"content": expectedJSONStr,
						},
					},
				},
			}
			w.Header().Set("Content-Type", "text/event-stream")
			b, _ := json.Marshal(resp)
			w.Write([]byte("data: " + string(b) + "\n\n"))
			w.Write([]byte("data: [DONE]\n\n"))
		}))
		defer server.Close()

		planner := NewLLMPlannerWithProvider(ProviderOpenAI, "test-key", "gpt-4o", server.URL)
		decision, err := planner.PlanNext(context.Background(), task, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if len(decision.Actions) == 0 || decision.Actions[0].Action != expectedDecision.Actions[0].Action {
			t.Errorf("expected action %q, got %+v", expectedDecision.Actions[0].Action, decision.Actions)
		}
	})

	t.Run("ollama", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Mock Ollama chat response format
			resp := map[string]any{
				"message": map[string]any{
					"content": expectedJSONStr,
				},
				"done": true,
			}
			w.Header().Set("Content-Type", "application/x-ndjson")
			_ = json.NewEncoder(w).Encode(resp)
		}))
		defer server.Close()

		planner := NewLLMPlannerWithProvider(ProviderOllama, "", "llama3", server.URL)
		decision, err := planner.PlanNext(context.Background(), task, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if len(decision.Actions) == 0 || decision.Actions[0].Action != expectedDecision.Actions[0].Action {
			t.Errorf("expected action %q, got %+v", expectedDecision.Actions[0].Action, decision.Actions)
		}
	})

	t.Run("gemini", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Mock Gemini SDK response format
			resp := map[string]any{
				"candidates": []any{
					map[string]any{
						"content": map[string]any{
							"parts": []any{
								map[string]any{
									"text": expectedJSONStr,
								},
							},
							"role": "model",
						},
						"finishReason": "STOP",
					},
				},
			}
			w.Header().Set("Content-Type", "text/event-stream")
			b, _ := json.Marshal(resp)
			w.Write([]byte("data: " + string(b) + "\n\n"))
		}))
		defer server.Close()

		planner := NewLLMPlannerWithProvider(ProviderGemini, "test-key", "gemini-2.5-flash", server.URL)
		decision, err := planner.PlanNext(context.Background(), task, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if len(decision.Actions) == 0 || decision.Actions[0].Action != expectedDecision.Actions[0].Action {
			t.Errorf("expected action %q, got %+v", expectedDecision.Actions[0].Action, decision.Actions)
		}
	})
}

func TestUnmarshalDecisionFallback(t *testing.T) {
rawInput := `thought_summary\nThe file content confirms that CAIE refers to the Cambridge Assessment International Education, specifically for AS level exams in 2026. This is sufficient to answer the prompt.{
  "thought_summary": "Finding files",
  "stop": false,
  "final_answer": "",
  "actions": [
    {
      "action": "find_files",
      "parameters": {
        "pattern": "*"
      }
    }
  ]
}`

	var decision PlanDecision
	err := unmarshalDecision(rawInput, &decision)
	if err != nil {
		t.Fatalf("failed to unmarshal with fallback: %v", err)
	}

	if len(decision.Actions) == 0 || decision.Actions[0].Action != "find_files" {
		t.Errorf("expected action find_files, got %+v", decision.Actions)
	}
	if decision.ThoughtSummary != "Finding files" {
		t.Errorf("expected thought_summary 'Finding files', got %q", decision.ThoughtSummary)
	}
}
