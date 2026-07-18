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
)

var log = logger.Component("promptmanager")

type PromptResponse struct {
	Name   string          `json:"name"`
	Prompt json.RawMessage `json:"prompt"`
}

type cachedPrompt struct {
	content   string
	expiredAt time.Time
}

type PromptManager struct {
	mu     sync.RWMutex
	cache  map[string]cachedPrompt
	client *http.Client
	ttl    time.Duration
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
	cfg := config.Get().Langfuse
	if !cfg.Enabled || cfg.PublicKey == "" || cfg.SecretKey == "" {
		return fallback
	}

	cacheKey := promptCacheKey(cfg.Host, cfg.PublicKey, name)

	// 1. Thread-safe read check of cache
	pm.mu.RLock()
	cached, exists := pm.cache[cacheKey]
	pm.mu.RUnlock()

	if exists && time.Now().Before(cached.expiredAt) {
		return cached.content
	}

	// 2. Cache miss or expired: fetch from Langfuse
	log.Info("fetching prompt from Langfuse", "name", name)
	content, err := pm.fetchFromLangfuse(ctx, name, cfg.Host, cfg.PublicKey, cfg.SecretKey)
	if err != nil {
		log.Error("failed to fetch prompt from Langfuse; using fallback", "name", name, "error", err)
		// On failure, write a short-lived fallback cache (e.g. 1 minute) to avoid hammering Langfuse on every subsequent request
		pm.mu.Lock()
		pm.cache[cacheKey] = cachedPrompt{
			content:   fallback,
			expiredAt: time.Now().Add(1 * time.Minute),
		}
		pm.mu.Unlock()
		return fallback
	}

	// 3. Update cache with long-lived TTL
	pm.mu.Lock()
	pm.cache[cacheKey] = cachedPrompt{
		content:   content,
		expiredAt: time.Now().Add(pm.ttl),
	}
	pm.mu.Unlock()

	return content
}

func (pm *PromptManager) fetchFromLangfuse(ctx context.Context, name, host, pubKey, secKey string) (string, error) {
	if host == "" {
		host = "https://cloud.langfuse.com"
	}

	endpoint, err := buildPromptURL(host, name)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return "", err
	}

	req.SetBasicAuth(pubKey, secKey)
	req.Header.Set("Accept", "application/json")

	resp, err := pm.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("http status %d", resp.StatusCode)
	}

	var data PromptResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return "", err
	}

	content, err := renderPromptContent(data.Prompt)
	if err != nil {
		return "", err
	}

	return content, nil
}

func promptCacheKey(host, pubKey, name string) string {
	if host == "" {
		host = "https://cloud.langfuse.com"
	}
	return strings.Join([]string{host, pubKey, name, "production"}, "\x00")
}

func buildPromptURL(host, name string) (string, error) {
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

	basePath := strings.TrimRight(base.Path, "/")
	baseEscapedPath := strings.TrimRight(base.EscapedPath(), "/")
	base.Path = basePath + "/api/public/v2/prompts/" + name
	base.RawPath = baseEscapedPath + "/api/public/v2/prompts/" + url.PathEscape(name)

	query := base.Query()
	query.Set("label", "production")
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
