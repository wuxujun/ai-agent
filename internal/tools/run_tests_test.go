package tools

import (
	"testing"

	"github.com/wuxujun/ai-agent/internal/types"
)

func TestRunTestsToolValidation(t *testing.T) {
	tool := &RunTestsTool{}
	for _, params := range []map[string]any{
		{},
		{"package": "./..."},
		{"package": "./internal/store"},
		{"package": "./internal/store/...", "run": "TestQuery", "race": true},
	} {
		if err := tool.Validate(params); err != nil {
			t.Errorf("valid params rejected: %+v: %v", params, err)
		}
	}
	for _, params := range []map[string]any{
		{"package": "internal/store"},
		{"package": "../store"},
		{"package": "/tmp/pkg"},
		{"package": "./internal/store -count=1"},
		{"package": "./internal/../store"},
		{"package": "./internal/store", "race": "true"},
		{"package": "./internal/store", "run": 1},
	} {
		if err := tool.Validate(params); err == nil {
			t.Errorf("invalid params accepted: %+v", params)
		}
	}
	if tool.RiskLevel() != types.RiskLevelHigh {
		t.Fatalf("risk level = %q, want high", tool.RiskLevel())
	}
}
