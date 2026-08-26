package api

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/wiki"
)

type fakeWikiPageRuntime struct {
	document wiki.Document
	err      error
	reads    int
	space    string
	slug     string
}

func (*fakeWikiPageRuntime) Check(context.Context) error { return nil }

func (f *fakeWikiPageRuntime) Read(_ context.Context, document wiki.Document, space string) (wiki.Document, error) {
	f.reads++
	f.space = space
	f.slug = document.Slug
	if f.err != nil {
		return wiki.Document{}, f.err
	}
	result := f.document
	if result.URI == "" {
		result.URI = document.URI
	}
	return result, nil
}

func TestGetWikiPageEnforcesTenantSpaceAndReturnsMarkdown(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.API.Auth.Mode = "api_key"
		cfg.Wiki.DefaultSpace = "shared"
		cfg.API.Tenants = map[string]config.APITenantConfig{
			"tenant-a": {APIKey: "wiki-page-key", WikiSpace: "tenant-space"},
		}
	}))
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handler := RegisterRoutes(router, store.NewMemoryStore(), nil, nil)
	runtime := &fakeWikiPageRuntime{document: wiki.Document{Title: "Page", Content: "# Page\n\nEvidence."}}
	handler.SetWikiReadinessChecker(runtime)

	request := httptest.NewRequest(http.MethodGet, "/api/wiki/pages/tenant-space/concepts/example", nil)
	request.Header.Set("X-API-Key", "wiki-page-key")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	var page wikiPageResponse
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.URI != "wiki://tenant-space/concepts/example" || page.Slug != "concepts/example" || page.Content != "# Page\n\nEvidence." {
		t.Fatalf("page = %+v", page)
	}
	if runtime.reads != 1 || runtime.space != "tenant-space" || runtime.slug != "concepts/example" {
		t.Fatalf("read call = count:%d space:%q slug:%q", runtime.reads, runtime.space, runtime.slug)
	}
	if response.Header().Get("Cache-Control") != "private, no-store" || response.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("security headers = %+v", response.Header())
	}

	request = httptest.NewRequest(http.MethodGet, "/api/wiki/pages/shared/concepts/example", nil)
	request.Header.Set("X-API-Key", "wiki-page-key")
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || runtime.reads != 1 {
		t.Fatalf("cross-space response = %d %s, reads=%d", response.Code, response.Body.String(), runtime.reads)
	}
}

func TestGetWikiPageRejectsUnsafeSlugsBeforeRead(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.API.Auth.Mode = "api_key"
		cfg.API.APIKey = "wiki-admin-key"
		cfg.Wiki.DefaultSpace = "local"
	}))
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handler := RegisterRoutes(router, store.NewMemoryStore(), nil, nil)
	runtime := &fakeWikiPageRuntime{}
	handler.SetWikiReadinessChecker(runtime)

	for _, requestPath := range []string{
		"/api/wiki/pages/local/concepts/private.json",
		"/api/wiki/pages/local/concepts/%252e%252e%252fsecret",
		"/api/wiki/pages/local/.hidden/page",
	} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, requestPath, nil)
		request.Header.Set("X-API-Key", "wiki-admin-key")
		router.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Errorf("path %q status = %d: %s", requestPath, response.Code, response.Body.String())
		}
	}
	if runtime.reads != 0 {
		t.Fatalf("unsafe requests reached reader %d time(s)", runtime.reads)
	}
}

func TestGetWikiPageMapsUnavailableAndMissingPages(t *testing.T) {
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.API.Auth.Mode = "api_key"
		cfg.API.APIKey = "wiki-admin-key"
		cfg.Wiki.DefaultSpace = "local"
	}))
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handler := RegisterRoutes(router, store.NewMemoryStore(), nil, nil)

	request := httptest.NewRequest(http.MethodGet, "/api/wiki/pages/local/concepts/missing", nil)
	request.Header.Set("X-API-Key", "wiki-admin-key")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("unavailable status = %d: %s", response.Code, response.Body.String())
	}

	handler.SetWikiReadinessChecker(&fakeWikiPageRuntime{err: fs.ErrNotExist})
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), "not found") {
		t.Fatalf("missing status = %d: %s", response.Code, response.Body.String())
	}
}
