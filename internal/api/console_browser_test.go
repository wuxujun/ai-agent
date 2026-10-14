package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/executor"
	"github.com/wuxujun/ai-agent/internal/metrics"
	"github.com/wuxujun/ai-agent/internal/orchestrator"
	"github.com/wuxujun/ai-agent/internal/planner"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

type consoleBrowserPlanner struct{}

func (consoleBrowserPlanner) PlanNext(ctx context.Context, task *types.Task, _ func(string)) (*planner.PlanDecision, error) {
	if strings.HasPrefix(task.Goal, "browser-cancel") {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return &planner.PlanDecision{Stop: true, FinalAnswer: "Browser fixture completed"}, nil
}

// Opt-in: the ordinary Go suite needs neither Node nor a browser installation.
// CI explicitly enables this test after installing the pinned browser tooling.
func TestConsoleBrowser(t *testing.T) {
	if os.Getenv("AI_AGENT_RUN_CONSOLE_BROWSER_TESTS") != "1" {
		t.Skip("set AI_AGENT_RUN_CONSOLE_BROWSER_TESTS=1 to run Chromium regressions")
	}
	suite, err := filepath.Abs("../../tests/console")
	if err != nil {
		t.Fatal(err)
	}
	cli := filepath.Join(suite, "node_modules", "@playwright", "test", "cli.js")
	if _, err := os.Stat(cli); err != nil {
		t.Fatalf("run npm ci in tests/console first: %v", err)
	}
	teams, err := os.ReadFile(filepath.Join(suite, "..", "..", "teams.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// macOS's default TMPDIR is under /private/var, which the production
	// workspace policy correctly rejects. Keep the fixture in an allowed root.
	if runtime.GOOS == "darwin" {
		t.Setenv("TMPDIR", "/private/tmp")
	}
	root := t.TempDir()
	t.Chdir(root)
	if err := os.WriteFile("teams.yaml", teams, 0644); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(root, "tenant-a")
	if err := os.Mkdir(workspace, 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AI_AGENT_CONSOLE_SESSION_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x67}, 32)))
	introspection := testIntrospectionConfig()
	introspection.CacheTTLSeconds = 0
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.API.APIKey = ""
		cfg.API.Auth = config.APIAuthConfig{Mode: "introspection", Introspection: introspection, RequireTenantWorkspaceRoot: true}
		cfg.API.Tenants = map[string]config.APITenantConfig{
			"tenant-a": {WorkspaceRoot: workspace},
			"tenant-b": {WorkspaceRoot: filepath.Join(root, "tenant-b")},
		}
		cfg.Orchestrator.Mode = "legacy"
		cfg.Orchestrator.MaxConcurrentTasks = 2
		cfg.AnswerPipeline.Enabled = false
		cfg.Brain.Enabled = false
	}))
	installTestIntrospector(t, introspection, nil, func(req *http.Request) (int, string) {
		if req.Header.Get("Authorization") != "Bearer browser-fixture-token" {
			return http.StatusUnauthorized, `{}`
		}
		return http.StatusOK, `{"iss":"https://issuer.example","data":{"active":true,"userid":"tenant-a","aud":"ai-agent","exp":` + expirationAfter(time.Hour) + `}}`
	})
	t.Cleanup(func() {
		introspectionVerifierState.Lock()
		introspectionVerifierState.verifier = nil
		introspectionVerifierState.Unlock()
	})
	st := store.NewMemoryStore()
	for _, task := range []*types.Task{
		{ID: "foreign-task", TenantID: "tenant-b", Goal: "Foreign private task", Status: types.StatusCompleted},
		{ID: "long-trace", TenantID: "tenant-a", Goal: "Long trace fixture", Workspace: workspace, Status: types.StatusCompleted, Trace: browserTraceFixture()},
	} {
		if err := st.CreateTask(t.Context(), task); err != nil {
			t.Fatal(err)
		}
	}
	eng := &orchestrator.Engine{
		Planner: consoleBrowserPlanner{}, Executor: &executor.DefaultExecutor{},
		Store: st, Mode: orchestrator.ModeLegacy, Approvals: orchestrator.NewApprovalStore(),
	}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handler := RegisterRoutes(router, st, eng, metrics.NewCollector())
	server := httptest.NewServer(router)
	defer server.Close()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := handler.Shutdown(ctx); err != nil {
			t.Errorf("fixture shutdown: %v", err)
		}
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", cli, "test")
	cmd.Dir = suite
	cmd.Env = append(os.Environ(), "UI_BASE_URL="+server.URL, "UI_WORKSPACE="+workspace)
	output, err := cmd.CombinedOutput()
	t.Logf("Console browser regression results:\n%s", output)
	if err != nil {
		t.Fatalf("browser regressions: %v", err)
	}
}

func browserTraceFixture() []types.StepTrace {
	traces := make([]types.StepTrace, 250)
	for i := range traces {
		traces[i] = types.StepTrace{Step: i + 1, Action: "read_file", Observation: fmt.Sprintf("Trace row %03d <script>window.fixtureXSS=true</script>", i+1), AgentRole: types.AgentRoleResearcher}
		if i == 49 {
			traces[i].Error = "Fixture read failure"
		}
	}
	return traces
}
