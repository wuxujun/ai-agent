package promptmanager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/wuxujun/ai-agent/internal/config"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// HTTPStatusError preserves the response status needed to distinguish a
// missing prompt from authentication, rate-limit, and server failures.
type HTTPStatusError struct {
	Method     string
	URL        string
	StatusCode int
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("Langfuse %s %s returned http status %d", e.Method, e.URL, e.StatusCode)
}

func IsHTTPStatus(err error, status int) bool {
	var statusErr *HTTPStatusError
	return errors.As(err, &statusErr) && statusErr.StatusCode == status
}

// Seed describes one teams.yaml text prompt that should already exist in
// Langfuse or may be created from local fallback content.
type Seed struct {
	Name     string
	Content  string
	Selector Selector
}

// EnsureTextPrompt fetches and caches a prompt. It creates a first text-prompt
// version only when both the requested selector and the latest selector prove
// that the prompt name does not exist. Existing prompts are never overwritten.
// The bool result reports whether this call created the prompt.
func (pm *PromptManager) EnsureTextPrompt(ctx context.Context, seed Seed) (ResolvedPrompt, bool, error) {
	pm.ensureMu.Lock()
	defer pm.ensureMu.Unlock()

	cfg := config.Get().Langfuse
	if !cfg.Enabled {
		return ResolvedPrompt{}, false, fmt.Errorf("Langfuse prompt management is disabled")
	}
	if strings.TrimSpace(cfg.PublicKey) == "" || strings.TrimSpace(cfg.SecretKey) == "" {
		return ResolvedPrompt{}, false, fmt.Errorf("Langfuse credentials are not configured")
	}
	name := strings.TrimSpace(seed.Name)
	if name == "" {
		return ResolvedPrompt{}, false, fmt.Errorf("Langfuse prompt name is empty")
	}
	selector, err := seed.Selector.Normalize()
	if err != nil {
		return ResolvedPrompt{}, false, err
	}

	resolved, err := pm.fetchFromLangfuse(ctx, name, selector, cfg.Host, cfg.PublicKey, cfg.SecretKey)
	if err == nil {
		if err := validateBootstrapResolution(resolved, name, selector); err != nil {
			return ResolvedPrompt{}, false, err
		}
		pm.cacheResolved(cfg.Host, cfg.PublicKey, selector, resolved)
		recordPromptResolution(ctx, resolved, "bootstrap_existing")
		return resolved, false, nil
	}
	if !IsHTTPStatus(err, http.StatusNotFound) {
		return ResolvedPrompt{}, false, err
	}
	if selector.Version > 0 {
		return ResolvedPrompt{}, false, fmt.Errorf("Langfuse prompt %q version %d does not exist; fixed versions cannot be bootstrapped", name, selector.Version)
	}

	// A label can be absent even though the prompt already has other versions.
	// Never create a new local-content version in that case.
	if selector.Label != "latest" {
		_, latestErr := pm.fetchFromLangfuse(ctx, name, Selector{Label: "latest"}, cfg.Host, cfg.PublicKey, cfg.SecretKey)
		switch {
		case latestErr == nil:
			return ResolvedPrompt{}, false, fmt.Errorf("Langfuse prompt %q exists but label %q is missing", name, selector.Label)
		case !IsHTTPStatus(latestErr, http.StatusNotFound):
			return ResolvedPrompt{}, false, fmt.Errorf("check latest Langfuse prompt %q: %w", name, latestErr)
		}
	}

	content := strings.TrimSpace(seed.Content)
	if content == "" {
		return ResolvedPrompt{}, false, fmt.Errorf("Langfuse prompt %q is missing and has no local system_prompt seed", name)
	}
	labels := []string(nil)
	if selector.Label != "latest" {
		labels = []string{selector.Label}
	}
	resolved, err = pm.createTextPrompt(ctx, name, content, labels, cfg.Host, cfg.PublicKey, cfg.SecretKey)
	if err != nil {
		return ResolvedPrompt{}, false, err
	}
	resolved.Selector = selector
	pm.cacheResolved(cfg.Host, cfg.PublicKey, selector, resolved)
	recordPromptResolution(ctx, resolved, "bootstrap_created")
	return resolved, true, nil
}

func validateBootstrapResolution(resolved ResolvedPrompt, name string, selector Selector) error {
	if resolved.Name != name {
		return fmt.Errorf("Langfuse prompt response name %q does not match requested name %q", resolved.Name, name)
	}
	if resolved.Version <= 0 {
		return fmt.Errorf("Langfuse prompt %q response has no positive version", name)
	}
	if selector.Version > 0 && resolved.Version != selector.Version {
		return fmt.Errorf("Langfuse prompt %q returned version %d for requested version %d", name, resolved.Version, selector.Version)
	}
	return nil
}

func (pm *PromptManager) createTextPrompt(ctx context.Context, name, content string, labels []string, host, pubKey, secKey string) (resolved ResolvedPrompt, resultErr error) {
	endpoint, err := buildPromptCollectionURL(host)
	if err != nil {
		return ResolvedPrompt{}, err
	}
	started := time.Now()
	ctx, span := promptTracer(ctx).Start(ctx, "langfuse.prompt.create", trace.WithAttributes(
		attribute.String("langfuse.observation.metadata.prompt_name", strings.TrimSpace(name)),
		attribute.StringSlice("langfuse.observation.metadata.prompt_labels", labels),
		attribute.String("http.request.method", http.MethodPost),
	))
	statusCode := 0
	defer func() {
		span.SetAttributes(
			attribute.Int("http.response.status_code", statusCode),
			attribute.Int64("langfuse.observation.metadata.duration_ms", time.Since(started).Milliseconds()),
		)
		if resultErr != nil {
			span.RecordError(resultErr)
			span.SetStatus(codes.Error, "prompt create failed")
		}
		span.End()
	}()
	body, err := json.Marshal(struct {
		Type   string   `json:"type"`
		Name   string   `json:"name"`
		Prompt string   `json:"prompt"`
		Labels []string `json:"labels,omitempty"`
	}{
		Type: "text", Name: name, Prompt: content, Labels: labels,
	})
	if err != nil {
		return ResolvedPrompt{}, fmt.Errorf("encode Langfuse prompt %q: %w", name, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return ResolvedPrompt{}, err
	}
	req.SetBasicAuth(pubKey, secKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := pm.client.Do(req)
	if err != nil {
		return ResolvedPrompt{}, err
	}
	defer resp.Body.Close()
	statusCode = resp.StatusCode
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return ResolvedPrompt{}, &HTTPStatusError{Method: http.MethodPost, URL: endpoint, StatusCode: resp.StatusCode}
	}
	var data PromptResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return ResolvedPrompt{}, fmt.Errorf("decode created Langfuse prompt %q: %w", name, err)
	}
	prompt, err := renderPromptContent(data.Prompt)
	if err != nil {
		return ResolvedPrompt{}, err
	}
	resolvedName := strings.TrimSpace(data.Name)
	if resolvedName == "" {
		resolvedName = name
	}
	if resolvedName != name {
		return ResolvedPrompt{}, fmt.Errorf("created Langfuse prompt response name %q does not match requested name %q", resolvedName, name)
	}
	if data.Version <= 0 {
		return ResolvedPrompt{}, fmt.Errorf("created Langfuse prompt %q response has no positive version", name)
	}
	return ResolvedPrompt{
		Name: name, Version: data.Version, Labels: append([]string(nil), data.Labels...),
		Source: "langfuse", Content: prompt,
	}, nil
}

func (pm *PromptManager) cacheResolved(host, pubKey string, selector Selector, resolved ResolvedPrompt) {
	key := promptCacheKey(host, pubKey, resolved.Name, selector)
	pm.mu.Lock()
	pm.cache[key] = cachedPrompt{resolved: cloneResolvedPrompt(resolved), expiredAt: time.Now().Add(pm.ttl)}
	pm.mu.Unlock()
}

func buildPromptCollectionURL(host string) (string, error) {
	if host == "" {
		host = "https://cloud.langfuse.com"
	}
	base, err := url.Parse(host)
	if err != nil {
		return "", fmt.Errorf("invalid Langfuse host: %w", err)
	}
	if base.Scheme == "" || base.Host == "" {
		return "", fmt.Errorf("invalid Langfuse host %q", host)
	}
	basePath := strings.TrimRight(base.Path, "/")
	baseEscapedPath := strings.TrimRight(base.EscapedPath(), "/")
	base.Path = basePath + "/api/public/v2/prompts"
	base.RawPath = baseEscapedPath + "/api/public/v2/prompts"
	return base.String(), nil
}
