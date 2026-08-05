package llm

import (
	"context"
	"testing"

	"github.com/wuxujun/ai-agent/internal/types"
	"go.opentelemetry.io/otel/trace"
)

func TestLiteLLMMetadataCorrelatesTaskTraceAndPrompt(t *testing.T) {
	traceID, err := trace.TraceIDFromHex("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	spanID, err := trace.SpanIDFromHex("0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	spanContext := trace.NewSpanContext(trace.SpanContextConfig{TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled})
	ctx := trace.ContextWithSpanContext(context.Background(), spanContext)
	ctx = WithTaskBudget(ctx, &types.Task{ID: "task-123", TenantID: "tenant-456"})
	ctx = WithPromptBinding(ctx, PromptBinding{Name: "teams/software/writer", Version: 7, Source: "langfuse"})

	metadata := liteLLMMetadata(ctx, Config{Provider: "litellm", Scene: "multiagent_writer"})
	if metadata["trace_id"] != traceID.String() || metadata["parent_observation_id"] != spanID.String() {
		t.Fatalf("trace metadata=%v", metadata)
	}
	if metadata["task_id"] != "task-123" || metadata["session_id"] != "task-123" || metadata["tenant_id"] != "tenant-456" || metadata["user_id"] != "tenant-456" {
		t.Fatalf("task metadata=%v", metadata)
	}
	if metadata["generation_name"] != "multiagent_writer" || metadata["observation.prompt.name"] != "teams/software/writer" || metadata["observation.prompt.version"] != 7 || metadata["langfuse_prompt_name"] != "teams/software/writer" || metadata["langfuse_prompt_version"] != 7 {
		t.Fatalf("generation metadata=%v", metadata)
	}
}

func TestLiteLLMMetadataDoesNotModifyDirectProviderRequests(t *testing.T) {
	ctx := WithPromptBinding(context.Background(), PromptBinding{Name: "prompt", Version: 1, Source: "langfuse"})
	if metadata := liteLLMMetadata(ctx, Config{Provider: "openai", Scene: "writer"}); metadata != nil {
		t.Fatalf("direct provider metadata=%v, want nil", metadata)
	}
	if _, ok := promptBindingFromContext(WithPromptBinding(ctx, PromptBinding{Name: "fallback", Version: 0, Source: "fallback"})); !ok {
		t.Fatal("an invalid binding must not replace an existing valid context binding")
	}
}
