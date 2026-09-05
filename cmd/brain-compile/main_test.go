package main

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/brain"
	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/store"
)

func TestRunPublishRequiresScopeSnapshotAndExpectedCurrent(t *testing.T) {
	code := run([]string{"publish", "--tenant", "tenant-a"}, &bytes.Buffer{}, &bytes.Buffer{}, testDeps(nil))
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
}

func TestRunPublishConflictReturnsGateExit(t *testing.T) {
	deps := testDeps(brain.ErrCurrentConflict)
	code := run([]string{"publish", "--tenant", "tenant-a", "--project", "atlas", "--snapshot", "snap-1", "--expected-current", ""}, &bytes.Buffer{}, &bytes.Buffer{}, deps)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
}

func TestRunRequiresExplicitTenantAndProject(t *testing.T) {
	for _, args := range [][]string{{"status", "--project", "atlas"}, {"status", "--tenant", "tenant-a"}} {
		if code := run(args, &bytes.Buffer{}, &bytes.Buffer{}, testDeps(nil)); code != 2 {
			t.Fatalf("run(%v) = %d, want 2", args, code)
		}
	}
}

func TestRunLifecycleCommandsStayBoundedAndOnlyBuildOpensStore(t *testing.T) {
	for _, command := range []string{"inspect", "verify", "rollback", "status"} {
		deps := testDeps(nil)
		opened := 0
		deps.openStore = func(string, string) (store.Store, error) {
			opened++
			return store.NewMemoryStore(), nil
		}
		args := []string{command, "--tenant", "tenant-a", "--project", "atlas"}
		if command == "inspect" || command == "verify" {
			args = append(args, "--snapshot", "snap-1")
		} else if command == "rollback" {
			args = append(args, "--snapshot", "snap-1", "--expected-current=")
		}
		var output bytes.Buffer
		if code := run(args, &output, &bytes.Buffer{}, deps); code != 0 {
			t.Fatalf("run(%v) = %d, output=%s", args, code, output.String())
		}
		if opened != 0 {
			t.Fatalf("run(%v) opened Store", args)
		}
		if bytes.Contains(output.Bytes(), []byte("secret")) {
			t.Fatalf("run(%v) leaked secret: %s", args, output.String())
		}
	}
	deps := testDeps(nil)
	opened := 0
	deps.openStore = func(string, string) (store.Store, error) { opened++; return store.NewMemoryStore(), nil }
	deps.build = func(context.Context, *config.Config, store.Store, brain.ProjectRef, time.Time, string) (brain.Manifest, error) {
		return brain.Manifest{SnapshotID: "staged"}, nil
	}
	if code := run([]string{"build", "--tenant", "tenant-a", "--project", "atlas"}, &bytes.Buffer{}, &bytes.Buffer{}, deps); code != 0 || opened != 1 {
		t.Fatalf("build code=%d opened=%d, want 0/1", code, opened)
	}
}

type fakeRepository struct{ err error }

func (f fakeRepository) Current(context.Context, brain.ProjectRef) (string, error) { return "", nil }
func (f fakeRepository) OpenRelease(context.Context, brain.ProjectRef, string) (brain.Release, error) {
	return brain.Release{}, nil
}
func (f fakeRepository) OpenStaging(context.Context, brain.ProjectRef, string) (brain.Release, error) {
	return brain.Release{}, f.err
}
func (f fakeRepository) Status(context.Context, brain.ProjectRef) (brain.RepositoryStatus, error) {
	return brain.RepositoryStatus{Current: "current", Staging: []string{"staging"}, Releases: []string{"release"}, RevocationState: "verified"}, f.err
}
func (f fakeRepository) Publish(context.Context, brain.ProjectRef, string, string) (brain.Manifest, error) {
	return brain.Manifest{}, f.err
}
func (f fakeRepository) Rollback(context.Context, brain.ProjectRef, string, string) (brain.Manifest, error) {
	return brain.Manifest{}, f.err
}

func testDeps(repoErr error) dependencies {
	cfg := &config.Config{}
	cfg.Brain.Enabled = true
	cfg.Brain.Root = tTempRoot
	cfg.API.Tenants = map[string]config.APITenantConfig{"tenant-a": {BrainProjects: map[string]config.BrainProjectConfig{"atlas": {WikiSpace: "brain-atlas"}}}}
	return dependencies{
		loadConfig: func() *config.Config { return cfg },
		openStore:  func(string, string) (store.Store, error) { return store.NewMemoryStore(), nil },
		newRepository: func(string, *brain.FileRetractionLedger) (repository, error) {
			return fakeRepository{err: repoErr}, nil
		},
	}
}

var tTempRoot = "/tmp/brain-compile-test"
