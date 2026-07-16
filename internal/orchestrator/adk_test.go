package orchestrator

import (
	"context"
	"iter"
	"testing"

	"github.com/wuxujun/ai-agent/internal/tools"
	"github.com/wuxujun/ai-agent/internal/types"
	"google.golang.org/adk/model"
	"google.golang.org/genai"
)

type mockLLM struct {
	response *model.LLMResponse
	calls    int
}

type sequenceADKLLM struct {
	responses []*model.LLMResponse
	calls     int
}

func (m *sequenceADKLLM) Name() string { return "sequence-adk-llm" }

func (m *sequenceADKLLM) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	index := m.calls
	m.calls++
	return func(yield func(*model.LLMResponse, error) bool) {
		if index >= len(m.responses) {
			yield(nil, nil)
			return
		}
		yield(m.responses[index], nil)
	}
}

func (m *mockLLM) Name() string {
	return "mock-llm"
}

func (m *mockLLM) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	m.calls++
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(m.response, nil)
	}
}

func TestAdkNextExecutesModelAndCompletes(t *testing.T) {
	mockResponse := &model.LLMResponse{
		Content: &genai.Content{
			Role: string(genai.RoleModel),
			Parts: []*genai.Part{
				{
					Text: "The answer is secret-123",
				},
			},
		},
		TurnComplete: true,
		UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount:     20,
			CandidatesTokenCount: 5,
			TotalTokenCount:      25,
		},
	}

	m := &mockLLM{response: mockResponse}
	engine := &Engine{
		Mode:     ModeAdk,
		AdkModel: m,
	}

	task := &types.Task{
		ID:         "task-adk-1",
		Goal:       "find the key secret",
		Status:     "created",
		MaxSteps:   5,
		ToolBudget: 5,
		Workspace:  "./workspace",
	}

	err := engine.Next(context.Background(), task)
	if err != nil {
		t.Fatalf("ADK Next returned error: %v", err)
	}

	if m.calls != 1 {
		t.Fatalf("expected 1 model call, got %d", m.calls)
	}
	if task.LLMCalls != 1 {
		t.Fatalf("expected ADK call to consume task LLM quota, got %d", task.LLMCalls)
	}

	if task.Status != types.StatusCompleted {
		t.Fatalf("expected task status completed, got %s", task.Status)
	}

	if task.FinalAnswer != "The answer is secret-123" {
		t.Fatalf("expected final answer 'The answer is secret-123', got '%s'", task.FinalAnswer)
	}
	usage := aggregateTaskTokenUsage(task)
	if usage.PromptTokens != 20 || usage.CompletionTokens != 5 || usage.TotalTokens != 25 {
		t.Fatalf("ADK token usage was not recorded: %+v", usage)
	}
}

func TestAdkUnableAnswerIsPartial(t *testing.T) {
	m := &mockLLM{response: &model.LLMResponse{
		Content:      genai.NewContentFromText("I am unable to retrieve any information, so I cannot answer.", genai.RoleModel),
		TurnComplete: true,
	}}
	engine := &Engine{Mode: ModeAdk, AdkModel: m}
	task := &types.Task{ID: "task-adk-unable", Goal: "lookup", Status: types.StatusCreated, MaxSteps: 5, ToolBudget: 5}
	if err := engine.Next(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if task.Status != types.StatusPartial || task.FinalAnswer == "" {
		t.Fatalf("status=%s answer=%q", task.Status, task.FinalAnswer)
	}
}

func TestAdkUnableToAnswerMarkers(t *testing.T) {
	for _, answer := range []string{
		"I am unable to retrieve any information.",
		"未检索到足够证据，无法回答。",
	} {
		if !adkUnableToAnswer(answer) {
			t.Fatalf("did not recognize unsupported answer %q", answer)
		}
	}
	if adkUnableToAnswer("根据检索证据，数学竞赛设置金奖。") {
		t.Fatal("valid answer was classified as unsupported")
	}
}

func TestAdkExhaustedBudgetIsPartial(t *testing.T) {
	engine := &Engine{Mode: ModeAdk, AdkModel: &mockLLM{}}
	task := &types.Task{ID: "task-adk-budget", Goal: "lookup", Status: types.StatusRunning, MaxSteps: 5, ToolBudget: 0}
	if err := engine.Next(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if task.Status != types.StatusPartial {
		t.Fatalf("status=%s, want partial", task.Status)
	}
}

func TestAdkRAGToolRetrievesEvidence(t *testing.T) {
	oldSearch, _ := tools.Get("rag_search")
	oldFetch, _ := tools.Get("rag_fetch")
	tools.RegisterRetrievalTools(tools.RetrievalDependencies{
		SearchRAG: func(context.Context, string) ([]types.Memory, error) {
			return []types.Memory{{Goal: "数学竞赛", KeyFindings: "数学竞赛设置金奖、银奖和铜奖。"}}, nil
		},
	})
	t.Cleanup(func() {
		if oldSearch != nil {
			tools.Register(oldSearch)
		}
		if oldFetch != nil {
			tools.Register(oldFetch)
		}
	})

	toolCall := &model.LLMResponse{
		Content: &genai.Content{
			Role: string(genai.RoleModel),
			Parts: []*genai.Part{{
				FunctionCall: &genai.FunctionCall{
					ID:   "rag-call-1",
					Name: "rag_search",
					Args: map[string]any{"query": "数学竞赛奖项", "top_k": float64(5)},
				},
			}},
		},
		TurnComplete: true,
	}
	answer := &model.LLMResponse{
		Content:      genai.NewContentFromText("数学竞赛设有金奖、银奖和铜奖。", genai.RoleModel),
		TurnComplete: true,
	}
	m := &sequenceADKLLM{responses: []*model.LLMResponse{toolCall, answer}}
	engine := &Engine{Mode: ModeAdk, AdkModel: m}
	task := &types.Task{ID: "task-adk-rag", TenantID: "default", Goal: "数学竞赛有哪些奖项", Status: types.StatusCreated, MaxSteps: 5, ToolBudget: 5}
	if err := engine.Next(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if task.Status != types.StatusCompleted || m.calls != 2 {
		t.Fatalf("status=%s calls=%d answer=%q", task.Status, m.calls, task.FinalAnswer)
	}
	if len(task.Trace) == 0 || task.Trace[0].Action != "rag_search" || len(task.Trace[0].Evidence) == 0 {
		t.Fatalf("RAG evidence trace not recorded: %+v", task.Trace)
	}
}
