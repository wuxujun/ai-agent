package brain

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/types"
	"github.com/wuxujun/ai-agent/internal/wiki"
)

type TaskContext struct {
	Ref                                    ProjectRef
	SnapshotID, ConfigDigest, CompactIndex string
}
type SnapshotPinner interface {
	Pin(context.Context, *types.Task) (TaskContext, bool, error)
}
type taskContextKey struct{}

func WithTaskContext(ctx context.Context, task TaskContext) context.Context {
	return context.WithValue(ctx, taskContextKey{}, task)
}
func TaskContextFrom(ctx context.Context) (TaskContext, bool) {
	value, ok := ctx.Value(taskContextKey{}).(TaskContext)
	return value, ok
}

var (
	ErrProviderScope       = errors.New("brain provider scope is invalid")
	ErrProviderWatermark   = errors.New("brain provider retraction watermark changed")
	ErrProviderUnavailable = errors.New("brain provider snapshot unavailable")
	ErrPinConfiguration    = errors.New("brain task pin configuration is invalid")
	ErrPinMissing          = errors.New("brain task snapshot is unavailable")
	ErrPinConfigDrift      = errors.New("brain task snapshot configuration drift")
)

type SnapshotPinnerImpl struct {
	Repository *Repository
	Ledger     RetractionView
	Config     *config.Config
}

func NewSnapshotPinner(repository *Repository, ledger RetractionView, cfg *config.Config) *SnapshotPinnerImpl {
	return &SnapshotPinnerImpl{Repository: repository, Ledger: ledger, Config: cfg}
}
func (p *SnapshotPinnerImpl) Pin(ctx context.Context, task *types.Task) (TaskContext, bool, error) {
	if p == nil || p.Repository == nil || p.Ledger == nil || p.Config == nil || task == nil || strings.TrimSpace(task.BrainProjectID) == "" {
		return TaskContext{}, false, nil
	}
	ref, err := ResolveProject(p.Config, task.TenantID, task.BrainProjectID)
	if err != nil {
		return TaskContext{}, false, ErrPinConfiguration
	}
	snapshotID, changed := task.BrainSnapshotID, false
	if snapshotID == "" {
		snapshotID, err = p.Repository.Current(ctx, ref)
		if err != nil || snapshotID == "" {
			return TaskContext{}, false, ErrPinMissing
		}
		changed = true
	}
	release, err := p.Repository.OpenRelease(ctx, ref, snapshotID)
	if err != nil {
		return TaskContext{}, false, ErrPinMissing
	}
	admissionDigest := ProjectConfigDigest(ref)
	if task.BrainConfigDigest != "" && task.BrainConfigDigest != admissionDigest {
		return TaskContext{}, false, ErrPinConfigDrift
	}
	watermark, err := p.Ledger.Watermark(ctx, ref)
	if err != nil || watermark != release.Manifest.RetractionWatermark {
		return TaskContext{}, false, ErrProviderWatermark
	}
	compactIndex := ""
	if limit := p.Config.Brain.CompactIndexMaxBytes; limit > 0 {
		index, readErr := os.ReadFile(filepath.Join(release.Root, "wiki", "_index.md"))
		if readErr != nil || len(index) > limit {
			return TaskContext{}, false, ErrPinMissing
		}
		compactIndex = string(index)
	}
	if changed {
		task.BrainSnapshotID = snapshotID
		task.BrainConfigDigest = admissionDigest
	}
	return TaskContext{Ref: ref, SnapshotID: snapshotID, ConfigDigest: admissionDigest, CompactIndex: compactIndex}, changed, nil
}

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
	space = scope.WikiSpace
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
	space = scope.WikiSpace
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

func (p *Provider) GraphCorpus(ctx context.Context, document wiki.Document, space string, depth int, direction string, scope ProjectRef, snapshotID string) (wiki.GraphResult, error) {
	space = scope.WikiSpace
	client, _, watermark, err := p.open(ctx, scope, snapshotID)
	if err != nil {
		return wiki.GraphResult{}, err
	}
	result, err := client.Graph(ctx, document, space, depth, direction)
	if err != nil {
		return wiki.GraphResult{}, err
	}
	if err := p.checkWatermark(ctx, scope, watermark); err != nil {
		return wiki.GraphResult{}, err
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
