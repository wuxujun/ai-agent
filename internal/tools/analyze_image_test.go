package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/types"
	"github.com/wuxujun/ai-agent/internal/vision"
)

type fakeVisionCaller struct{ image llm.VisionInput }

func (f *fakeVisionCaller) CallJSON(context.Context, llm.Config, string, string, map[string]any, any) (types.TokenUsage, error) {
	return types.TokenUsage{}, nil
}

func (f *fakeVisionCaller) CallVisionJSON(_ context.Context, _ llm.Config, _, _ string, image llm.VisionInput, _ map[string]any, dest any) (types.TokenUsage, error) {
	f.image = image
	result := dest.(*vision.Analysis)
	result.Description = "one pixel image"
	result.Objects = []string{"pixel"}
	return types.TokenUsage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10}, nil
}

func TestAnalyzeImageTool(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.LLM.Provider = "openai"
		cfg.LLM.Model = "vision"
		cfg.LLM.Scenes = map[string]config.LLMEndpointConfig{config.LLMSceneVisionAnalyzer: {Model: "vision"}}
	}))
	workspace := t.TempDir()
	image := []byte("GIF89a\x01\x00\x01\x00\x80\x00\x00\x00\x00\x00\xff\xff\xff!\xf9\x04\x01\x00\x00\x00\x00,\x00\x00\x00\x00\x01\x00\x01\x00\x00\x02\x02D\x01\x00;")
	if err := os.WriteFile(filepath.Join(workspace, "pixel.gif"), image, 0o600); err != nil {
		t.Fatal(err)
	}
	caller := &fakeVisionCaller{}
	ctx := llm.WithRuntime(context.Background(), llm.NewRuntime(caller, nil))
	result, err := (&AnalyzeImageTool{}).Execute(ctx, workspace, map[string]interface{}{"path": "pixel.gif", "prompt": "Describe it"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Observation != "one pixel image" || result.TokenUsage.TotalTokens != 10 || caller.image.MIMEType != "image/gif" {
		t.Fatalf("result=%+v image=%+v", result, caller.image)
	}
}

func TestAnalyzeImageToolRejectsNonImage(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "note.txt"), []byte("not an image"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := (&AnalyzeImageTool{}).Execute(context.Background(), workspace, map[string]interface{}{"path": "note.txt", "prompt": "read"})
	if err == nil {
		t.Fatal("expected unsupported image error")
	}
}
