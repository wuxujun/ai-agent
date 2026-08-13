package multiagent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/promptmanager"
	"github.com/wuxujun/ai-agent/internal/types"
)

type promptPinPlanner struct{}

func (promptPinPlanner) Plan(ctx context.Context, _, _ string, _ []types.Memory) (*ResearchPlan, error) {
	_, err := promptmanager.GetManager().ResolvePinned(ctx, "teams/test/planner", promptmanager.Selector{Label: "production"}, "fallback planner prompt")
	if err != nil {
		return nil, err
	}
	return &ResearchPlan{ThoughtSummary: "inspect", Steps: []ResearchStep{{ID: "step-1", Action: "read_file", FilePath: "result.txt"}}}, nil
}

func (promptPinPlanner) Replan(context.Context, string, string, []types.StepTrace, []types.Memory) (*ResearchPlan, error) {
	return &ResearchPlan{}, nil
}

func TestPromptVersionBindingsAreScopedByTeamDigest(t *testing.T) {
	task := &types.Task{Goal: "test"}
	appendPromptVersionBinding(task, "digest-a", promptmanager.VersionPin{Name: "critic", Version: 7, Selector: promptmanager.Selector{Label: "production"}})
	appendPromptVersionBinding(task, "digest-b", promptmanager.VersionPin{Name: "critic", Version: 8, Selector: promptmanager.Selector{Label: "latest"}})
	oldPins := persistedPromptVersionPins(task, "digest-a")
	newPins := persistedPromptVersionPins(task, "digest-b")
	if len(oldPins) != 1 || oldPins[0].Version != 7 || len(newPins) != 1 || newPins[0].Version != 8 {
		t.Fatalf("old=%+v new=%+v", oldPins, newPins)
	}
	if len(task.Trace) != 2 || strings.Contains(task.Trace[0].Observation, "prompt body") {
		t.Fatalf("trace=%+v", task.Trace)
	}
	if formatted := formatTracesForReplanner(task.Trace); strings.Contains(formatted, PromptVersionBindingTraceAction) || strings.Contains(formatted, "digest-a") {
		t.Fatalf("prompt binding leaked into replanner input: %q", formatted)
	}
}

func TestCoordinatorResumeUsesPersistedPromptVersion(t *testing.T) {
	t.Cleanup(func() { config.Reload() })
	t.Setenv("LANGFUSE_ENABLED", "true")
	t.Setenv("LANGFUSE_PUBLIC_KEY", "pk-test")
	t.Setenv("LANGFUSE_SECRET_KEY", "sk-test")
	t.Setenv("AI_AGENT_MULTIAGENT_TEAM", "software")
	t.Setenv("AI_AGENT_MULTIAGENT_RUNTIME", "legacy")
	t.Setenv("AI_AGENT_MULTIAGENT_DAG_CANARY_PERCENT", "0")

	var firstQueriesMu sync.Mutex
	var firstQueries []string
	firstServer := httptest.NewServer(promptVersionServer(7, &firstQueriesMu, &firstQueries))
	defer firstServer.Close()
	t.Setenv("LANGFUSE_BASE_URL", firstServer.URL)
	config.Reload()

	firstTask := newPromptPinTask(t)
	if err := newPromptPinCoordinator().Run(context.Background(), firstTask); err != nil {
		t.Fatal(err)
	}
	teamCfg := GetTeamsConfig()
	digest := newTeamConfigSnapshot(teamCfg.ActiveTeam, teamCfg.GetActiveTeam()).Digest
	pins := persistedPromptVersionPins(firstTask, digest)
	if len(pins) != 1 || pins[0].Name != "teams/test/planner" || pins[0].Version != 7 {
		t.Fatalf("pins=%+v trace=%+v", pins, firstTask.Trace)
	}
	firstQueriesMu.Lock()
	gotFirstQueries := append([]string(nil), firstQueries...)
	firstQueriesMu.Unlock()
	if len(gotFirstQueries) != 1 || gotFirstQueries[0] != "label=production" {
		t.Fatalf("first queries=%v", gotFirstQueries)
	}

	var secondQueriesMu sync.Mutex
	var secondQueries []string
	secondServer := httptest.NewServer(promptVersionServer(8, &secondQueriesMu, &secondQueries))
	defer secondServer.Close()
	t.Setenv("LANGFUSE_BASE_URL", secondServer.URL)
	config.Reload()
	resumedTask := newPromptPinTask(t)
	resumedTask.Trace = append([]types.StepTrace(nil), firstTask.Trace...)
	resumedTask.StepCount = firstTask.StepCount
	if err := newPromptPinCoordinator().Run(context.Background(), resumedTask); err != nil {
		t.Fatal(err)
	}
	secondQueriesMu.Lock()
	gotSecondQueries := append([]string(nil), secondQueries...)
	secondQueriesMu.Unlock()
	if len(gotSecondQueries) != 1 || gotSecondQueries[0] != "version=7" {
		t.Fatalf("resume queries=%v; expected pinned version instead of moved label", gotSecondQueries)
	}
	bindings := 0
	for _, trace := range resumedTask.Trace {
		if trace.Action == PromptVersionBindingTraceAction {
			bindings++
		}
	}
	if bindings != 1 {
		t.Fatalf("binding traces=%d trace=%+v", bindings, resumedTask.Trace)
	}
}

func promptVersionServer(labelVersion int, mu *sync.Mutex, queries *[]string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		*queries = append(*queries, r.URL.RawQuery)
		mu.Unlock()
		version := labelVersion
		if requested := r.URL.Query().Get("version"); requested == "7" {
			version = 7
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name": "teams/test/planner", "version": version, "labels": []string{"production"}, "prompt": "planner prompt",
		})
	})
}

func newPromptPinTask(t *testing.T) *types.Task {
	t.Helper()
	return &types.Task{ID: "prompt-pin", Goal: "inspect", Workspace: t.TempDir(), Status: types.StatusCreated, MaxSteps: 10, ToolBudget: 10, TokenBudget: 10000}
}

func newPromptPinCoordinator() *Coordinator {
	return &Coordinator{Planner: promptPinPlanner{}, Researcher: &recordingResearcher{}, Writer: &supportedWriter{}}
}
