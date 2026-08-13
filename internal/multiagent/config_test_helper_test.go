package multiagent

import (
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
)

func configureMultiAgentSelectionTest(t *testing.T, team string, runtime OrchestrationRuntime, percent int) {
	t.Helper()
	previous := config.Get().MultiAgent
	config.OverrideForTesting(func(cfg *config.Config) {
		cfg.MultiAgent.Team = team
		cfg.MultiAgent.Runtime = string(runtime)
		cfg.MultiAgent.DAGCanaryPercent = percent
	})
	t.Cleanup(func() {
		// Install the captured fields explicitly. OverrideForTesting deliberately
		// refuses stale restores, while these tests commonly nest other config
		// overrides or reload Langfuse settings before cleanup.
		config.OverrideForTesting(func(cfg *config.Config) {
			cfg.MultiAgent = previous
		})
	})
}
