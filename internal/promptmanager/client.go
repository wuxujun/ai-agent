package promptmanager

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/logger"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

var log = logger.Component("promptmanager")

const promptResolutionSpanName = "agent.prompt.resolve"

type PromptResponse struct {
	Name    string          `json:"name"`
	Version int             `json:"version"`
	Labels  []string        `json:"labels"`
	Prompt  json.RawMessage `json:"prompt"`
}

type Selector struct {
	Label   string `json:"label,omitempty"`
	Version int    `json:"version,omitempty"`
}

type ResolvedPrompt struct {
	Name     string   `json:"name"`
	Version  int      `json:"version"`
	Labels   []string `json:"labels,omitempty"`
	Selector Selector `json:"selector"`
	Source   string   `json:"source"`
	Content  string   `json:"-"`
}

func (s Selector) Normalize() (Selector, error) {
	s.Label = strings.TrimSpace(s.Label)
	if s.Version < 0 {
		return Selector{}, fmt.Errorf("Langfuse prompt version must be greater than zero")
	}
	if s.Version > 0 && s.Label != "" {
		return Selector{}, fmt.Errorf("Langfuse prompt label and version are mutually exclusive")
	}
	if s.Version == 0 && s.Label == "" {
		s.Label = "production"
	}
	return s, nil
}

func (s Selector) String() string {
	normalized, err := s.Normalize()
	if err != nil {
		return "invalid"
	}
	if normalized.Version > 0 {
		return fmt.Sprintf("version:%d", normalized.Version)
	}
	return "label:" + normalized.Label
}

type cachedPrompt struct {
	resolved  ResolvedPrompt
	expiredAt time.Time
}

type PromptManager struct {
	mu       sync.RWMutex
	ensureMu sync.Mutex
	cache    map[string]cachedPrompt
	client   *http.Client
	ttl      time.Duration
}

var (
	instance *PromptManager
	once     sync.Once
)

func GetManager() *PromptManager {
	once.Do(func() {
		instance = &PromptManager{
			cache:  make(map[string]cachedPrompt),
			client: &http.Client{Timeout: 3 * time.Second}, // Strict timeout to prevent latency amplification
			ttl:    10 * time.Minute,                       // 10 minutes TTL
		}
	})
	return instance
}

// Get fetches the prompt from Langfuse with automatic caching, expiration, and fallback
func (pm *PromptManager) Get(ctx context.Context, name string, fallback string) string {
	return pm.GetWithSelector(ctx, name, Selector{}, fallback)
}

// GetWithSelector fetches a prompt by an immutable version or a movable label.
// The zero selector preserves the existing production-label behavior.
func (pm *PromptManager) GetWithSelector(ctx context.Context, name string, selector Selector, fallback string) string {
	return pm.Resolve(ctx, name, selector, fallback).Content
}

// Resolve returns prompt content together with non-sensitive resolution
// metadata. It preserves the production fallback behavior of Get.
func (pm *PromptManager) Resolve(ctx context.Context, name string, selector Selector, fallback string) ResolvedPrompt {
	cfg := config.Get().Langfuse
	normalized, selectorErr := selector.Normalize()
	if !cfg.Enabled || cfg.PublicKey == "" || cfg.SecretKey == "" {
		resolved := fallbackPrompt(name, normalized, fallback)
		recordPromptResolution(ctx, resolved, "disabled_or_unconfigured")
		return resolved
	}
	if selectorErr != nil {
		log.Error("invalid Langfuse prompt selector; using fallback", "name", name, "error", selectorErr)
		resolved := fallbackPrompt(name, Selector{}, fallback)
		recordPromptResolution(ctx, resolved, "invalid_selector")
		return resolved
	}

	cacheKey := promptCacheKey(cfg.Host, cfg.PublicKey, name, normalized)

	// 1. Thread-safe read check of cache
	pm.mu.RLock()
	cached, exists := pm.cache[cacheKey]
	pm.mu.RUnlock()

	if exists && time.Now().Before(cached.expiredAt) {
		resolved := cloneResolvedPrompt(cached.resolved)
		recordPromptResolution(ctx, resolved, "cache_hit")
		return resolved
	}

	// 2. Cache miss or expired: fetch from Langfuse
	log.Info("fetching prompt from Langfuse", "name", name, "selector", normalized.String())
	resolved, err := pm.fetchFromLangfuse(ctx, name, normalized, cfg.Host, cfg.PublicKey, cfg.SecretKey)
	if err != nil {
		log.Error("failed to fetch prompt from Langfuse; using fallback", "name", name, "error", err)
		// On failure, write a short-lived fallback cache (e.g. 1 minute) to avoid hammering Langfuse on every subsequent request
		pm.mu.Lock()
		resolved = fallbackPrompt(name, normalized, fallback)
		pm.cache[cacheKey] = cachedPrompt{resolved: resolved, expiredAt: time.Now().Add(1 * time.Minute)}
		pm.mu.Unlock()
		recordPromptResolution(ctx, resolved, "fetch_error")
		return resolved
	}

	// 3. Update cache with long-lived TTL
	pm.mu.Lock()
	pm.cache[cacheKey] = cachedPrompt{resolved: cloneResolvedPrompt(resolved), expiredAt: time.Now().Add(pm.ttl)}
	pm.mu.Unlock()
	recordPromptResolution(ctx, resolved, "fetched")
	return resolved
}

// ResolvePinned uses the task-scoped registry, when present, to turn a movable
// label into an immutable version after the first successful remote resolution.
// Once pinned, remote failure is returned instead of falling back to different
// prompt content.
func (pm *PromptManager) ResolvePinned(ctx context.Context, name string, selector Selector, fallback string) (ResolvedPrompt, error) {
	registry := versionPinRegistryFromContext(ctx)
	if registry == nil {
		return pm.Resolve(ctx, name, selector, fallback), nil
	}
	name = strings.TrimSpace(name)
	requested, err := selector.Normalize()
	if err != nil {
		return ResolvedPrompt{}, err
	}
	if pin, ok := registry.Get(name); ok {
		if requested.Version > 0 && requested.Version != pin.Version {
			return ResolvedPrompt{}, fmt.Errorf("prompt %q is pinned to version %d, configured version is %d", name, pin.Version, requested.Version)
		}
		if requested.Version == 0 && pin.Selector.Version == 0 && requested.Label != pin.Selector.Label {
			return ResolvedPrompt{}, fmt.Errorf("prompt %q is pinned from label %q, configured label is %q", name, pin.Selector.Label, requested.Label)
		}
		return pm.ResolveStrict(ctx, name, Selector{Version: pin.Version})
	}
	resolved := pm.Resolve(ctx, name, requested, fallback)
	if resolved.Source != "langfuse" {
		return resolved, nil
	}
	if resolved.Version <= 0 {
		return ResolvedPrompt{}, fmt.Errorf("Langfuse prompt %q response has no positive version to pin", name)
	}
	if resolved.Name != name {
		return ResolvedPrompt{}, fmt.Errorf("Langfuse prompt response name %q does not match requested name %q", resolved.Name, name)
	}
	if requested.Version > 0 && resolved.Version != requested.Version {
		return ResolvedPrompt{}, fmt.Errorf("Langfuse prompt %q returned version %d for requested version %d", name, resolved.Version, requested.Version)
	}
	if err := registry.Pin(VersionPin{Name: name, Version: resolved.Version, Selector: requested, Labels: resolved.Labels}); err != nil {
		return ResolvedPrompt{}, err
	}
	return resolved, nil
}

// GetStrict never falls back to local content. It is intended for evaluations
// and release gates where testing a fallback under the candidate's name would
// produce a false comparison.
func (pm *PromptManager) GetStrict(ctx context.Context, name string, selector Selector) (string, error) {
	resolved, err := pm.ResolveStrict(ctx, name, selector)
	return resolved.Content, err
}

// ResolveStrict returns remote resolution metadata and never accepts fallback
// content, including fallback entries written into the shared cache.
func (pm *PromptManager) ResolveStrict(ctx context.Context, name string, selector Selector) (ResolvedPrompt, error) {
	cfg := config.Get().Langfuse
	if !cfg.Enabled {
		recordPromptResolutionError(ctx, name, selector, "disabled")
		return ResolvedPrompt{}, fmt.Errorf("Langfuse prompt management is disabled")
	}
	if cfg.PublicKey == "" || cfg.SecretKey == "" {
		recordPromptResolutionError(ctx, name, selector, "credentials_missing")
		return ResolvedPrompt{}, fmt.Errorf("Langfuse credentials are not configured")
	}
	normalized, err := selector.Normalize()
	if err != nil {
		recordPromptResolutionError(ctx, name, selector, "invalid_selector")
		return ResolvedPrompt{}, err
	}
	cacheKey := promptCacheKey(cfg.Host, cfg.PublicKey, name, normalized)
	pm.mu.RLock()
	cached, exists := pm.cache[cacheKey]
	pm.mu.RUnlock()
	if exists && cached.resolved.Source == "langfuse" && cached.resolved.Version > 0 && time.Now().Before(cached.expiredAt) {
		resolved := cloneResolvedPrompt(cached.resolved)
		recordPromptResolution(ctx, resolved, "strict_cache_hit")
		return resolved, nil
	}
	resolved, err := pm.fetchFromLangfuse(ctx, name, normalized, cfg.Host, cfg.PublicKey, cfg.SecretKey)
	if err != nil {
		recordPromptResolutionError(ctx, name, normalized, "fetch_error")
		return ResolvedPrompt{}, err
	}
	if resolved.Version <= 0 {
		recordPromptResolutionError(ctx, name, normalized, "version_missing")
		return ResolvedPrompt{}, fmt.Errorf("Langfuse prompt response has no positive version")
	}
	if normalized.Version > 0 && resolved.Version != normalized.Version {
		recordPromptResolutionError(ctx, name, normalized, "version_mismatch")
		return ResolvedPrompt{}, fmt.Errorf("Langfuse prompt response version %d does not match requested version %d", resolved.Version, normalized.Version)
	}
	if resolved.Name != strings.TrimSpace(name) {
		recordPromptResolutionError(ctx, name, normalized, "name_mismatch")
		return ResolvedPrompt{}, fmt.Errorf("Langfuse prompt response name %q does not match requested name %q", resolved.Name, strings.TrimSpace(name))
	}
	pm.mu.Lock()
	pm.cache[cacheKey] = cachedPrompt{resolved: cloneResolvedPrompt(resolved), expiredAt: time.Now().Add(pm.ttl)}
	pm.mu.Unlock()
	recordPromptResolution(ctx, resolved, "strict_fetched")
	return resolved, nil
}

func (pm *PromptManager) fetchFromLangfuse(ctx context.Context, name string, selector Selector, host, pubKey, secKey string) (ResolvedPrompt, error) {
	if host == "" {
		host = "https://cloud.langfuse.com"
	}

	endpoint, err := buildPromptURL(host, name, selector)
	if err != nil {
		return ResolvedPrompt{}, err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return ResolvedPrompt{}, err
	}

	req.SetBasicAuth(pubKey, secKey)
	req.Header.Set("Accept", "application/json")

	resp, err := pm.client.Do(req)
	if err != nil {
		return ResolvedPrompt{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return ResolvedPrompt{}, &HTTPStatusError{Method: http.MethodGet, URL: endpoint, StatusCode: resp.StatusCode}
	}

	var data PromptResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return ResolvedPrompt{}, err
	}

	content, err := renderPromptContent(data.Prompt)
	if err != nil {
		return ResolvedPrompt{}, err
	}
	resolvedName := strings.TrimSpace(data.Name)
	if resolvedName == "" {
		resolvedName = strings.TrimSpace(name)
	}
	return ResolvedPrompt{
		Name: resolvedName, Version: data.Version, Labels: append([]string(nil), data.Labels...),
		Selector: selector, Source: "langfuse", Content: content,
	}, nil
}

func fallbackPrompt(name string, selector Selector, content string) ResolvedPrompt {
	return ResolvedPrompt{Name: strings.TrimSpace(name), Selector: selector, Source: "fallback", Content: content}
}

func cloneResolvedPrompt(source ResolvedPrompt) ResolvedPrompt {
	source.Labels = append([]string(nil), source.Labels...)
	return source
}

func recordPromptResolution(ctx context.Context, resolved ResolvedPrompt, outcome string) {
	if ctx == nil {
		return
	}
	attrs := []attribute.KeyValue{
		attribute.String("agent.prompt.name", resolved.Name),
		attribute.String("agent.prompt.selector", resolved.Selector.String()),
		attribute.String("agent.prompt.source", resolved.Source),
		attribute.String("agent.prompt.outcome", outcome),
		attribute.Int("agent.prompt.version", resolved.Version),
	}
	if len(resolved.Labels) > 0 {
		attrs = append(attrs, attribute.StringSlice("agent.prompt.labels", resolved.Labels))
	}
	trace.SpanFromContext(ctx).AddEvent("agent.prompt.resolved", trace.WithAttributes(attrs...))
	recordPromptResolutionSpan(ctx, attrs, false, "")
}

func recordPromptResolutionError(ctx context.Context, name string, selector Selector, outcome string) {
	if ctx == nil {
		return
	}
	attrs := []attribute.KeyValue{
		attribute.String("agent.prompt.name", strings.TrimSpace(name)),
		attribute.String("agent.prompt.selector", selector.String()),
		attribute.String("agent.prompt.outcome", outcome),
	}
	trace.SpanFromContext(ctx).AddEvent("agent.prompt.resolve_failed", trace.WithAttributes(attrs...))
	recordPromptResolutionSpan(ctx, attrs, true, outcome)
}

// recordPromptResolutionSpan mirrors prompt resolution metadata onto a short
// child span. Grafana's trace timeline always exposes spans, while span events
// can be hidden by drilldown and trace-detail views. The existing parent event
// remains the compatibility and point-in-time signal.
func recordPromptResolutionSpan(ctx context.Context, attrs []attribute.KeyValue, failed bool, outcome string) {
	parent := trace.SpanFromContext(ctx)
	tracer := parent.TracerProvider().Tracer("ai-agent/promptmanager")
	_, span := tracer.Start(ctx, promptResolutionSpanName, trace.WithAttributes(attrs...))
	if failed {
		span.SetStatus(codes.Error, outcome)
	}
	span.End()
}

func promptCacheKey(host, pubKey, name string, selector Selector) string {
	if host == "" {
		host = "https://cloud.langfuse.com"
	}
	return strings.Join([]string{host, pubKey, name, selector.String()}, "\x00")
}

func buildPromptURL(host, name string, selector Selector) (string, error) {
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
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("Langfuse prompt name is empty")
	}
	normalized, err := selector.Normalize()
	if err != nil {
		return "", err
	}

	basePath := strings.TrimRight(base.Path, "/")
	baseEscapedPath := strings.TrimRight(base.EscapedPath(), "/")
	base.Path = basePath + "/api/public/v2/prompts/" + name
	base.RawPath = baseEscapedPath + "/api/public/v2/prompts/" + url.PathEscape(name)

	query := base.Query()
	if normalized.Version > 0 {
		query.Set("version", fmt.Sprintf("%d", normalized.Version))
	} else {
		query.Set("label", normalized.Label)
	}
	base.RawQuery = query.Encode()
	return base.String(), nil
}

func renderPromptContent(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", fmt.Errorf("received empty prompt content from Langfuse")
	}

	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		text = strings.TrimSpace(text)
		if text == "" {
			return "", fmt.Errorf("received empty prompt content from Langfuse")
		}
		return text, nil
	}

	var messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(raw, &messages); err != nil {
		return "", fmt.Errorf("unsupported Langfuse prompt format: %w", err)
	}
	parts := make([]string, 0, len(messages))
	for _, msg := range messages {
		content := strings.TrimSpace(msg.Content)
		if content == "" {
			continue
		}
		role := strings.TrimSpace(msg.Role)
		if role != "" {
			parts = append(parts, role+": "+content)
		} else {
			parts = append(parts, content)
		}
	}
	if len(parts) == 0 {
		return "", fmt.Errorf("received empty prompt content from Langfuse")
	}
	return strings.Join(parts, "\n\n"), nil
}
