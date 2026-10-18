package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/multiagent"
	"github.com/wuxujun/ai-agent/internal/orchestrator"
	"github.com/wuxujun/ai-agent/internal/promptmanager"
	"github.com/wuxujun/ai-agent/internal/store"
)

func TestInitTenantPromptsUsesAuthenticatedTenantAndAllowedTeams(t *testing.T) {
	var mu sync.Mutex
	copies := map[string]string{}
	langfuse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			var body struct{ Name, Prompt string }
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			if !strings.HasPrefix(body.Name, "tenants/") {
				t.Error("attempted to write system prompt")
				w.WriteHeader(400)
				return
			}
			copies[body.Name] = body.Prompt
			_ = json.NewEncoder(w).Encode(map[string]any{"name": body.Name, "prompt": body.Prompt, "version": 1, "labels": []string{"production"}})
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/api/public/v2/prompts/")
		content, ok := copies[name]
		if strings.HasPrefix(name, "tenants/") && !ok {
			w.WriteHeader(404)
			return
		}
		if !ok {
			content = "current source: " + name
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"name": name, "prompt": content, "version": 1, "labels": []string{"production"}})
	}))
	t.Cleanup(langfuse.Close)
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.API.APIKey = "system-key"
		cfg.API.Auth.Mode = "api_key"
		cfg.API.Tenants = map[string]config.APITenantConfig{
			"tenant-a": {APIKey: "tenant-key-a", AllowedMultiAgentTeams: []string{"wiki"}},
			"tenant-b": {APIKey: "tenant-key-b", AllowedMultiAgentTeams: []string{"software"}},
		}
		cfg.Langfuse.Enabled = true
		cfg.Langfuse.Host = langfuse.URL
		cfg.Langfuse.PublicKey = "pk-test"
		cfg.Langfuse.SecretKey = "sk-test"
		cfg.Langfuse.BootstrapTimeoutSeconds = 5
	}))
	st := store.NewMemoryStore()
	t.Cleanup(func() { _ = st.Close() })
	router := setupTestRouter(t, st, &orchestrator.Engine{})
	request := func(key, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path+"?tenant_id=tenant-b", strings.NewReader(`{"tenant_id":"tenant-b"}`))
		req.Header.Set("X-API-Key", key)
		req.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		return response
	}
	for _, tc := range []struct {
		key, path string
		want      int
	}{
		{"", "/api/tenant/prompt/init", 401},
		{"tenant-key-a", "/api/prompt/init", 403},
	} {
		if got := request(tc.key, tc.path).Code; got != tc.want {
			t.Fatalf("status=%d want=%d", got, tc.want)
		}
	}
	teamsCfg, err := multiagent.LoadTeamsConfigStrict()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ tenant, key, team string }{{"tenant-a", "tenant-key-a", "wiki"}, {"tenant-b", "tenant-key-b", "software"}} {
		seeds, err := multiagent.TeamPromptSeeds(&multiagent.TeamsConfig{Teams: map[string]multiagent.TeamConfig{tc.team: teamsCfg.Teams[tc.team]}})
		if err != nil || len(seeds) == 0 {
			t.Fatal("expected configured team prompt seeds")
		}
		for attempt := 0; attempt < 2; attempt++ {
			response := request(tc.key, "/api/tenant/prompt/init")
			var summary struct {
				TenantID                     string `json:"tenant_id"`
				Created, Existing, Processed int
			}
			if json.Unmarshal(response.Body.Bytes(), &summary) != nil || response.Code != 200 || summary.TenantID != tc.tenant || summary.Processed != len(seeds) {
				t.Fatalf("tenant init status=%d body=%s", response.Code, response.Body.String())
			}
			if (attempt == 0 && summary.Created != len(seeds)) || (attempt == 1 && summary.Existing != len(seeds)) {
				t.Fatal("initialization was not idempotent")
			}
		}
		mu.Lock()
		for _, seed := range seeds {
			name, _ := promptmanager.TenantPromptName(tc.tenant, seed.Name)
			if copies[name] != "current source: "+seed.Name {
				t.Error("tenant copy did not use the selected system content")
			}
		}
		mu.Unlock()
	}
	mu.Lock()
	defer mu.Unlock()
	if len(copies) != 4 {
		t.Fatalf("unexpected or unauthorized copies: %d", len(copies))
	}
}

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
