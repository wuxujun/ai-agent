package promptmanager

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"unicode"

	"github.com/wuxujun/ai-agent/internal/config"
)

type tenantPromptScopeKey struct{}

// WithTenantScope binds prompt selection to the task's persisted tenant, never
// to an HTTP parameter or a model-provided name.
func WithTenantScope(ctx context.Context, tenantID string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, tenantPromptScopeKey{}, tenantID)
}

// TenantPromptName keeps the tenant ID readable as one path segment.
// Tenant copies have their own Langfuse versions, labels and cache keys.
func TenantPromptName(tenantID, name string) (string, error) {
	if strings.TrimSpace(tenantID) == "" || strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("tenant prompt requires a tenant and prompt name")
	}
	if tenantID != strings.TrimSpace(tenantID) || tenantID == "." || tenantID == ".." || strings.ContainsAny(tenantID, "/\\") || strings.ContainsFunc(tenantID, unicode.IsControl) {
		return "", fmt.Errorf("tenant ID must be a single path segment without surrounding whitespace or control characters")
	}
	return "tenants/" + tenantID + "/" + strings.TrimSpace(name), nil
}

// EnsureTenantTextPrompt copies the selected system prompt only when the
// tenant copy is missing. A confirmed missing system prompt may use its local
// seed; upstream failures never cause a local-content overwrite.
func (pm *PromptManager) EnsureTenantTextPrompt(ctx context.Context, tenantID string, seed Seed) (ResolvedPrompt, bool, error) {
	selector, err := seed.Selector.Normalize()
	if err != nil {
		return ResolvedPrompt{}, false, err
	}
	seed.Selector = selector
	name, err := TenantPromptName(tenantID, seed.Name)
	if err != nil {
		return ResolvedPrompt{}, false, err
	}
	resolved, err := pm.ResolveStrict(ctx, name, seed.Selector)
	if err == nil {
		return resolved, false, nil
	}
	if !IsHTTPStatus(err, http.StatusNotFound) {
		return ResolvedPrompt{}, false, err
	}
	// Fixed system versions are not tenant versions. Do not silently change
	// an operator's immutable-version configuration into a movable label.
	if seed.Selector.Version > 0 {
		return ResolvedPrompt{}, false, fmt.Errorf("tenant copy of a fixed-version prompt must already exist at the configured version")
	}
	source, err := pm.ResolveStrict(ctx, seed.Name, seed.Selector)
	if err == nil {
		seed.Content = source.Content
	} else if !IsHTTPStatus(err, http.StatusNotFound) {
		return ResolvedPrompt{}, false, err
	} else if selector.Label != "latest" {
		_, latestErr := pm.ResolveStrict(ctx, seed.Name, Selector{Label: "latest"})
		if latestErr == nil {
			return ResolvedPrompt{}, false, fmt.Errorf("system prompt is missing its configured selector")
		}
		if !IsHTTPStatus(latestErr, http.StatusNotFound) {
			return ResolvedPrompt{}, false, latestErr
		}
	}
	seed.Name = name
	return pm.EnsureTextPrompt(ctx, seed)
}

// ResolveTenantPinned prefers a tenant copy, and retains the previous global
// resolution until one exists. Persisted version pins take precedence over
// newly initialized copies so an in-flight/resumed task cannot change content.
func (pm *PromptManager) ResolveTenantPinned(ctx context.Context, name string, selector Selector, fallback string) (ResolvedPrompt, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	tenantID, _ := ctx.Value(tenantPromptScopeKey{}).(string)
	if tenantID == "" {
		return pm.ResolvePinned(ctx, name, selector, fallback)
	}
	tenantName, err := TenantPromptName(tenantID, name)
	if err != nil {
		return ResolvedPrompt{}, err
	}
	registry := versionPinRegistryFromContext(ctx)
	if _, ok := registry.Get(tenantName); ok {
		return pm.ResolvePinned(ctx, tenantName, selector, fallback)
	}
	if _, ok := registry.Get(name); ok {
		return pm.ResolvePinned(ctx, name, selector, fallback)
	}
	cfg := config.Get().Langfuse
	if !cfg.Enabled || cfg.PublicKey == "" || cfg.SecretKey == "" {
		return pm.ResolvePinned(ctx, name, selector, fallback)
	}
	resolved, err := pm.ResolveStrict(ctx, tenantName, selector)
	if err == nil {
		if err := registry.Pin(VersionPin{Name: tenantName, Version: resolved.Version, Selector: resolved.Selector, Labels: resolved.Labels}); err != nil {
			return ResolvedPrompt{}, err
		}
		return resolved, nil
	}
	if !IsHTTPStatus(err, http.StatusNotFound) {
		return ResolvedPrompt{}, err
	}
	// A missing selector on an existing copy is an error, not permission to
	// switch this tenant back to system content.
	normalized, err := selector.Normalize()
	if err != nil {
		return ResolvedPrompt{}, err
	}
	if normalized.Label != "latest" {
		_, latestErr := pm.ResolveStrict(ctx, tenantName, Selector{Label: "latest"})
		if latestErr == nil {
			return ResolvedPrompt{}, fmt.Errorf("tenant prompt is missing its configured selector")
		}
		if !IsHTTPStatus(latestErr, http.StatusNotFound) {
			return ResolvedPrompt{}, latestErr
		}
	}
	return pm.ResolvePinned(ctx, name, selector, fallback)
}
