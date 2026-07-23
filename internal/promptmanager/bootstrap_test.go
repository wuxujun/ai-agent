package promptmanager

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/config"
)

func TestEnsureTextPromptCreatesOnlyAfterConfirmedMissing(t *testing.T) {
	var getProduction, getLatest, posts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		if !ok || user != "pk-test" || password != "sk-test" {
			t.Fatal("missing Langfuse basic auth")
		}
		switch r.Method {
		case http.MethodGet:
			switch r.URL.Query().Get("label") {
			case "production":
				getProduction++
			case "latest":
				getLatest++
			default:
				t.Fatalf("unexpected selector: %s", r.URL.RawQuery)
			}
			w.WriteHeader(http.StatusNotFound)
		case http.MethodPost:
			posts++
			var body struct {
				Type   string   `json:"type"`
				Name   string   `json:"name"`
				Prompt string   `json:"prompt"`
				Labels []string `json:"labels"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.Type != "text" || body.Name != "teams/test/planner" || body.Prompt != "local seed" || len(body.Labels) != 1 || body.Labels[0] != "production" {
				t.Fatalf("unexpected create body: %+v", body)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name": "teams/test/planner", "version": 1,
				"labels": []string{"production", "latest"}, "prompt": "local seed",
			})
		default:
			t.Fatalf("unexpected method %s", r.Method)
		}
	}))
	defer server.Close()
	restore := overrideLangfuseForBootstrapTest(server.URL)
	defer restore()

	manager := &PromptManager{cache: make(map[string]cachedPrompt), client: server.Client(), ttl: time.Minute}
	resolved, created, err := manager.EnsureTextPrompt(context.Background(), Seed{
		Name: "teams/test/planner", Content: "local seed", Selector: Selector{Label: "production"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !created || resolved.Version != 1 || resolved.Content != "local seed" {
		t.Fatalf("created=%t resolved=%+v", created, resolved)
	}
	if getProduction != 1 || getLatest != 1 || posts != 1 {
		t.Fatalf("production=%d latest=%d posts=%d", getProduction, getLatest, posts)
	}

	strict, err := manager.ResolveStrict(context.Background(), "teams/test/planner", Selector{Label: "production"})
	if err != nil || strict.Version != 1 || posts != 1 {
		t.Fatalf("cached strict=%+v err=%v posts=%d", strict, err, posts)
	}
}

func TestEnsureTextPromptUsesExistingPrompt(t *testing.T) {
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts++
			t.Fatal("existing prompt must not be overwritten")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name": "teams/test/writer", "version": 8,
			"labels": []string{"production"}, "prompt": "remote content",
		})
	}))
	defer server.Close()
	restore := overrideLangfuseForBootstrapTest(server.URL)
	defer restore()

	manager := &PromptManager{cache: make(map[string]cachedPrompt), client: server.Client(), ttl: time.Minute}
	resolved, created, err := manager.EnsureTextPrompt(context.Background(), Seed{
		Name: "teams/test/writer", Content: "local fallback", Selector: Selector{},
	})
	if err != nil || created || resolved.Content != "remote content" || resolved.Version != 8 || posts != 0 {
		t.Fatalf("resolved=%+v created=%t posts=%d err=%v", resolved, created, posts, err)
	}
}

func TestEnsureTextPromptDoesNotCreateForNonMissingFailures(t *testing.T) {
	tests := []struct {
		name          string
		selector      Selector
		production    int
		latest        int
		expectedError string
	}{
		{name: "unauthorized", production: http.StatusUnauthorized, expectedError: "401"},
		{name: "server error", production: http.StatusInternalServerError, expectedError: "500"},
		{name: "fixed version missing", selector: Selector{Version: 9}, production: http.StatusNotFound, expectedError: "fixed versions cannot be bootstrapped"},
		{name: "label missing from existing prompt", selector: Selector{Label: "production"}, production: http.StatusNotFound, latest: http.StatusOK, expectedError: "label \"production\" is missing"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			posts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					posts++
					t.Fatal("failure must not create a prompt")
				}
				status := tc.production
				if r.URL.Query().Get("label") == "latest" {
					status = tc.latest
				}
				if status == http.StatusOK {
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{
						"name": "teams/test/prompt", "version": 3,
						"labels": []string{"latest"}, "prompt": "existing latest",
					})
					return
				}
				w.WriteHeader(status)
			}))
			defer server.Close()
			restore := overrideLangfuseForBootstrapTest(server.URL)
			defer restore()

			manager := &PromptManager{cache: make(map[string]cachedPrompt), client: server.Client(), ttl: time.Minute}
			_, _, err := manager.EnsureTextPrompt(context.Background(), Seed{
				Name: "teams/test/prompt", Content: "local seed", Selector: tc.selector,
			})
			if err == nil || !strings.Contains(err.Error(), tc.expectedError) || posts != 0 {
				t.Fatalf("err=%v posts=%d", err, posts)
			}
		})
	}
}

func overrideLangfuseForBootstrapTest(host string) func() {
	return config.OverrideForTesting(func(cfg *config.Config) {
		cfg.Langfuse.Enabled = true
		cfg.Langfuse.Host = host
		cfg.Langfuse.PublicKey = "pk-test"
		cfg.Langfuse.SecretKey = "sk-test"
	})
}

func TestBuildPromptCollectionURLPreservesBasePath(t *testing.T) {
	endpoint, err := buildPromptCollectionURL("https://langfuse.example.com/base/")
	if err != nil {
		t.Fatal(err)
	}
	if endpoint != "https://langfuse.example.com/base/api/public/v2/prompts" {
		t.Fatalf("endpoint=%q", endpoint)
	}
}
