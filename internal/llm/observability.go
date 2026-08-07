package llm

import (
	"context"
	"strings"

	"github.com/wuxujun/ai-agent/internal/buildinfo"
	"github.com/wuxujun/ai-agent/internal/config"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// PromptBinding identifies the immutable Langfuse prompt version used for an
// LLM call. Content is intentionally excluded so observability metadata cannot
// duplicate prompt payloads or leak a local fallback.
type PromptBinding struct {
	Name    string
	Version int
	Source  string
}

type promptBindingContextKey struct{}

// WithPromptBinding associates a resolved prompt with the immediately
// following LLM call. Only real, versioned Langfuse prompts are linked.
func WithPromptBinding(ctx context.Context, binding PromptBinding) context.Context {
	if ctx == nil || strings.TrimSpace(binding.Name) == "" || binding.Version <= 0 || binding.Source != "langfuse" {
		return ctx
	}
	binding.Name = strings.TrimSpace(binding.Name)
	return context.WithValue(ctx, promptBindingContextKey{}, binding)
}

func promptBindingFromContext(ctx context.Context) (PromptBinding, bool) {
	if ctx == nil {
		return PromptBinding{}, false
	}
	binding, ok := ctx.Value(promptBindingContextKey{}).(PromptBinding)
	return binding, ok && binding.Name != "" && binding.Version > 0 && binding.Source == "langfuse"
}

func taskIdentityFromContext(ctx context.Context) (taskID, sessionID, tenantID string) {
	if ctx == nil {
		return "", "", ""
	}
	state, _ := ctx.Value(taskBudgetContextKey{}).(*taskBudgetState)
	if state == nil {
		return "", "", ""
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.task == nil {
		return "", "", state.tenantID
	}
	return state.task.ID, state.task.SessionID, state.task.TenantID
}

// applyLangfuseSpanAttributes makes application spans filterable after they
// are fanned out through the generic OTel Collector. It does not turn the span
// into a Generation; LiteLLM's langfuse_otel callback owns Generation details.
func applyLangfuseSpanAttributes(ctx context.Context, span trace.Span, cfg Config) {
	taskID, sessionID, tenantID := taskIdentityFromContext(ctx)
	attrs := []attribute.KeyValue{
		attribute.String("langfuse.version", buildinfo.Current()),
		attribute.String("langfuse.environment", config.Get().Telemetry.Environment),
		attribute.String("langfuse.observation.metadata.scene", cfg.Scene),
		attribute.String("langfuse.observation.metadata.provider", cfg.Provider),
	}
	if taskID != "" {
		if sessionID == "" {
			sessionID = taskID
		}
		attrs = append(attrs,
			attribute.String("langfuse.trace.name", "ai-agent.task"),
			attribute.String("langfuse.session.id", sessionID),
			attribute.String("langfuse.trace.metadata.task_id", taskID),
			attribute.String("langfuse.trace.metadata.session_id", sessionID),
			attribute.String("langfuse.observation.metadata.task_id", taskID),
		)
	}
	if tenantID != "" {
		attrs = append(attrs,
			attribute.String("langfuse.user.id", tenantID),
			attribute.String("langfuse.trace.metadata.tenant_id", tenantID),
		)
	}
	if binding, ok := promptBindingFromContext(ctx); ok {
		attrs = append(attrs,
			attribute.String("langfuse.observation.metadata.prompt_name", binding.Name),
			attribute.Int("langfuse.observation.metadata.prompt_version", binding.Version),
		)
	}
	span.SetAttributes(attrs...)
}

func liteLLMMetadata(ctx context.Context, cfg Config) map[string]any {
	if !strings.EqualFold(strings.TrimSpace(cfg.Provider), "litellm") {
		return nil
	}
	metadata := map[string]any{
		"generation_name": cfg.Scene,
		"scene":           cfg.Scene,
		"provider":        cfg.Provider,
		"app_version":     buildinfo.Current(),
		"version":         buildinfo.Current(),
		"environment":     config.Get().Telemetry.Environment,
		"tags":            []string{"ai-agent", cfg.Scene},
	}
	spanContext := trace.SpanContextFromContext(ctx)
	if spanContext.IsValid() {
		metadata["trace_id"] = spanContext.TraceID().String()
		metadata["parent_observation_id"] = spanContext.SpanID().String()
	}
	taskID, sessionID, tenantID := taskIdentityFromContext(ctx)
	if taskID != "" {
		if sessionID == "" {
			sessionID = taskID
		}
		metadata["session_id"] = sessionID
		metadata["task_id"] = taskID
	}
	if tenantID != "" {
		metadata["user_id"] = tenantID
		metadata["tenant_id"] = tenantID
	}
	if binding, ok := promptBindingFromContext(ctx); ok {
		// langfuse_otel prefixes arbitrary metadata keys with "langfuse.". The
		// dotted suffixes therefore become Langfuse's native prompt-link fields.
		metadata["observation.prompt.name"] = binding.Name
		metadata["observation.prompt.version"] = binding.Version
		// Retain legacy callback keys for pinned LiteLLM images predating the OTel
		// attribute mapping. They are harmless metadata on newer integrations.
		metadata["langfuse_prompt_name"] = binding.Name
		metadata["langfuse_prompt_version"] = binding.Version
	}
	return metadata
}
