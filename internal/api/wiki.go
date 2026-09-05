package api

import (
	"errors"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/wiki"
)

type wikiPageResponse struct {
	URI     string `json:"uri"`
	Space   string `json:"space"`
	Slug    string `json:"slug"`
	Title   string `json:"title,omitempty"`
	Content string `json:"content"`
}

func (h *Handler) getWikiPage(c *gin.Context) {
	if h.wikiPages == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "wiki page reader is unavailable"})
		return
	}
	cfg := config.Get()
	principal := principalFromGin(c)
	space := strings.TrimSpace(c.Param("space"))
	allowedSpaces := wikiSpacesForPrincipal(cfg, principal)
	if len(allowedSpaces) == 0 || space == "" || !allowedSpaces[space] {
		c.JSON(http.StatusForbidden, gin.H{"error": "wiki space is not available to the current tenant"})
		return
	}
	slug, err := normalizeWikiPageSlug(c.Param("slug"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid wiki page slug"})
		return
	}
	if projectID, ok := brainProjectForSpace(cfg, principal, space); ok {
		if h.brainPages == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "brain page reader is unavailable"})
			return
		}
		snapshotID := strings.TrimSpace(c.Query("brain_snapshot_id"))
		requestedProject := strings.TrimSpace(c.Query("brain_project_id"))
		if requestedProject != "" && requestedProject != projectID {
			c.JSON(http.StatusForbidden, gin.H{"error": "brain project is not authorized"})
			return
		}
		if snapshotID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "brain snapshot is required"})
			return
		}
		document, err := h.brainPages.ReadBrain(c.Request.Context(), wiki.Document{Slug: slug, URI: "wiki://" + space + "/" + slug}, space, principal.TenantID, projectID, snapshotID)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				c.JSON(http.StatusNotFound, gin.H{"error": "wiki page not found"})
				return
			}
			log.Warn("Brain page read failed", "tenant_id", principal.TenantID, "space", space)
			c.JSON(http.StatusBadGateway, gin.H{"error": "wiki page could not be read"})
			return
		}
		c.Header("Cache-Control", "private, no-store")
		c.Header("X-Content-Type-Options", "nosniff")
		c.JSON(http.StatusOK, wikiPageResponse{URI: document.URI, Space: space, Slug: slug, Title: document.Title, Content: document.Content})
		return
	}
	document, err := h.wikiPages.Read(c.Request.Context(), wiki.Document{
		Slug: slug,
		URI:  "wiki://" + space + "/" + slug,
	}, space)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			c.JSON(http.StatusNotFound, gin.H{"error": "wiki page not found"})
			return
		}
		log.Warn("Wiki page read failed", "tenant_id", principalFromGin(c).TenantID, "space", space)
		c.JSON(http.StatusBadGateway, gin.H{"error": "wiki page could not be read"})
		return
	}
	c.Header("Cache-Control", "private, no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	c.JSON(http.StatusOK, wikiPageResponse{
		URI: document.URI, Space: space, Slug: slug, Title: document.Title, Content: document.Content,
	})
}

func brainProjectForSpace(cfg *config.Config, principal Principal, space string) (string, bool) {
	if cfg == nil || !cfg.Brain.Enabled {
		return "", false
	}
	tenant, ok := cfg.API.Tenants[strings.TrimSpace(principal.TenantID)]
	if !ok {
		return "", false
	}
	var projectID string
	for id, project := range tenant.BrainProjects {
		if strings.TrimSpace(project.WikiSpace) != space {
			continue
		}
		if projectID != "" {
			return "", false
		}
		projectID = id
	}
	return projectID, projectID != ""
}

func wikiSpaceForPrincipal(cfg *config.Config, principal Principal) (string, bool) {
	if cfg == nil {
		return "", false
	}
	tenantID := strings.TrimSpace(principal.TenantID)
	if tenant, exists := cfg.API.Tenants[tenantID]; exists {
		if space := strings.TrimSpace(tenant.WikiSpace); space != "" {
			return space, true
		}
	}
	if space := strings.TrimSpace(cfg.Wiki.DefaultSpace); space != "" {
		return space, true
	}
	if tenantID == "" || tenantID == "default" {
		return "local", true
	}
	return "", false
}

func wikiSpacesForPrincipal(cfg *config.Config, principal Principal) map[string]bool {
	spaces := make(map[string]bool)
	if cfg == nil {
		return spaces
	}
	if ordinary, ok := wikiSpaceForPrincipal(cfg, principal); ok {
		spaces[ordinary] = true
	}
	if tenant, exists := cfg.API.Tenants[strings.TrimSpace(principal.TenantID)]; exists && cfg.Brain.Enabled {
		for _, project := range tenant.BrainProjects {
			if space := strings.TrimSpace(project.WikiSpace); space != "" {
				spaces[space] = true
			}
		}
	}
	return spaces
}

func normalizeWikiPageSlug(value string) (string, error) {
	value = strings.TrimSpace(strings.TrimPrefix(value, "/"))
	decoded, err := url.PathUnescape(value)
	if err != nil {
		return "", err
	}
	if decodedAgain, decodeErr := url.PathUnescape(decoded); decodeErr != nil || decodedAgain != decoded {
		return "", errors.New("wiki page slug is not canonically encoded")
	}
	value = strings.TrimSpace(strings.ReplaceAll(decoded, "\\", "/"))
	if value == "" || strings.ContainsRune(value, 0) || filepath.IsAbs(value) || path.Clean(value) != value {
		return "", errors.New("wiki page slug is not a clean relative path")
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." || strings.HasPrefix(segment, ".") {
			return "", errors.New("wiki page slug contains a forbidden segment")
		}
	}
	if extension := path.Ext(value); extension != "" {
		if !strings.EqualFold(extension, ".md") {
			return "", errors.New("wiki page must be Markdown")
		}
		value = strings.TrimSuffix(value, extension)
	}
	return value, nil
}
