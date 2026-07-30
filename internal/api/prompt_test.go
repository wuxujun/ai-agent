package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/multiagent"
	"github.com/wuxujun/ai-agent/internal/orchestrator"
	"github.com/wuxujun/ai-agent/internal/store"
)

func TestInitPromptsSynchronizesRepositoryTeamPrompts(t *testing.T) {
	var requests atomic.Int32
	langfuse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		publicKey, secretKey, ok := r.BasicAuth()
		if !ok || publicKey != "pk-test" || secretKey != "sk-test" {
			t.Fatalf("unexpected Langfuse credentials")
		}
		if r.Method != http.MethodGet {
			t.Fatalf("existing prompts must not be overwritten; method=%s", r.Method)
		}
		requests.Add(1)
		name := strings.TrimPrefix(r.URL.Path, "/api/public/v2/prompts/")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name":    name,
			"version": 7,
			"labels":  []string{"production", "latest"},
			"prompt":  "remote prompt",
		})
	}))
	defer langfuse.Close()

	restore := config.OverrideForTesting(func(cfg *config.Config) {
		cfg.API.APIKey = ""
		cfg.API.Tenants = nil
		cfg.Langfuse.Enabled = true
		cfg.Langfuse.Host = langfuse.URL
		cfg.Langfuse.PublicKey = "pk-test"
		cfg.Langfuse.SecretKey = "sk-test"
		cfg.Langfuse.BootstrapTimeoutSeconds = 5
	})
	t.Cleanup(restore)

	teamsCfg, err := multiagent.LoadTeamsConfigStrict()
	if err != nil {
		t.Fatal(err)
	}
	seeds, err := multiagent.TeamPromptSeeds(teamsCfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(seeds) == 0 {
		t.Fatal("repository teams.yaml must declare prompt seeds")
	}

	r := setupTestRouter(t, store.NewMemoryStore(), &orchestrator.Engine{})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/prompt/init", nil)
	r.ServeHTTP(w, req)

	var response struct {
		Status    string `json:"status"`
		Existing  int    `json:"existing"`
		Created   int    `json:"created"`
		Processed int    `json:"processed"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v: %s", err, w.Body.String())
	}
	if w.Code != http.StatusOK || response.Status != "ok" ||
		response.Existing != len(seeds) || response.Created != 0 || response.Processed != len(seeds) {
		t.Fatalf("status=%d response=%+v body=%s", w.Code, response, w.Body.String())
	}
	if requests.Load() != int32(len(seeds)) {
		t.Fatalf("Langfuse requests=%d, want %d", requests.Load(), len(seeds))
	}
}

func TestInitPromptsRequiresEnabledLangfuse(t *testing.T) {
	restore := config.OverrideForTesting(func(cfg *config.Config) {
		cfg.API.APIKey = ""
		cfg.API.Tenants = nil
		cfg.Langfuse.Enabled = false
	})
	t.Cleanup(restore)

	r := setupTestRouter(t, store.NewMemoryStore(), &orchestrator.Engine{})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/prompt/init", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "disabled") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}
