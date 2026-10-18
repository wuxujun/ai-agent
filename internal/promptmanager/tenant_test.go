package promptmanager

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func tenantTestManager(t *testing.T, handler http.HandlerFunc) *PromptManager {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	t.Cleanup(overrideLangfuseForBootstrapTest(server.URL))
	return &PromptManager{cache: make(map[string]cachedPrompt), client: server.Client(), ttl: time.Minute}
}

func writeTenantTestPrompt(w http.ResponseWriter, name, content string, version int) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"name": name, "prompt": content, "version": version, "labels": []string{"production", "latest"}})
}

func TestTenantPromptNameUsesPlainTenantID(t *testing.T) {
	for _, tenantID := range []string{"tenant-a", "tenant_a", "12345", "租户甲"} {
		got, err := TenantPromptName(tenantID, " teams/test/planner ")
		if err != nil || got != "tenants/"+tenantID+"/teams/test/planner" {
			t.Fatalf("plain tenant name = %q, err = %v", got, err)
		}
	}
	for _, tenantID := range []string{"", " ", "tenant/a", "tenant\\a", ".", "..", " tenant-a", "tenant-a ", "tenant\na"} {
		if _, err := TenantPromptName(tenantID, "teams/test/planner"); err == nil {
			t.Fatalf("invalid tenant ID accepted: %q", tenantID)
		}
	}
	if _, err := TenantPromptName("tenant-a", " "); err == nil {
		t.Fatal("empty prompt name accepted")
	}
}

func TestEnsureTenantTextPromptIsolatedIdempotentCopies(t *testing.T) {
	var mu sync.Mutex
	prompts := map[string]string{"teams/test/planner": "current system content"}
	posts := 0
	manager := tenantTestManager(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodPost {
			var body struct{ Name, Prompt string }
			if json.NewDecoder(r.Body).Decode(&body) != nil || !strings.HasPrefix(body.Name, "tenants/") {
				t.Error("invalid tenant prompt create request")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			posts++
			prompts[body.Name] = body.Prompt
			writeTenantTestPrompt(w, body.Name, body.Prompt, 1)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/api/public/v2/prompts/")
		if content, ok := prompts[name]; ok {
			writeTenantTestPrompt(w, name, content, 1)
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	})
	seed := Seed{Name: "teams/test/planner", Content: "outdated local content", Selector: Selector{Label: "production"}}
	for _, tenant := range []string{"tenant_a", "tenant-a"} {
		name, err := TenantPromptName(tenant, seed.Name)
		if err != nil {
			t.Fatal(err)
		}
		for attempt := 0; attempt < 2; attempt++ {
			resolved, created, err := manager.EnsureTenantTextPrompt(t.Context(), tenant, seed)
			if err != nil || created != (attempt == 0) || resolved.Name != name || resolved.Content != "current system content" {
				t.Fatalf("tenant copy attempt %d: created=%t, err=%v", attempt, created, err)
			}
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if posts != 2 || len(prompts) != 3 || prompts[seed.Name] != "current system content" {
		t.Fatal("tenant initialization modified system content or created duplicate versions")
	}
}

func TestEnsureTenantTextPromptDoesNotWriteOnUpstreamErrors(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		tenantStatus, systemStatus int
		fixed                      bool
	}{
		{"tenant unauthorized", 401, 200, false},
		{"tenant unavailable", 503, 200, false},
		{"source unavailable", 404, 503, false},
		{"fixed source version", 404, 200, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var writes atomic.Int32
			manager := tenantTestManager(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					writes.Add(1)
					w.WriteHeader(500)
					return
				}
				if strings.Contains(r.URL.Path, "/tenants/") {
					w.WriteHeader(tc.tenantStatus)
				} else {
					w.WriteHeader(tc.systemStatus)
				}
			})
			selector := Selector{Label: "production"}
			if tc.fixed {
				selector = Selector{Version: 7}
			}
			_, _, err := manager.EnsureTenantTextPrompt(t.Context(), "tenant-a", Seed{Name: "teams/test/planner", Content: "local", Selector: selector})
			if err == nil || writes.Load() != 0 {
				t.Fatal("upstream failure was accepted or caused writes")
			}
		})
	}
}

func TestEnsureTenantTextPromptMissingSystemSeedAndLabel(t *testing.T) {
	for _, sourceExists := range []bool{false, true} {
		t.Run(map[bool]string{false: "local seed", true: "missing system label"}[sourceExists], func(t *testing.T) {
			var writes atomic.Int32
			manager := tenantTestManager(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					writes.Add(1)
					var body struct{ Name, Prompt string }
					_ = json.NewDecoder(r.Body).Decode(&body)
					writeTenantTestPrompt(w, body.Name, body.Prompt, 1)
					return
				}
				if sourceExists && strings.Contains(r.URL.Path, "/teams/test/planner") && !strings.Contains(r.URL.Path, "/tenants/") && r.URL.Query().Get("label") == "latest" {
					writeTenantTestPrompt(w, "teams/test/planner", "draft system content", 1)
					return
				}
				w.WriteHeader(404)
			})
			resolved, _, err := manager.EnsureTenantTextPrompt(t.Context(), "tenant-a", Seed{Name: "teams/test/planner", Content: "local seed"})
			if sourceExists {
				if err == nil || writes.Load() != 0 {
					t.Fatal("missing system label caused local-content creation")
				}
			} else if err != nil || writes.Load() != 1 || resolved.Content != "local seed" {
				t.Fatalf("local seed copy: %v", err)
			}
		})
	}
}

func TestResolveTenantPinnedIsolationAndResume(t *testing.T) {
	const system = "teams/test/planner"
	tenantName, _ := TenantPromptName("tenant-a", system)
	var unavailable atomic.Bool
	manager := tenantTestManager(t, func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/api/public/v2/prompts/")
		switch name {
		case tenantName:
			if unavailable.Load() {
				w.WriteHeader(503)
				return
			}
			writeTenantTestPrompt(w, name, "tenant custom content", 2)
		case system:
			writeTenantTestPrompt(w, name, "system content", 7)
		default:
			w.WriteHeader(404)
		}
	})
	registry, _ := NewVersionPinRegistry(nil, nil)
	ctx := WithTenantScope(WithVersionPinRegistry(t.Context(), registry), "tenant-a")
	resolved, err := manager.ResolveTenantPinned(ctx, system, Selector{}, "local")
	if err != nil || resolved.Name != tenantName || resolved.Content != "tenant custom content" {
		t.Fatalf("tenant selection: %v", err)
	}
	if pin, ok := registry.Get(tenantName); !ok || pin.Version != 2 {
		t.Fatal("tenant version was not pinned")
	}
	// Another tenant cannot see A's copy, and old tasks keep their system pin
	// even after a tenant copy becomes available.
	for _, tenant := range []string{"tenant-b", "tenant-a"} {
		initial := []VersionPin(nil)
		if tenant == "tenant-a" {
			initial = []VersionPin{{Name: system, Version: 7, Selector: Selector{Label: "production"}}}
		}
		pins, _ := NewVersionPinRegistry(initial, nil)
		other := WithTenantScope(WithVersionPinRegistry(t.Context(), pins), tenant)
		got, err := manager.ResolveTenantPinned(other, system, Selector{}, "local")
		if err != nil || got.Name != system || got.Content != "system content" {
			t.Fatalf("system fallback/resume: %v", err)
		}
	}
	unavailable.Store(true)
	// Version 2 has a different cache key from the initial production lookup.
	if _, err := manager.ResolveTenantPinned(ctx, system, Selector{}, "local must not run"); err == nil {
		t.Fatal("pinned tenant failure fell back")
	}
}

func TestResolveTenantPinnedRejectsTenantFailures(t *testing.T) {
	for _, status := range []int{401, 429, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			manager := tenantTestManager(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) })
			_, err := manager.ResolveTenantPinned(WithTenantScope(t.Context(), "tenant-a"), "teams/test/planner", Selector{}, "local")
			if err == nil {
				t.Fatal("tenant failure silently used system or local content")
			}
		})
	}
}

func TestResolveTenantPinnedRejectsMissingLabelOnExistingCopy(t *testing.T) {
	manager := tenantTestManager(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("label") != "latest" {
			w.WriteHeader(404)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/api/public/v2/prompts/")
		writeTenantTestPrompt(w, name, "tenant draft", 2)
	})
	_, err := manager.ResolveTenantPinned(WithTenantScope(t.Context(), "tenant-a"), "teams/test/planner", Selector{}, "local")
	if err == nil {
		t.Fatal("missing tenant production label fell back to system content")
	}
}

// Opt-in real HTTP proof against the checked-in Postman Langfuse simulation.
func TestTenantPromptSimulation(t *testing.T) {
	host := os.Getenv("AI_AGENT_TEST_LANGFUSE_SIM_URL")
	if host == "" {
		t.Skip("set AI_AGENT_TEST_LANGFUSE_SIM_URL for local Postman simulation")
	}
	t.Cleanup(overrideLangfuseForBootstrapTest(host))
	manager := &PromptManager{cache: make(map[string]cachedPrompt), client: &http.Client{Timeout: 5 * time.Second}, ttl: time.Minute}
	seed := Seed{Name: "teams/test/planner", Content: "local", Selector: Selector{Label: "production"}}
	resolved, _, err := manager.EnsureTenantTextPrompt(t.Context(), "simulation-a", seed)
	if err != nil || resolved.Content != "simulation system prompt" {
		t.Fatalf("simulation bootstrap: %v", err)
	}
	manager = &PromptManager{cache: make(map[string]cachedPrompt), client: manager.client, ttl: time.Minute}
	ctx := WithTenantScope(context.Background(), "simulation-a")
	got, err := manager.ResolveTenantPinned(ctx, seed.Name, seed.Selector, "fallback")
	if err != nil || got.Name != resolved.Name || got.Content != resolved.Content {
		t.Fatalf("simulation resolution: %v", err)
	}
}
