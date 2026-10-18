package multiagent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/promptmanager"
	"github.com/wuxujun/ai-agent/internal/types"
)

type tenantBindingPlanner struct{ promptPinPlanner }

func (tenantBindingPlanner) Plan(ctx context.Context, _, _ string, _ []types.Memory) (*ResearchPlan, error) {
	_, err := resolveAgentPromptDetailsForTask(ctx, AgentConfig{PromptName: "teams/test/planner"}, "default", "fallback")
	if err != nil {
		return nil, err
	}
	return &ResearchPlan{Steps: []ResearchStep{{ID: "step-1", Action: "read_file", FilePath: "result.txt"}}}, nil
}

func TestCoordinatorUsesTaskTenantPromptAndPersistsPin(t *testing.T) {
	t.Setenv("AI_AGENT_MULTIAGENT_TEAM", "software")
	t.Setenv("AI_AGENT_MULTIAGENT_RUNTIME", "legacy")
	t.Setenv("AI_AGENT_MULTIAGENT_DAG_CANARY_PERCENT", "0")
	tenantName, _ := promptmanager.TenantPromptName("tenant-a", "teams/test/planner")
	var pinnedReads atomic.Int32
	var labelVersion atomic.Int32
	labelVersion.Store(2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/api/public/v2/prompts/")
		if name != tenantName {
			t.Error("task read another tenant or system prompt")
			w.WriteHeader(500)
			return
		}
		version := int(labelVersion.Load())
		if r.URL.Query().Get("version") == "2" {
			pinnedReads.Add(1)
			version = 2
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"name": name, "version": version, "labels": []string{"production"}, "prompt": "tenant custom content"})
	}))
	t.Cleanup(server.Close)
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.Langfuse.Enabled = true
		cfg.Langfuse.Host = server.URL
		cfg.Langfuse.PublicKey = "pk-test"
		cfg.Langfuse.SecretKey = "sk-test"
	}))
	coordinator := newPromptPinCoordinator()
	coordinator.Planner = tenantBindingPlanner{}
	task := newPromptPinTask(t)
	task.TenantID = "tenant-a"
	// A caller's stale scope must never override the task's persisted tenant.
	ctx := promptmanager.WithTenantScope(t.Context(), "tenant-b")
	if err := coordinator.Run(ctx, task); err != nil {
		t.Fatal(err)
	}
	teams := GetTeamsConfig()
	digest := newTeamConfigSnapshot(teams.ActiveTeam, teams.GetActiveTeam()).Digest
	pins := persistedPromptVersionPins(task, digest)
	if len(pins) != 1 || pins[0].Name != tenantName || pins[0].Version != 2 {
		t.Fatalf("tenant version pin = %+v", pins)
	}
	labelVersion.Store(3)
	resumed := newPromptPinTask(t)
	resumed.TenantID = task.TenantID
	resumed.Trace = append([]types.StepTrace(nil), task.Trace...)
	resumed.StepCount = task.StepCount
	if err := coordinator.Run(ctx, resumed); err != nil {
		t.Fatal(err)
	}
	if pinnedReads.Load() == 0 {
		t.Fatal("resume did not request tenant pinned version")
	}
}

func TestTenantPromptBootstrapCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := BootstrapTenantTeamPrompts(ctx, "tenant-a", &TeamsConfig{}); err == nil {
		t.Fatal("cancellation ignored")
	}
	if _, err := BootstrapTenantTeamPrompts(t.Context(), "", &TeamsConfig{}); err == nil {
		t.Fatal("missing tenant accepted")
	}
}
