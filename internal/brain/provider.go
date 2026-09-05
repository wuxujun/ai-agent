package brain

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/wuxujun/ai-agent/internal/wiki"
)

var (
	ErrProviderScope       = errors.New("brain provider scope is invalid")
	ErrProviderWatermark   = errors.New("brain provider retraction watermark changed")
	ErrProviderUnavailable = errors.New("brain provider snapshot unavailable")
)

// Provider serves only a verified, immutable release selected by the caller's
// pinned task scope. It never falls back to CURRENT or staging.
type Provider struct {
	Repository *Repository
	Ledger     RetractionView
}

func NewProvider(repository *Repository, ledger RetractionView) *Provider {
	return &Provider{Repository: repository, Ledger: ledger}
}

func (p *Provider) Search(ctx context.Context, query string, topK int, space string) ([]wiki.Document, error) {
	return nil, fmt.Errorf("brain provider requires explicit scope: %w", ErrProviderScope)
}

func (p *Provider) Read(context.Context, wiki.Document, string) (wiki.Document, error) {
	return wiki.Document{}, fmt.Errorf("brain provider requires explicit scope: %w", ErrProviderScope)
}

func (p *Provider) SearchCorpus(ctx context.Context, query string, topK int, space string, scope ProjectRef, snapshotID string) ([]wiki.Document, error) {
	client, release, watermark, err := p.open(ctx, scope, snapshotID)
	if err != nil {
		return nil, err
	}
	documents, err := client.Search(ctx, query, topK, space)
	if err != nil {
		return nil, err
	}
	if err := p.checkWatermark(ctx, scope, watermark); err != nil {
		return nil, err
	}
	_ = release
	return documents, nil
}

func (p *Provider) ReadCorpus(ctx context.Context, document wiki.Document, space string, scope ProjectRef, snapshotID string) (wiki.Document, error) {
	client, _, watermark, err := p.open(ctx, scope, snapshotID)
	if err != nil {
		return wiki.Document{}, err
	}
	result, err := client.Read(ctx, document, space)
	if err != nil {
		return wiki.Document{}, err
	}
	if err := p.checkWatermark(ctx, scope, watermark); err != nil {
		return wiki.Document{}, err
	}
	return result, nil
}

func (p *Provider) open(ctx context.Context, scope ProjectRef, snapshotID string) (*wiki.DirectoryClient, Release, string, error) {
	if p == nil || p.Repository == nil || p.Ledger == nil || strings.TrimSpace(scope.TenantID) == "" || strings.TrimSpace(scope.ProjectID) == "" || strings.TrimSpace(snapshotID) == "" {
		return nil, Release{}, "", ErrProviderScope
	}
	release, err := p.Repository.OpenRelease(ctx, scope, snapshotID)
	if err != nil {
		return nil, Release{}, "", fmt.Errorf("%w: %v", ErrProviderUnavailable, sanitizeProviderError(err))
	}
	watermark, err := p.Ledger.Watermark(ctx, scope)
	if err != nil || watermark != release.Manifest.RetractionWatermark {
		return nil, Release{}, "", ErrProviderWatermark
	}
	client, err := wiki.NewDirectory(filepath.Join(release.Root, "wiki"))
	if err != nil {
		return nil, Release{}, "", ErrProviderUnavailable
	}
	if err := client.Initialize(ctx); err != nil {
		return nil, Release{}, "", ErrProviderUnavailable
	}
	return client, release, watermark, nil
}

func (p *Provider) checkWatermark(ctx context.Context, scope ProjectRef, expected string) error {
	watermark, err := p.Ledger.Watermark(ctx, scope)
	if err != nil || watermark != expected {
		return ErrProviderWatermark
	}
	return nil
}

func sanitizeProviderError(error) string { return "snapshot unavailable" }
