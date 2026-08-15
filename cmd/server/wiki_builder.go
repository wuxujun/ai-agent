package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/tools"
	"github.com/wuxujun/ai-agent/internal/wiki"
)

type wikiClient interface {
	tools.WikiReader
	Initialize(context.Context) error
	Close(context.Context) error
}

type wikiClientFactory func(wiki.Config) (wikiClient, error)

type wikiRuntime struct {
	client wikiClient
}

func newWikiClient(cfg wiki.Config) (wikiClient, error) { return wiki.New(cfg) }

func buildWikiRuntime(ctx context.Context, cfg *config.Config, registry *tools.Registry) (*wikiRuntime, error) {
	return buildWikiRuntimeWithFactory(ctx, cfg, registry, newWikiClient)
}

func buildWikiRuntimeWithFactory(ctx context.Context, cfg *config.Config, registry *tools.Registry, factory wikiClientFactory) (*wikiRuntime, error) {
	runtime := &wikiRuntime{}
	if cfg == nil || (strings.TrimSpace(cfg.Wiki.URL) == "" && strings.TrimSpace(cfg.Wiki.Directory) == "") {
		return runtime, nil
	}
	if strings.TrimSpace(cfg.Wiki.Directory) != "" {
		client, err := wiki.NewDirectory(cfg.Wiki.Directory)
		if err == nil {
			err = client.Initialize(ctx)
		}
		if err != nil {
			if cfg.Wiki.Required {
				return nil, fmt.Errorf("initialize required local Wiki: %w", err)
			}
			slog.Warn("optional local Wiki unavailable", "error", err)
			return runtime, nil
		}
		if err := tools.RegisterWikiTools(registry, client); err != nil {
			return nil, err
		}
		runtime.client = client
		slog.Info("read-only local Wiki initialized", "directory", cfg.Wiki.Directory)
		return runtime, nil
	}
	authorization := ""
	if envName := strings.TrimSpace(cfg.Wiki.AuthorizationEnv); envName != "" {
		authorization = os.Getenv(envName)
		if authorization == "" {
			err := fmt.Errorf("wiki authorization environment variable %s is empty", envName)
			if cfg.Wiki.Required {
				return nil, err
			}
			slog.Warn("optional LLM Wiki unavailable", "error", err)
			return runtime, nil
		}
	}
	timeout := time.Duration(cfg.Wiki.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	client, err := factory(wiki.Config{
		URL: cfg.Wiki.URL, Authorization: authorization, Timeout: timeout,
		AllowPrivateNetwork: cfg.Wiki.AllowPrivateNetwork,
	})
	if err == nil {
		initCtx, cancel := context.WithTimeout(ctx, timeout)
		err = client.Initialize(initCtx)
		cancel()
	}
	if err != nil {
		if client != nil {
			closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = client.Close(closeCtx)
			cancel()
		}
		if cfg.Wiki.Required {
			return nil, fmt.Errorf("initialize required LLM Wiki: %w", err)
		}
		slog.Warn("optional LLM Wiki unavailable", "error", err)
		return runtime, nil
	}
	if err := tools.RegisterWikiTools(registry, client); err != nil {
		closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = client.Close(closeCtx)
		cancel()
		return nil, err
	}
	runtime.client = client
	slog.Info("read-only LLM Wiki initialized", "url", cfg.Wiki.URL, "default_space", cfg.Wiki.DefaultSpace)
	return runtime, nil
}

func (r *wikiRuntime) Close() error {
	if r == nil || r.client == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return r.client.Close(ctx)
}
