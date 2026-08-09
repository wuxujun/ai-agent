package planner

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/wuxujun/ai-agent/internal/sanitize"
)

const plannedParameterLogLimit = 500

// plannedStepLog is the safe, structured representation of a validated planner
// decision. Actions in one decision execute in parallel, so they share an
// orchestration step and are distinguished by action_index.
type plannedStepLog struct {
	Step        int            `json:"step"`
	ActionIndex int            `json:"action_index"`
	Action      string         `json:"action"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

func plannedStepsForLog(step int, actions []ActionCall) []plannedStepLog {
	result := make([]plannedStepLog, 0, len(actions))
	for i, action := range actions {
		result = append(result, plannedStepLog{
			Step:        step,
			ActionIndex: i,
			Action:      action.Action,
			Parameters:  safeParametersForLog(action.Parameters),
		})
	}
	return result
}

func safeParametersForLog(parameters map[string]any) map[string]any {
	if len(parameters) == 0 {
		return nil
	}
	result := make(map[string]any, len(parameters))
	for key, value := range parameters {
		lowerKey := strings.ToLower(key)
		if strings.Contains(lowerKey, "secret") ||
			strings.Contains(lowerKey, "token") ||
			strings.Contains(lowerKey, "password") ||
			strings.Contains(lowerKey, "api_key") ||
			lowerKey == "key" {
			result[key] = "[REDACTED]"
			continue
		}
		if isSensitivePlannedParameter(lowerKey) {
			result[key] = fmt.Sprintf("<%d chars>", len([]rune(fmt.Sprint(value))))
			continue
		}

		text, ok := value.(string)
		if !ok {
			raw, err := json.Marshal(value)
			if err != nil {
				result[key] = "[UNENCODABLE]"
				continue
			}
			text = string(raw)
		}
		result[key] = truncateLogValue(sanitize.Secrets(text), plannedParameterLogLimit)
	}
	return result
}

func isSensitivePlannedParameter(key string) bool {
	switch key {
	case "args", "command", "content", "input", "output", "prompt", "query", "url":
		return true
	default:
		return false
	}
}

func truncateLogValue(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "..."
}
