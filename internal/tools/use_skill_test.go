package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/wuxujun/ai-agent/internal/skills"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestUseSkillEmitsSearchableSpan(t *testing.T) {
	root := t.TempDir()
	skillDir := filepath.Join(root, "code-review")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(`---
name: code-review
description: Review code.
allowed-tools: read_file
---
Review the code carefully.
`), 0o644); err != nil {
		t.Fatal(err)
	}

	registry := skills.NewRegistry(root)
	if err := registry.Load(); err != nil {
		t.Fatal(err)
	}

	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previousProvider := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		otel.SetTracerProvider(previousProvider)
		skillTracer = previousProvider.Tracer("ai-agent/skills")
		_ = provider.Shutdown(context.Background())
	})
	skillTracer = otel.Tracer("ai-agent/skills")

	result, err := (&UseSkillTool{Skills: registry}).Execute(
		context.Background(),
		root,
		map[string]interface{}{"name": "code-review"},
	)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Query != "use_skill:code-review" {
		t.Fatalf("query = %q", result.Query)
	}

	spans := recorder.Ended()
	if len(spans) != 1 || spans[0].Name() != "skill.use" {
		t.Fatalf("spans = %+v", spans)
	}
	attributes := make(map[string]any)
	for _, attr := range spans[0].Attributes() {
		attributes[string(attr.Key)] = attr.Value.AsInterface()
	}
	if attributes["agent.skill.name"] != "code-review" {
		t.Fatalf("skill attribute = %#v", attributes["agent.skill.name"])
	}
	if attributes["agent.skill.loaded"] != true {
		t.Fatalf("loaded attribute = %#v", attributes["agent.skill.loaded"])
	}
	if attributes["agent.orchestrator.mode"] == "" {
		t.Fatalf("orchestrator mode attribute = %#v", attributes["agent.orchestrator.mode"])
	}
	if attributes["agent.skill.allowed_tool_count"] != int64(1) {
		t.Fatalf("allowed tool count = %#v", attributes["agent.skill.allowed_tool_count"])
	}
}
