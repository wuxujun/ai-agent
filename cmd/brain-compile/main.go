package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"strings"
	"time"

	"github.com/wuxujun/ai-agent/internal/brain"
	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/store"
)

type repository interface {
	Current(context.Context, brain.ProjectRef) (string, error)
	OpenRelease(context.Context, brain.ProjectRef, string) (brain.Release, error)
	OpenStaging(context.Context, brain.ProjectRef, string) (brain.Release, error)
	Status(context.Context, brain.ProjectRef) (brain.RepositoryStatus, error)
	Publish(context.Context, brain.ProjectRef, string, string) (brain.Manifest, error)
	Rollback(context.Context, brain.ProjectRef, string, string) (brain.Manifest, error)
}

type dependencies struct {
	loadConfig    func() *config.Config
	openStore     func(string, string) (store.Store, error)
	newRepository func(string, *brain.FileRetractionLedger) (repository, error)
	build         func(context.Context, *config.Config, store.Store, brain.ProjectRef, time.Time, string) (brain.Manifest, error)
	now           func() time.Time
}

func defaultDependencies() dependencies {
	return dependencies{
		loadConfig: config.Get,
		openStore:  store.Open,
		newRepository: func(root string, ledger *brain.FileRetractionLedger) (repository, error) {
			return brain.NewRepository(root, ledger)
		},
		build: defaultBuild,
		now:   time.Now,
	}
}

func defaultBuild(ctx context.Context, cfg *config.Config, st store.Store, ref brain.ProjectRef, cutoff time.Time, expectedCurrent string) (brain.Manifest, error) {
	ledger := brain.NewFileRetractionLedger(cfg.Brain.Root)
	repo, err := brain.NewRepository(cfg.Brain.Root, ledger)
	if err != nil {
		return brain.Manifest{}, err
	}
	compiler := &brain.Compiler{
		Sources:    brain.NewSourceReader(st),
		Repository: repo,
		Ledger:     ledger,
		Config:     cfg,
	}
	return compiler.Build(ctx, brain.BuildRequest{Ref: ref, Cutoff: cutoff, ExpectedCurrent: expectedCurrent})
}

type commonFlags struct {
	tenant, project, snapshot, expectedCurrent, cutoff string
}

func run(args []string, stdout, stderr io.Writer, deps dependencies) int {
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	if deps.loadConfig == nil {
		deps.loadConfig = config.Get
	}
	if deps.openStore == nil {
		deps.openStore = store.Open
	}
	if deps.newRepository == nil {
		deps.newRepository = defaultDependencies().newRepository
	}
	if deps.build == nil {
		deps.build = defaultBuild
	}
	if deps.now == nil {
		deps.now = time.Now
	}
	if len(args) == 0 {
		return fail(stderr, 2)
	}
	command := args[0]
	common, provided, ok := parseFlags(command, args[1:])
	if !ok || common.tenant == "" || common.project == "" {
		return fail(stderr, 2)
	}
	cfg := deps.loadConfig()
	if cfg == nil {
		return fail(stderr, 2)
	}
	ref, err := brain.ResolveProject(cfg, common.tenant, common.project)
	if err != nil {
		return failClass(stderr, err)
	}
	ctx := context.Background()
	ledger := brain.NewFileRetractionLedger(cfg.Brain.Root)
	repo, err := deps.newRepository(cfg.Brain.Root, ledger)
	if err != nil {
		return failClass(stderr, err)
	}

	switch command {
	case "build":
		cutoff := deps.now().UTC()
		if common.cutoff != "" {
			cutoff, err = time.Parse(time.RFC3339, common.cutoff)
			if err != nil {
				return fail(stderr, 2)
			}
		}
		st, openErr := deps.openStore(cfg.Store.Type, cfg.Store.DSN)
		if openErr != nil {
			return fail(stderr, 2)
		}
		defer st.Close()
		manifest, buildErr := deps.build(ctx, cfg, st, ref, cutoff, common.expectedCurrent)
		if buildErr != nil {
			return failClass(stderr, buildErr)
		}
		return emitManifest(stdout, manifest)
	case "publish", "rollback":
		if !provided["snapshot"] || !provided["expected-current"] || common.snapshot == "" {
			return fail(stderr, 2)
		}
		var manifest brain.Manifest
		if command == "publish" {
			manifest, err = repo.Publish(ctx, ref, common.snapshot, common.expectedCurrent)
		} else {
			manifest, err = repo.Rollback(ctx, ref, common.snapshot, common.expectedCurrent)
		}
		if err != nil {
			return failClass(stderr, err)
		}
		return emitManifest(stdout, manifest)
	case "inspect", "verify":
		if !provided["snapshot"] || common.snapshot == "" {
			return fail(stderr, 2)
		}
		release, openErr := repo.OpenRelease(ctx, ref, common.snapshot)
		if errors.Is(openErr, brain.ErrSnapshotNotFound) {
			release, openErr = repo.OpenStaging(ctx, ref, common.snapshot)
		}
		if openErr != nil {
			return failClass(stderr, openErr)
		}
		if command == "verify" {
			return emitMetadata(stdout, map[string]any{"snapshot_id": release.Manifest.SnapshotID, "verified": true, "tenant": ref.TenantID, "project": ref.ProjectID})
		}
		return emitMetadata(stdout, manifestMetadata(release.Manifest))
	case "status":
		repositoryStatus, statusErr := repo.Status(ctx, ref)
		if statusErr != nil && !isGateError(statusErr) {
			return fail(stderr, 2)
		}
		status := map[string]any{"tenant": ref.TenantID, "project": ref.ProjectID, "current_snapshot_id": repositoryStatus.Current, "staging_snapshot_ids": repositoryStatus.Staging, "release_snapshot_ids": repositoryStatus.Releases, "revocation_state": repositoryStatus.RevocationState}
		if emitMetadata(stdout, status) != 0 {
			return 2
		}
		if statusErr != nil {
			return 1
		}
		return 0
	default:
		return fail(stderr, 2)
	}
}

func parseFlags(command string, args []string) (commonFlags, map[string]bool, bool) {
	var f commonFlags
	provided := map[string]bool{}
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&f.tenant, "tenant", "", "tenant")
	fs.StringVar(&f.project, "project", "", "project")
	fs.StringVar(&f.snapshot, "snapshot", "", "snapshot")
	fs.StringVar(&f.expectedCurrent, "expected-current", "", "expected current")
	fs.StringVar(&f.cutoff, "cutoff", "", "source cutoff")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return f, provided, false
	}
	for _, arg := range args {
		for _, name := range []string{"snapshot", "expected-current"} {
			if strings.HasPrefix(arg, "--"+name+"=") || arg == "--"+name {
				provided[name] = true
			}
		}
	}
	return f, provided, true
}

func manifestMetadata(manifest brain.Manifest) map[string]any {
	return map[string]any{"snapshot_id": manifest.SnapshotID, "parent_id": manifest.ParentID, "tenant": manifest.TenantID, "project": manifest.ProjectID, "source_count": len(manifest.SourceIDs), "model": manifest.Model, "prompt_version": manifest.PromptVersion, "config_digest": manifest.ConfigDigest, "estimated_cost_usd": manifest.EstimatedCostUSD, "publishable": manifest.Validation.Publishable}
}

func emitManifest(w io.Writer, manifest brain.Manifest) int {
	return emitMetadata(w, manifestMetadata(manifest))
}

func emitMetadata(w io.Writer, value any) int {
	if err := json.NewEncoder(w).Encode(value); err != nil {
		return 2
	}
	return 0
}

func fail(w io.Writer, code int) int { _, _ = io.WriteString(w, "brain compile failed\n"); return code }

func failClass(w io.Writer, err error) int {
	if isGateError(err) {
		return fail(w, 1)
	}
	return fail(w, 2)
}

func isGateError(err error) bool {
	for _, target := range []error{brain.ErrCurrentConflict, brain.ErrSnapshotRevoked, brain.ErrSnapshotUnverified, brain.ErrSnapshotCorrupt, brain.ErrRetractionChanged, brain.ErrCompileValidation, brain.ErrCompileRetraction, brain.ErrCompileBudget, brain.ErrCompileSynthesis} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, defaultDependencies())) }
