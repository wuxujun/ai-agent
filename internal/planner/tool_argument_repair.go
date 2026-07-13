package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/wuxujun/ai-agent/internal/config"
	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/tools"
	"github.com/wuxujun/ai-agent/internal/types"
)

type ToolArgumentRepairer interface {
	Repair(ctx context.Context, goal, action string, parameters map[string]any, validationError error) (map[string]any, types.TokenUsage, error)
}

type LLMToolArgumentRepairer struct {
	Scene string
}

func NewLLMToolArgumentRepairer(scene string) *LLMToolArgumentRepairer {
	return &LLMToolArgumentRepairer{Scene: scene}
}

func (r *LLMToolArgumentRepairer) Repair(ctx context.Context, goal, action string, parameters map[string]any, validationError error) (map[string]any, types.TokenUsage, error) {
	tool, ok := tools.Get(action)
	if !ok {
		return nil, types.TokenUsage{}, fmt.Errorf("cannot repair unknown action %q", action)
	}
	parameterProperties := tool.Parameters()
	parameterNames := make([]string, 0, len(parameterProperties))
	for name := range parameterProperties {
		parameterNames = append(parameterNames, name)
	}
	sort.Strings(parameterNames)
	schema := map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"parameters": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"properties":           parameterProperties,
				"required":             parameterNames,
			},
		},
		"required": []string{"parameters"},
	}
	originalJSON, _ := json.Marshal(parameters)
	schemaJSON, _ := json.Marshal(parameterProperties)
	var output struct {
		Parameters map[string]any `json:"parameters"`
	}
	prompt := fmt.Sprintf("Goal: %s\nTool: %s\nValidation error: %v\nOriginal parameters: %s\nTool parameter schema: %s", goal, action, validationError, originalJSON, schemaJSON)
	usage, err := llmcore.CallJSON(ctx, llmcore.ConfigForScene(r.Scene), `Repair only the supplied tool parameters so they satisfy the tool schema and validation error. Preserve the user's intent. Do not change the tool, broaden paths, bypass policy restrictions, or introduce unrelated values. Return JSON only.`, truncateRunes(prompt, 32000), schema, &output)
	if err != nil {
		return nil, usage, err
	}
	if output.Parameters == nil {
		return nil, usage, fmt.Errorf("tool argument repair returned no parameters")
	}
	for name := range output.Parameters {
		if _, allowed := parameterProperties[name]; !allowed {
			return nil, usage, fmt.Errorf("tool argument repair returned unknown parameter %q", name)
		}
	}
	for _, name := range parameterNames {
		if _, present := output.Parameters[name]; !present {
			return nil, usage, fmt.Errorf("tool argument repair omitted parameter %q", name)
		}
	}
	if err := ValidateToolArguments(action, output.Parameters); err != nil {
		return nil, usage, fmt.Errorf("repaired parameters are still invalid: %w", err)
	}
	return output.Parameters, usage, nil
}

func toolArgumentRepairConfigured() bool {
	_, ok := config.Get().LLM.Scenes[config.LLMSceneToolArgumentRepair]
	return ok
}
