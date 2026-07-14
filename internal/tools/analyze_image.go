package tools

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/policy"
	"github.com/wuxujun/ai-agent/internal/types"
	"github.com/wuxujun/ai-agent/internal/vision"
)

const maxVisionImageBytes = 10 << 20

type AnalyzeImageTool struct{}

func (t *AnalyzeImageTool) Name() string { return "analyze_image" }
func (t *AnalyzeImageTool) Description() string {
	return "Analyze an image file in the workspace using the configured vision model"
}
func (t *AnalyzeImageTool) RiskLevel() types.RiskLevel { return types.RiskLevelLow }
func (t *AnalyzeImageTool) Parameters() map[string]any {
	return map[string]any{
		"path":   map[string]any{"type": "string", "description": "Workspace-relative image path"},
		"prompt": map[string]any{"type": "string", "description": "Question or analysis instruction for the image"},
	}
}
func (t *AnalyzeImageTool) Validate(params map[string]any) error {
	path, _ := params["path"].(string)
	path = strings.TrimSpace(path)
	if path == "" || filepath.IsAbs(path) || strings.Contains(path, "..") {
		return fmt.Errorf("analyze_image requires a valid workspace-relative path")
	}
	if _, ok := params["prompt"].(string); !ok {
		return fmt.Errorf("analyze_image requires prompt to be a string")
	}
	return nil
}
func (t *AnalyzeImageTool) Execute(ctx context.Context, workspace string, params map[string]interface{}) (*ToolResult, error) {
	path, _ := params["path"].(string)
	prompt, _ := params["prompt"].(string)
	full := filepath.Join(workspace, path)
	if err := policy.ValidateReadPath(workspace, full); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(full, os.O_RDONLY|policy.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxVisionImageBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxVisionImageBytes {
		return nil, fmt.Errorf("image exceeds 10 MiB limit")
	}
	mimeType := http.DetectContentType(data)
	if !supportedImageMIME(mimeType) {
		return nil, fmt.Errorf("unsupported image MIME type %q; supported: PNG, JPEG, GIF, WebP", mimeType)
	}
	if _, configured := config.Get().LLM.Scenes[config.LLMSceneVisionAnalyzer]; !configured {
		return nil, fmt.Errorf("LLM scene %q is not configured", config.LLMSceneVisionAnalyzer)
	}
	analysis, usage, err := vision.Analyze(ctx, mimeType, data, prompt)
	if err != nil {
		return nil, err
	}
	lines := []string{analysis.Description}
	if analysis.DetectedText != "" {
		lines = append(lines, "Detected text: "+analysis.DetectedText)
	}
	if len(analysis.Objects) > 0 {
		lines = append(lines, "Objects: "+strings.Join(analysis.Objects, ", "))
	}
	if len(analysis.Warnings) > 0 {
		lines = append(lines, "Warnings: "+strings.Join(analysis.Warnings, "; "))
	}
	return &ToolResult{Query: prompt, Observation: analysis.Description, Evidence: []types.Evidence{{Path: path, Query: prompt, Lines: lines}}, TokenUsage: usage}, nil
}

func supportedImageMIME(value string) bool {
	switch value {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	default:
		return false
	}
}

func init() { Register(&AnalyzeImageTool{}) }
