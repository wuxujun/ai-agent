package brain

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"golang.org/x/sys/unix"
)

const (
	maxCurrentBytes             = 4096
	maxSnapshotFileBytes        = 1024 * 1024
	maxSnapshotTreeBytes        = 8 * 1024 * 1024
	maxSnapshotEvidenceBytes    = 2 * 1024 * 1024
	maxSnapshotManifestBytes    = 1024 * 1024
	maxEvidenceLineBytes        = 64 * 1024
	maxSnapshotFiles            = 1024
	maxSnapshotEvidenceRecords  = 1000
	maxSnapshotEvidenceFieldLen = 32 * 1024
	maxSnapshotPathBytes        = 4096
)

var (
	ErrUnsafePath               = errors.New("unsafe brain repository path")
	ErrCurrentConflict          = errors.New("brain current snapshot conflict")
	ErrSnapshotExists           = errors.New("brain snapshot already exists")
	ErrSnapshotNotFound         = errors.New("brain snapshot not found")
	ErrSnapshotRevoked          = errors.New("brain snapshot revoked")
	ErrRetractionChanged        = errors.New("brain retraction watermark changed")
	ErrSnapshotUnverified       = errors.New("brain snapshot is not verified")
	ErrSnapshotCorrupt          = errors.New("brain snapshot is corrupt")
	ErrCrossFilesystem          = errors.New("brain snapshot cross-filesystem rename rejected")
	ErrSnapshotTooLarge         = errors.New("brain snapshot exceeds repository limits")
	ErrCurrentDurabilityUnknown = errors.New("brain current snapshot committed with unknown durability")
	errSecurePathMissing        = errors.New("brain secure path is missing")
)

// Repository persists complete Brain snapshots under one configured root.
// All tree operations are anchored to opened directory descriptors. Publish
// and rollback serialize by flocking the private, regular project lock file,
// which is also the lock protocol that retraction writers must honor.
type Repository struct {
	root   string
	ledger RetractionView

	// The following hooks are narrow crash/race injection seams. A nil rename
	// means use the platform atomic operation; a non-nil rename runs immediately
	// before it and may abort it.
	rename                  func(string, string) error
	beforeFileCreate        func()
	beforeReleaseRename     func()
	beforeCurrentCommit     func()
	syncCurrentBeforeRename func() error
	syncCurrentParent       func() error
}

// RepositoryStatus is bounded operator metadata for one Brain project.
type RepositoryStatus struct {
	Current         string   `json:"current_snapshot_id"`
	Staging         []string `json:"staging_snapshot_ids"`
	Releases        []string `json:"release_snapshot_ids"`
	RevocationState string   `json:"revocation_state"`
}

type secureDir struct {
	fd   int
	path string
}

type projectLock struct {
	directory *secureDir
	fd        int
}

type projectHandle struct {
	dir         *secureDir
	tenant      *secureDir
	projectName string
}

type preparedSnapshot struct {
	manifest      Manifest
	manifestBytes []byte
	evidenceBytes []byte
	fileNames     []string
}

func NewRepository(root string, ledger RetractionView) (*Repository, error) {
	if ledger == nil || strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("brain repository configuration is invalid: %w", ErrUnsafePath)
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve brain repository root: %w", ErrUnsafePath)
	}
	absRoot = filepath.Clean(absRoot)
	directory, err := openAbsoluteDirectory(absRoot, true)
	if err != nil {
		return nil, err
	}
	directory.close()
	return &Repository{root: absRoot, ledger: ledger}, nil
}

func (r *Repository) Current(ctx context.Context, ref ProjectRef) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	project, err := openProjectHandle(r.root, ref, false)
	if errors.Is(err, errSecurePathMissing) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer project.close()
	current, err := currentAt(ctx, project.dir)
	if err != nil {
		return "", err
	}
	if err := project.verify(); err != nil {
		return "", err
	}
	return current, nil
}

func (r *Repository) CreateStage(ctx context.Context, ref ProjectRef, draft SnapshotDraft) (Manifest, error) {
	if err := ctx.Err(); err != nil {
		return Manifest{}, err
	}
	prepared, err := prepareSnapshot(ref, draft)
	if err != nil {
		return Manifest{}, err
	}
	project, err := openProjectHandle(r.root, ref, true)
	if err != nil {
		return Manifest{}, err
	}
	defer project.close()
	lock, err := acquireProjectLock(ctx, project.dir)
	if err != nil {
		return Manifest{}, err
	}
	defer lock.release()

	staging, err := project.dir.openChildDirectory("staging", true, false)
	if err != nil {
		return Manifest{}, err
	}
	defer staging.close()
	releases, err := project.dir.openChildDirectory("releases", true, false)
	if err != nil {
		return Manifest{}, err
	}
	defer releases.close()
	if exists, err := staging.childExists(prepared.manifest.SnapshotID); err != nil {
		return Manifest{}, err
	} else if exists {
		return Manifest{}, ErrSnapshotExists
	}
	if exists, err := releases.childExists(prepared.manifest.SnapshotID); err != nil {
		return Manifest{}, err
	} else if exists {
		return Manifest{}, ErrSnapshotExists
	}
	stage, err := staging.openChildDirectory(prepared.manifest.SnapshotID, true, true)
	if err != nil {
		return Manifest{}, err
	}
	defer stage.close()
	wiki, err := stage.openChildDirectory("wiki", true, true)
	if err != nil {
		return Manifest{}, err
	}
	defer wiki.close()

	for _, name := range prepared.fileNames {
		if err := ctx.Err(); err != nil {
			return Manifest{}, err
		}
		if r.beforeFileCreate != nil {
			r.beforeFileCreate()
		}
		if err := writeRelativeNewFile(wiki, name, draft.Files[name]); err != nil {
			return Manifest{}, err
		}
	}
	if err := writeNewFileAt(stage, "evidence.jsonl", prepared.evidenceBytes); err != nil {
		return Manifest{}, err
	}
	if err := writeNewFileAt(stage, "manifest.json", prepared.manifestBytes); err != nil {
		return Manifest{}, err
	}
	if err := syncSecureDirectory(stage); err != nil {
		return Manifest{}, err
	}
	if err := verifySnapshotFilesAt(ctx, stage, prepared.manifest.FileHashes, len(prepared.manifestBytes)); err != nil {
		return Manifest{}, err
	}
	if err := stage.verifyChildIdentity("wiki", wiki); err != nil {
		return Manifest{}, err
	}
	if err := staging.verifyChildIdentity(prepared.manifest.SnapshotID, stage); err != nil {
		return Manifest{}, err
	}
	if err := project.verify(); err != nil {
		return Manifest{}, err
	}
	if err := lock.verify(); err != nil {
		return Manifest{}, err
	}
	return prepared.manifest, nil
}

func (r *Repository) OpenRelease(ctx context.Context, ref ProjectRef, snapshotID string) (Release, error) {
	if err := ctx.Err(); err != nil {
		return Release{}, err
	}
	if !safeSingleComponent(snapshotID) {
		return Release{}, fmt.Errorf("brain snapshot identifier is unsafe: %w", ErrUnsafePath)
	}
	project, err := openProjectHandle(r.root, ref, false)
	if errors.Is(err, errSecurePathMissing) {
		return Release{}, ErrSnapshotNotFound
	}
	if err != nil {
		return Release{}, err
	}
	defer project.close()
	releases, err := project.dir.openChildDirectory("releases", false, false)
	if errors.Is(err, errSecurePathMissing) {
		return Release{}, ErrSnapshotNotFound
	}
	if err != nil {
		return Release{}, err
	}
	defer releases.close()
	release, err := r.openSnapshotChild(ctx, ref, releases, snapshotID, filepath.Join(project.dir.path, "releases", snapshotID))
	if err != nil {
		return Release{}, err
	}
	if err := project.verify(); err != nil {
		return Release{}, err
	}
	return release, nil
}

// OpenStaging verifies and opens a staged snapshot without publishing it.
func (r *Repository) OpenStaging(ctx context.Context, ref ProjectRef, snapshotID string) (Release, error) {
	if err := ctx.Err(); err != nil {
		return Release{}, err
	}
	if !safeSingleComponent(snapshotID) {
		return Release{}, fmt.Errorf("brain snapshot identifier is unsafe: %w", ErrUnsafePath)
	}
	project, err := openProjectHandle(r.root, ref, false)
	if errors.Is(err, errSecurePathMissing) {
		return Release{}, ErrSnapshotNotFound
	}
	if err != nil {
		return Release{}, err
	}
	defer project.close()
	staging, err := project.dir.openChildDirectory("staging", false, false)
	if errors.Is(err, errSecurePathMissing) {
		return Release{}, ErrSnapshotNotFound
	}
	if err != nil {
		return Release{}, err
	}
	defer staging.close()
	release, err := r.openSnapshotChild(ctx, ref, staging, snapshotID, filepath.Join(project.dir.path, "staging", snapshotID))
	if err != nil {
		return Release{}, err
	}
	if err := project.verify(); err != nil {
		return Release{}, err
	}
	return release, nil
}

// Status returns bounded lifecycle metadata and verifies the current release
// against the live retraction ledger when one exists.
func (r *Repository) Status(ctx context.Context, ref ProjectRef) (RepositoryStatus, error) {
	status := RepositoryStatus{RevocationState: "none"}
	current, err := r.Current(ctx, ref)
	if err != nil {
		return status, err
	}
	status.Current = current
	project, err := openProjectHandle(r.root, ref, false)
	if errors.Is(err, errSecurePathMissing) {
		return status, nil
	}
	if err != nil {
		return status, err
	}
	defer project.close()
	for _, spec := range []struct {
		name string
		out  *[]string
	}{
		{name: "staging", out: &status.Staging},
		{name: "releases", out: &status.Releases},
	} {
		directory, openErr := project.dir.openChildDirectory(spec.name, false, false)
		if errors.Is(openErr, errSecurePathMissing) {
			continue
		}
		if openErr != nil {
			return status, openErr
		}
		names, namesErr := directory.childNames()
		directory.close()
		if namesErr != nil {
			return status, namesErr
		}
		*spec.out = names
	}
	if current == "" {
		return status, nil
	}
	if _, err := r.OpenRelease(ctx, ref, current); err != nil {
		status.RevocationState = "revoked_or_invalid"
		return status, err
	}
	status.RevocationState = "verified"
	return status, nil
}

func (r *Repository) Publish(ctx context.Context, ref ProjectRef, snapshotID, expectedCurrent string) (Manifest, error) {
	if err := validateLifecycleIdentifiers(ctx, snapshotID, expectedCurrent); err != nil {
		return Manifest{}, err
	}
	// This first read preserves revocation precedence over a stale CAS value.
	if _, _, err := r.openPublishCandidate(ctx, ref, snapshotID, expectedCurrent); err != nil {
		return Manifest{}, err
	}

	project, err := openProjectHandle(r.root, ref, false)
	if err != nil {
		return Manifest{}, err
	}
	defer project.close()
	lock, err := acquireProjectLock(ctx, project.dir)
	if err != nil {
		return Manifest{}, err
	}
	defer lock.release()
	staging, err := project.dir.openChildDirectory("staging", false, false)
	if err != nil {
		return Manifest{}, err
	}
	defer staging.close()
	releases, err := project.dir.openChildDirectory("releases", false, false)
	if err != nil {
		return Manifest{}, err
	}
	defer releases.close()

	release, staged, err := r.publishCandidateAt(ctx, ref, project, staging, releases, snapshotID, expectedCurrent)
	if err != nil {
		return Manifest{}, err
	}
	current, err := currentAt(ctx, project.dir)
	if err != nil {
		return Manifest{}, err
	}
	if current != expectedCurrent {
		return Manifest{}, ErrCurrentConflict
	}
	if staged {
		if r.beforeReleaseRename != nil {
			r.beforeReleaseRename()
		}
		if err := r.renameInjected(snapshotID, snapshotID); err != nil {
			return Manifest{}, err
		}
		if err := renameNoReplaceAt(staging.fd, snapshotID, releases.fd, snapshotID); err != nil {
			return Manifest{}, classifyRenameError(err)
		}
		if err := syncSecureDirectory(staging); err != nil {
			return Manifest{}, err
		}
		if err := syncSecureDirectory(releases); err != nil {
			return Manifest{}, err
		}
		release.Root = filepath.Join(project.dir.path, "releases", snapshotID)
	}
	if r.beforeCurrentCommit != nil {
		r.beforeCurrentCommit()
	}
	// Retractions are checked at the final commit boundary while holding the
	// shared project lock used by the operator ledger writer.
	finalRelease, err := r.openSnapshotChild(ctx, ref, releases, snapshotID, release.Root)
	if err != nil {
		return Manifest{}, err
	}
	watermark, err := r.ledger.Watermark(ctx, ref)
	if err != nil {
		return Manifest{}, err
	}
	if watermark != finalRelease.Manifest.RetractionWatermark {
		return Manifest{}, ErrRetractionChanged
	}
	if err := project.verify(); err != nil {
		return Manifest{}, err
	}
	if err := lock.verify(); err != nil {
		return Manifest{}, err
	}
	if err := r.replaceCurrentAt(project.dir, snapshotID); err != nil {
		return Manifest{}, err
	}
	if err := project.verify(); err != nil {
		return Manifest{}, ErrCurrentDurabilityUnknown
	}
	return finalRelease.Manifest, nil
}

func (r *Repository) Rollback(ctx context.Context, ref ProjectRef, snapshotID, expectedCurrent string) (Manifest, error) {
	if err := validateLifecycleIdentifiers(ctx, snapshotID, expectedCurrent); err != nil {
		return Manifest{}, err
	}
	// This first read preserves revocation precedence over a stale CAS value.
	if _, err := r.OpenRelease(ctx, ref, snapshotID); err != nil {
		return Manifest{}, err
	}
	project, err := openProjectHandle(r.root, ref, false)
	if err != nil {
		return Manifest{}, err
	}
	defer project.close()
	lock, err := acquireProjectLock(ctx, project.dir)
	if err != nil {
		return Manifest{}, err
	}
	defer lock.release()
	releases, err := project.dir.openChildDirectory("releases", false, false)
	if err != nil {
		return Manifest{}, err
	}
	defer releases.close()
	release, err := r.openSnapshotChild(ctx, ref, releases, snapshotID, filepath.Join(project.dir.path, "releases", snapshotID))
	if err != nil {
		return Manifest{}, err
	}
	current, err := currentAt(ctx, project.dir)
	if err != nil {
		return Manifest{}, err
	}
	if current != expectedCurrent {
		return Manifest{}, ErrCurrentConflict
	}
	if r.beforeCurrentCommit != nil {
		r.beforeCurrentCommit()
	}
	release, err = r.openSnapshotChild(ctx, ref, releases, snapshotID, release.Root)
	if err != nil {
		return Manifest{}, err
	}
	watermark, err := r.ledger.Watermark(ctx, ref)
	if err != nil {
		return Manifest{}, err
	}
	if watermark != release.Manifest.RetractionWatermark {
		return Manifest{}, ErrRetractionChanged
	}
	if err := project.verify(); err != nil {
		return Manifest{}, err
	}
	if err := lock.verify(); err != nil {
		return Manifest{}, err
	}
	if err := r.replaceCurrentAt(project.dir, snapshotID); err != nil {
		return Manifest{}, err
	}
	if err := project.verify(); err != nil {
		return Manifest{}, ErrCurrentDurabilityUnknown
	}
	return release.Manifest, nil
}

func (r *Repository) projectRoot(ref ProjectRef) (string, error) {
	if r == nil || strings.TrimSpace(r.root) == "" || r.ledger == nil {
		return "", fmt.Errorf("brain repository is invalid: %w", ErrUnsafePath)
	}
	return resolveProjectRoot(r.root, ref)
}

func (r *Repository) openPublishCandidate(ctx context.Context, ref ProjectRef, snapshotID, expectedCurrent string) (Release, bool, error) {
	project, err := openProjectHandle(r.root, ref, false)
	if err != nil {
		return Release{}, false, err
	}
	defer project.close()
	staging, err := project.dir.openChildDirectory("staging", false, false)
	if err != nil {
		return Release{}, false, err
	}
	defer staging.close()
	releases, err := project.dir.openChildDirectory("releases", false, false)
	if err != nil {
		return Release{}, false, err
	}
	defer releases.close()
	return r.publishCandidateAt(ctx, ref, project, staging, releases, snapshotID, expectedCurrent)
}

func (r *Repository) publishCandidateAt(ctx context.Context, ref ProjectRef, project *projectHandle, staging, releases *secureDir, snapshotID, expectedCurrent string) (Release, bool, error) {
	staged, err := staging.childExists(snapshotID)
	if err != nil {
		return Release{}, false, err
	}
	base := releases
	root := filepath.Join(project.dir.path, "releases", snapshotID)
	if staged {
		base = staging
		root = filepath.Join(project.dir.path, "staging", snapshotID)
	}
	release, err := r.openSnapshotChild(ctx, ref, base, snapshotID, root)
	if err != nil {
		return Release{}, false, err
	}
	if release.Manifest.ExpectedCurrent != expectedCurrent {
		return Release{}, false, ErrCurrentConflict
	}
	watermark, err := r.ledger.Watermark(ctx, ref)
	if err != nil {
		return Release{}, false, err
	}
	if watermark != release.Manifest.RetractionWatermark {
		return Release{}, false, ErrRetractionChanged
	}
	if err := project.verify(); err != nil {
		return Release{}, false, err
	}
	return release, staged, nil
}

func (r *Repository) openSnapshotChild(ctx context.Context, ref ProjectRef, parent *secureDir, snapshotID, root string) (Release, error) {
	snapshot, err := parent.openChildDirectory(snapshotID, false, false)
	if errors.Is(err, errSecurePathMissing) {
		return Release{}, ErrSnapshotNotFound
	}
	if err != nil {
		return Release{}, err
	}
	defer snapshot.close()
	manifestBytes, err := readRegularFileAt(snapshot, "manifest.json", maxSnapshotManifestBytes)
	if err != nil {
		if errors.Is(err, ErrUnsafePath) {
			return Release{}, err
		}
		return Release{}, fmt.Errorf("read brain snapshot manifest: %w", ErrSnapshotCorrupt)
	}
	var manifest Manifest
	decoder := json.NewDecoder(bytes.NewReader(manifestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil || requireJSONEOF(decoder) != nil {
		return Release{}, fmt.Errorf("decode brain snapshot manifest: %w", ErrSnapshotCorrupt)
	}
	if err := validateStoredManifest(ref, snapshotID, manifest); err != nil {
		return Release{}, err
	}
	evidenceBytes, err := readRegularFileAt(snapshot, "evidence.jsonl", maxSnapshotEvidenceBytes)
	if err != nil {
		if errors.Is(err, ErrUnsafePath) {
			return Release{}, err
		}
		return Release{}, fmt.Errorf("read brain snapshot evidence: %w", ErrSnapshotCorrupt)
	}
	evidence, err := decodeEvidence(ctx, evidenceBytes)
	if err != nil {
		return Release{}, err
	}
	if err := verifySnapshotFilesAt(ctx, snapshot, manifest.FileHashes, len(manifestBytes)); err != nil {
		return Release{}, err
	}
	for _, record := range evidence {
		if err := ctx.Err(); err != nil {
			return Release{}, err
		}
		retracted, err := r.ledger.Contains(ctx, ref, record.URI)
		if err != nil {
			return Release{}, err
		}
		if retracted {
			return Release{}, ErrSnapshotRevoked
		}
	}
	if err := parent.verifyChildIdentity(snapshotID, snapshot); err != nil {
		return Release{}, err
	}
	return Release{Root: root, Manifest: manifest}, nil
}

func (r *Repository) replaceCurrentAt(project *secureDir, snapshotID string) error {
	temporaryName, file, err := createTemporaryCurrent(project)
	if err != nil {
		return err
	}
	temporaryExists := true
	defer func() {
		if temporaryExists {
			_ = unix.Unlinkat(project.fd, temporaryName, 0)
		}
	}()
	if _, err := io.WriteString(file, snapshotID+"\n"); err != nil {
		file.Close()
		return fmt.Errorf("write brain current snapshot: %w", ErrSnapshotCorrupt)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("sync brain current snapshot: %w", ErrSnapshotCorrupt)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close brain current snapshot: %w", ErrSnapshotCorrupt)
	}
	if r.syncCurrentBeforeRename != nil {
		if err := r.syncCurrentBeforeRename(); err != nil {
			return ErrSnapshotCorrupt
		}
	}
	if err := unix.Fsync(project.fd); err != nil {
		return ErrSnapshotCorrupt
	}
	if err := r.renameInjected(temporaryName, "CURRENT"); err != nil {
		return err
	}
	if err := unix.Renameat(project.fd, temporaryName, project.fd, "CURRENT"); err != nil {
		return classifyRenameError(err)
	}
	temporaryExists = false
	if r.syncCurrentParent != nil {
		if err := r.syncCurrentParent(); err != nil {
			return ErrCurrentDurabilityUnknown
		}
	}
	if err := unix.Fsync(project.fd); err != nil {
		return ErrCurrentDurabilityUnknown
	}
	return nil
}

func (r *Repository) renameInjected(from, to string) error {
	if r.rename == nil {
		return nil
	}
	if err := r.rename(from, to); err != nil {
		return classifyRenameError(err)
	}
	return nil
}

func prepareSnapshot(ref ProjectRef, draft SnapshotDraft) (preparedSnapshot, error) {
	if err := validateDraftScope(ref, draft); err != nil {
		return preparedSnapshot{}, err
	}
	return prepareSnapshotContent(draft)
}

// prepareSnapshotContent derives the exact bounded content that CreateStage
// writes after scope and publication gates have passed. Validation reuses this
// helper with a publishable candidate so it cannot accept a draft that staging
// later rejects after adding evidence.jsonl and canonical file hashes.
func prepareSnapshotContent(draft SnapshotDraft) (preparedSnapshot, error) {
	if len(draft.Files)+1 > maxSnapshotFiles || len(draft.Evidence) > maxSnapshotEvidenceRecords {
		return preparedSnapshot{}, ErrSnapshotTooLarge
	}
	if manifestFieldsTooLarge(draft.Manifest) {
		return preparedSnapshot{}, ErrSnapshotTooLarge
	}
	fileNames := make([]string, 0, len(draft.Files))
	totalBytes := 0
	for name, content := range draft.Files {
		if len(name) > maxSnapshotPathBytes || !safeRelativePath(name) {
			return preparedSnapshot{}, fmt.Errorf("brain snapshot file name is unsafe: %w", ErrUnsafePath)
		}
		if len(content) > maxSnapshotFileBytes || totalBytes > maxSnapshotTreeBytes-len(content) {
			return preparedSnapshot{}, ErrSnapshotTooLarge
		}
		totalBytes += len(content)
		fileNames = append(fileNames, name)
	}
	sort.Strings(fileNames)
	evidenceBytes, err := encodeEvidence(draft.Evidence)
	if err != nil {
		return preparedSnapshot{}, err
	}
	if totalBytes > maxSnapshotTreeBytes-len(evidenceBytes) {
		return preparedSnapshot{}, ErrSnapshotTooLarge
	}
	totalBytes += len(evidenceBytes)
	manifest := draft.Manifest
	manifest.FileHashes = make(map[string]string, len(draft.Files)+1)
	for _, name := range fileNames {
		manifest.FileHashes[filepath.ToSlash(filepath.Join("wiki", name))] = digestBytes(draft.Files[name])
	}
	manifest.FileHashes["evidence.jsonl"] = digestBytes(evidenceBytes)
	manifestBytes, err := encodeManifest(manifest)
	if err != nil {
		return preparedSnapshot{}, err
	}
	if len(manifestBytes) > maxSnapshotManifestBytes || totalBytes > maxSnapshotTreeBytes-len(manifestBytes) {
		return preparedSnapshot{}, ErrSnapshotTooLarge
	}
	return preparedSnapshot{manifest: manifest, manifestBytes: manifestBytes, evidenceBytes: evidenceBytes, fileNames: fileNames}, nil
}

func manifestFieldsTooLarge(manifest Manifest) bool {
	stringsToCheck := []string{
		manifest.SnapshotID, manifest.ParentID, manifest.TenantID, manifest.ProjectID,
		manifest.ExpectedCurrent, manifest.RetractionWatermark, manifest.Model,
		manifest.PromptVersion, manifest.ConfigDigest,
	}
	for _, value := range stringsToCheck {
		if len(value) > maxSnapshotEvidenceFieldLen {
			return true
		}
	}
	if len(manifest.SourceIDs) > maxSnapshotEvidenceRecords || len(manifest.SourceHashes) > maxSnapshotEvidenceRecords || len(manifest.Validation.Findings) > maxSnapshotFiles {
		return true
	}
	for _, value := range append(append([]string(nil), manifest.SourceIDs...), manifest.SourceHashes...) {
		if len(value) > maxSnapshotEvidenceFieldLen {
			return true
		}
	}
	for _, finding := range manifest.Validation.Findings {
		if len(finding.Code) > maxSnapshotEvidenceFieldLen || len(finding.Path) > maxSnapshotEvidenceFieldLen || len(finding.Message) > maxSnapshotEvidenceFieldLen {
			return true
		}
	}
	return false
}

func validateLifecycleIdentifiers(ctx context.Context, snapshotID, expectedCurrent string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !safeSingleComponent(snapshotID) || expectedCurrent != "" && !safeSingleComponent(expectedCurrent) {
		return fmt.Errorf("brain lifecycle identifier is unsafe: %w", ErrUnsafePath)
	}
	return nil
}

func validateDraftScope(ref ProjectRef, draft SnapshotDraft) error {
	manifest := draft.Manifest
	if !safeSingleComponent(manifest.SnapshotID) || manifest.ParentID != "" && !safeSingleComponent(manifest.ParentID) || manifest.ExpectedCurrent != "" && !safeSingleComponent(manifest.ExpectedCurrent) {
		return fmt.Errorf("brain snapshot manifest identifier is unsafe: %w", ErrUnsafePath)
	}
	if manifest.TenantID != ref.TenantID || manifest.ProjectID != ref.ProjectID {
		return fmt.Errorf("brain snapshot manifest scope mismatch: %w", ErrSnapshotCorrupt)
	}
	if manifest.ParentID != manifest.ExpectedCurrent {
		return fmt.Errorf("brain snapshot parent mismatch: %w", ErrSnapshotCorrupt)
	}
	if !manifest.Validation.Publishable {
		return ErrSnapshotUnverified
	}
	return nil
}

func validateStoredManifest(ref ProjectRef, snapshotID string, manifest Manifest) error {
	if manifest.SnapshotID != snapshotID || manifest.TenantID != ref.TenantID || manifest.ProjectID != ref.ProjectID {
		return fmt.Errorf("brain snapshot manifest scope mismatch: %w", ErrSnapshotCorrupt)
	}
	if manifest.ParentID != "" && !safeSingleComponent(manifest.ParentID) || manifest.ExpectedCurrent != "" && !safeSingleComponent(manifest.ExpectedCurrent) || manifest.ParentID != manifest.ExpectedCurrent {
		return fmt.Errorf("brain snapshot manifest lineage is invalid: %w", ErrSnapshotCorrupt)
	}
	if !manifest.Validation.Publishable {
		return ErrSnapshotUnverified
	}
	if len(manifest.FileHashes) == 0 {
		return ErrSnapshotCorrupt
	}
	for name, digest := range manifest.FileHashes {
		if !safeRelativePath(filepath.FromSlash(name)) || !validSHA256Digest(digest) {
			return ErrSnapshotCorrupt
		}
	}
	if _, ok := manifest.FileHashes["evidence.jsonl"]; !ok {
		return ErrSnapshotCorrupt
	}
	return nil
}

func encodeEvidence(records []EvidenceRecord) ([]byte, error) {
	if len(records) > maxSnapshotEvidenceRecords {
		return nil, ErrSnapshotTooLarge
	}
	copyOfRecords := append([]EvidenceRecord(nil), records...)
	sort.Slice(copyOfRecords, func(i, j int) bool {
		if copyOfRecords[i].URI != copyOfRecords[j].URI {
			return copyOfRecords[i].URI < copyOfRecords[j].URI
		}
		return copyOfRecords[i].ID < copyOfRecords[j].ID
	})
	var output bytes.Buffer
	for _, record := range copyOfRecords {
		if len(record.Content) > maxEvidenceBytes || evidenceRecordFieldsTooLarge(record) {
			return nil, ErrSnapshotTooLarge
		}
		encoded, err := json.Marshal(record)
		if err != nil {
			return nil, fmt.Errorf("encode brain snapshot evidence: %w", ErrSnapshotCorrupt)
		}
		if len(encoded)+1 > maxEvidenceLineBytes || output.Len() > maxSnapshotEvidenceBytes-len(encoded)-1 {
			return nil, ErrSnapshotTooLarge
		}
		output.Write(encoded)
		output.WriteByte('\n')
	}
	return output.Bytes(), nil
}

func evidenceRecordFieldsTooLarge(record EvidenceRecord) bool {
	return len(record.ID) > maxSnapshotEvidenceFieldLen || len(record.URI) > maxSnapshotEvidenceFieldLen || len(record.TaskID) > maxSnapshotEvidenceFieldLen || len(record.TraceStep) > maxSnapshotEvidenceFieldLen || len(record.ContentHash) > maxSnapshotEvidenceFieldLen
}

func encodeManifest(manifest Manifest) ([]byte, error) {
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("encode brain snapshot manifest: %w", ErrSnapshotCorrupt)
	}
	return append(encoded, '\n'), nil
}

func decodeEvidence(ctx context.Context, content []byte) ([]EvidenceRecord, error) {
	if len(content) > maxSnapshotEvidenceBytes || len(content) > 0 && content[len(content)-1] != '\n' {
		return nil, ErrSnapshotCorrupt
	}
	var records []EvidenceRecord
	scanner := bufio.NewScanner(bytes.NewReader(content))
	scanner.Buffer(make([]byte, 4096), maxEvidenceLineBytes)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.DisallowUnknownFields()
		var record EvidenceRecord
		if err := decoder.Decode(&record); err != nil || requireJSONEOF(decoder) != nil || record.URI == "" {
			return nil, ErrSnapshotCorrupt
		}
		records = append(records, record)
		if len(records) > maxSnapshotEvidenceRecords {
			return nil, ErrSnapshotTooLarge
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, ErrSnapshotCorrupt
	}
	return records, nil
}

func verifySnapshotFilesAt(ctx context.Context, snapshot *secureDir, hashes map[string]string, manifestBytes int) error {
	actual := make(map[string]string, len(hashes))
	if manifestBytes < 0 || manifestBytes > maxSnapshotManifestBytes || manifestBytes > maxSnapshotTreeBytes {
		return ErrSnapshotTooLarge
	}
	totalBytes := manifestBytes
	fileCount := 0
	if err := walkSnapshotFiles(ctx, snapshot, "", func(name string, content []byte) error {
		if name == "manifest.json" {
			return nil
		}
		fileCount++
		if fileCount > maxSnapshotFiles || totalBytes > maxSnapshotTreeBytes-len(content) {
			return ErrSnapshotTooLarge
		}
		totalBytes += len(content)
		want, ok := hashes[name]
		if !ok || !validSHA256Digest(want) || digestBytes(content) != want {
			return ErrSnapshotCorrupt
		}
		actual[name] = want
		return nil
	}); err != nil {
		return err
	}
	if len(actual) != len(hashes) {
		return ErrSnapshotCorrupt
	}
	for name := range hashes {
		if _, ok := actual[name]; !ok {
			return ErrSnapshotCorrupt
		}
	}
	return nil
}

func walkSnapshotFiles(ctx context.Context, directory *secureDir, prefix string, visit func(string, []byte) error) error {
	names, err := directory.childNames()
	if err != nil {
		return err
	}
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		var stat unix.Stat_t
		if err := unix.Fstatat(directory.fd, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return ErrSnapshotCorrupt
		}
		relative := name
		if prefix != "" {
			relative = prefix + "/" + name
		}
		switch stat.Mode & unix.S_IFMT {
		case unix.S_IFDIR:
			child, err := directory.openChildDirectory(name, false, false)
			if err != nil {
				return err
			}
			err = walkSnapshotFiles(ctx, child, relative, visit)
			if err == nil {
				err = directory.verifyChildIdentity(name, child)
			}
			child.close()
			if err != nil {
				return err
			}
		case unix.S_IFREG:
			limit := int64(maxSnapshotFileBytes)
			if relative == "manifest.json" {
				limit = maxSnapshotManifestBytes
			} else if relative == "evidence.jsonl" {
				limit = maxSnapshotEvidenceBytes
			}
			content, err := readRegularFileAt(directory, name, limit)
			if err != nil {
				return err
			}
			if err := visit(relative, content); err != nil {
				return err
			}
		default:
			return ErrUnsafePath
		}
	}
	return nil
}

func openAbsoluteDirectory(path string, create bool) (*secureDir, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, ErrUnsafePath
	}
	absPath = filepath.Clean(absPath)
	root, components := absolutePathComponents(absPath)
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, ErrUnsafePath
	}
	currentPath := root
	for _, component := range components {
		if component == "" || component == "." || component == ".." {
			unix.Close(fd)
			return nil, ErrUnsafePath
		}
		nextFD, openErr := unix.Openat(fd, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if errors.Is(openErr, unix.ENOENT) && create {
			made := unix.Mkdirat(fd, component, 0o700)
			if made != nil && !errors.Is(made, unix.EEXIST) {
				unix.Close(fd)
				return nil, ErrSnapshotCorrupt
			}
			if made == nil && unix.Fsync(fd) != nil {
				unix.Close(fd)
				return nil, ErrSnapshotCorrupt
			}
			nextFD, openErr = unix.Openat(fd, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		}
		if errors.Is(openErr, unix.ENOENT) {
			unix.Close(fd)
			return nil, errSecurePathMissing
		}
		if openErr != nil {
			unix.Close(fd)
			return nil, ErrUnsafePath
		}
		unix.Close(fd)
		fd = nextFD
		currentPath = filepath.Join(currentPath, component)
	}
	if err := requireDirectoryFD(fd); err != nil {
		unix.Close(fd)
		return nil, err
	}
	return &secureDir{fd: fd, path: currentPath}, nil
}

func openProjectHandle(root string, ref ProjectRef, create bool) (*projectHandle, error) {
	if _, err := resolveProjectRoot(root, ref); err != nil {
		return nil, err
	}
	rootDirectory, err := openAbsoluteDirectory(root, false)
	if err != nil {
		return nil, err
	}
	defer rootDirectory.close()
	tenant, err := rootDirectory.openChildDirectory(ref.StorageKey, create, false)
	if err != nil {
		return nil, err
	}
	project, err := tenant.openChildDirectory(ref.ProjectID, create, false)
	if err != nil {
		tenant.close()
		return nil, err
	}
	return &projectHandle{dir: project, tenant: tenant, projectName: ref.ProjectID}, nil
}

func (p *projectHandle) close() {
	if p == nil {
		return
	}
	p.dir.close()
	p.tenant.close()
}

func (p *projectHandle) verify() error {
	return p.tenant.verifyChildIdentity(p.projectName, p.dir)
}

func (d *secureDir) close() {
	if d != nil && d.fd >= 0 {
		_ = unix.Close(d.fd)
		d.fd = -1
	}
}

func (d *secureDir) openChildDirectory(name string, create, exclusive bool) (*secureDir, error) {
	if d == nil || d.fd < 0 || !safeSingleComponent(name) {
		return nil, ErrUnsafePath
	}
	if create {
		err := unix.Mkdirat(d.fd, name, 0o700)
		if errors.Is(err, unix.EEXIST) && exclusive {
			return nil, ErrSnapshotExists
		}
		if err != nil && !errors.Is(err, unix.EEXIST) {
			return nil, ErrSnapshotCorrupt
		}
		if err == nil && unix.Fsync(d.fd) != nil {
			return nil, ErrSnapshotCorrupt
		}
	}
	fd, err := unix.Openat(d.fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, errSecurePathMissing
	}
	if err != nil {
		return nil, ErrUnsafePath
	}
	if err := requireDirectoryFD(fd); err != nil {
		unix.Close(fd)
		return nil, err
	}
	return &secureDir{fd: fd, path: filepath.Join(d.path, name)}, nil
}

func (d *secureDir) childExists(name string) (bool, error) {
	if !safeSingleComponent(name) {
		return false, ErrUnsafePath
	}
	var stat unix.Stat_t
	err := unix.Fstatat(d.fd, name, &stat, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	if err != nil {
		return false, ErrUnsafePath
	}
	if stat.Mode&unix.S_IFMT == unix.S_IFLNK {
		return false, ErrUnsafePath
	}
	return true, nil
}

func (d *secureDir) verifyChildIdentity(name string, child *secureDir) error {
	var named, opened unix.Stat_t
	if err := unix.Fstatat(d.fd, name, &named, unix.AT_SYMLINK_NOFOLLOW); err != nil || named.Mode&unix.S_IFMT != unix.S_IFDIR {
		return ErrUnsafePath
	}
	if err := unix.Fstat(child.fd, &opened); err != nil || opened.Mode&unix.S_IFMT != unix.S_IFDIR {
		return ErrUnsafePath
	}
	if named.Dev != opened.Dev || named.Ino != opened.Ino {
		return ErrUnsafePath
	}
	return nil
}

func (d *secureDir) childNames() ([]string, error) {
	fd, err := unix.Dup(d.fd)
	if err != nil {
		return nil, ErrSnapshotCorrupt
	}
	file := os.NewFile(uintptr(fd), "brain-directory")
	entries, err := file.ReadDir(-1)
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		return nil, ErrSnapshotCorrupt
	}
	names := make([]string, len(entries))
	for index, entry := range entries {
		names[index] = entry.Name()
	}
	sort.Strings(names)
	return names, nil
}

func requireDirectoryFD(fd int) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return ErrUnsafePath
	}
	return nil
}

func acquireProjectLock(ctx context.Context, directory *secureDir) (*projectLock, error) {
	if directory == nil || directory.fd < 0 {
		return nil, ErrUnsafePath
	}
	fd, err := unix.Openat(directory.fd, ".publish.lock", unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, ErrUnsafePath
	}
	lock := &projectLock{directory: directory, fd: fd}
	if err := lock.verify(); err != nil {
		lock.release()
		return nil, err
	}
	if err := unix.Fsync(directory.fd); err != nil {
		lock.release()
		return nil, ErrSnapshotCorrupt
	}
	for {
		if err := unix.Flock(lock.fd, unix.LOCK_EX|unix.LOCK_NB); err == nil {
			if err := lock.verify(); err != nil {
				lock.release()
				return nil, err
			}
			return lock, nil
		} else if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			lock.release()
			return nil, ErrSnapshotCorrupt
		}
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			lock.release()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (lock *projectLock) verify() error {
	if lock == nil || lock.directory == nil || lock.directory.fd < 0 || lock.fd < 0 {
		return ErrUnsafePath
	}
	var opened, named unix.Stat_t
	if err := unix.Fstat(lock.fd, &opened); err != nil || opened.Mode&unix.S_IFMT != unix.S_IFREG || opened.Mode&0o077 != 0 {
		return ErrUnsafePath
	}
	if err := unix.Fstatat(lock.directory.fd, ".publish.lock", &named, unix.AT_SYMLINK_NOFOLLOW); err != nil || named.Mode&unix.S_IFMT != unix.S_IFREG || named.Mode&0o077 != 0 {
		return ErrUnsafePath
	}
	if opened.Dev != named.Dev || opened.Ino != named.Ino {
		return ErrUnsafePath
	}
	return nil
}

func (lock *projectLock) release() {
	if lock != nil && lock.fd >= 0 {
		_ = unix.Flock(lock.fd, unix.LOCK_UN)
		_ = unix.Close(lock.fd)
		lock.fd = -1
	}
}

func syncSecureDirectory(directory *secureDir) error {
	if directory == nil || unix.Fsync(directory.fd) != nil {
		return ErrSnapshotCorrupt
	}
	return nil
}

func writeRelativeNewFile(root *secureDir, relative string, content []byte) error {
	if !safeRelativePath(relative) {
		return ErrUnsafePath
	}
	components := strings.Split(filepath.ToSlash(relative), "/")
	currentFD, err := unix.Dup(root.fd)
	if err != nil {
		return ErrSnapshotCorrupt
	}
	current := &secureDir{fd: currentFD, path: root.path}
	defer current.close()
	for _, component := range components[:len(components)-1] {
		next, err := current.openChildDirectory(component, true, false)
		if err != nil {
			return err
		}
		current.close()
		current = next
	}
	return writeNewFileAt(current, components[len(components)-1], content)
}

func writeNewFileAt(parent *secureDir, name string, content []byte) error {
	if !safeSingleComponent(name) {
		return ErrUnsafePath
	}
	fd, err := unix.Openat(parent.fd, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if errors.Is(err, unix.EEXIST) {
		return ErrSnapshotExists
	}
	if err != nil {
		return ErrSnapshotCorrupt
	}
	file := os.NewFile(uintptr(fd), "brain-snapshot-file")
	if _, err := file.Write(content); err != nil {
		file.Close()
		return ErrSnapshotCorrupt
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return ErrSnapshotCorrupt
	}
	if err := file.Close(); err != nil {
		return ErrSnapshotCorrupt
	}
	return syncSecureDirectory(parent)
}

func readRegularFileAt(parent *secureDir, name string, limit int64) ([]byte, error) {
	if !safeSingleComponent(name) {
		return nil, ErrUnsafePath
	}
	fd, err := unix.Openat(parent.fd, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, errSecurePathMissing
	}
	if err != nil {
		return nil, ErrUnsafePath
	}
	file := os.NewFile(uintptr(fd), "brain-snapshot-file")
	defer file.Close()
	var before unix.Stat_t
	if err := unix.Fstat(fd, &before); err != nil {
		return nil, ErrSnapshotCorrupt
	}
	if before.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, ErrUnsafePath
	}
	if before.Size > limit {
		return nil, ErrSnapshotCorrupt
	}
	content, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(content)) > limit || int64(len(content)) != before.Size {
		return nil, ErrSnapshotCorrupt
	}
	var after unix.Stat_t
	if err := unix.Fstat(fd, &after); err != nil || before.Dev != after.Dev || before.Ino != after.Ino || before.Size != after.Size || before.Mtim != after.Mtim || before.Ctim != after.Ctim {
		return nil, ErrSnapshotCorrupt
	}
	return content, nil
}

func currentAt(ctx context.Context, project *secureDir) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	content, err := readRegularFileAt(project, "CURRENT", maxCurrentBytes)
	if errors.Is(err, errSecurePathMissing) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read brain current snapshot: %w", ErrSnapshotCorrupt)
	}
	value := string(content)
	if strings.HasSuffix(value, "\n") {
		value = strings.TrimSuffix(value, "\n")
	}
	if !safeSingleComponent(value) {
		return "", ErrSnapshotCorrupt
	}
	return value, nil
}

func createTemporaryCurrent(project *secureDir) (string, *os.File, error) {
	for range 32 {
		random := make([]byte, 12)
		if _, err := rand.Read(random); err != nil {
			return "", nil, ErrSnapshotCorrupt
		}
		name := ".CURRENT.tmp-" + hex.EncodeToString(random)
		fd, err := unix.Openat(project.fd, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return "", nil, ErrSnapshotCorrupt
		}
		return name, os.NewFile(uintptr(fd), "brain-current-temporary"), nil
	}
	return "", nil, ErrSnapshotCorrupt
}

func classifyRenameError(err error) error {
	switch {
	case errors.Is(err, unix.EXDEV):
		return ErrCrossFilesystem
	case errors.Is(err, unix.EEXIST), errors.Is(err, unix.ENOTEMPTY):
		return ErrSnapshotExists
	default:
		return ErrSnapshotCorrupt
	}
}

func resolveProjectRoot(root string, ref ProjectRef) (string, error) {
	if strings.TrimSpace(root) == "" || strings.TrimSpace(ref.TenantID) == "" || strings.TrimSpace(ref.TenantID) != ref.TenantID || !isProjectSlug(ref.ProjectID) || !safeStorageKey(ref.StorageKey) {
		return "", fmt.Errorf("brain project scope is unsafe: %w", ErrUnsafePath)
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", ErrUnsafePath
	}
	tenantRoot, err := safeJoin(filepath.Clean(absRoot), ref.StorageKey)
	if err != nil {
		return "", err
	}
	return safeJoin(tenantRoot, ref.ProjectID)
}

func safeStorageKey(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) <= len("sha256:") || strings.TrimSpace(value) != value || strings.ContainsAny(value, "/\\") {
		return false
	}
	return strings.IndexFunc(value, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) == -1
}

func safeSingleComponent(value string) bool {
	return value != "" && value != "." && value != ".." && !filepath.IsAbs(value) && filepath.VolumeName(value) == "" && !strings.ContainsAny(value, "/\\") && strings.TrimSpace(value) == value && strings.IndexFunc(value, unicode.IsControl) == -1
}

func safeRelativePath(value string) bool {
	if value == "" || filepath.IsAbs(value) || filepath.VolumeName(value) != "" || strings.Contains(value, "\\") || filepath.Clean(value) != value {
		return false
	}
	for _, component := range strings.Split(filepath.ToSlash(value), "/") {
		if !safeSingleComponent(component) {
			return false
		}
	}
	return true
}

func safeJoin(root, relative string) (string, error) {
	if !safeRelativePath(relative) {
		return "", ErrUnsafePath
	}
	joined := filepath.Join(root, relative)
	rel, err := filepath.Rel(root, joined)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", ErrUnsafePath
	}
	return joined, nil
}

func absolutePathComponents(path string) (string, []string) {
	volume := filepath.VolumeName(path)
	root := volume + string(filepath.Separator)
	remainder := strings.TrimPrefix(path, root)
	if remainder == "" {
		return root, nil
	}
	return root, strings.Split(remainder, string(filepath.Separator))
}

func validSHA256Digest(value string) bool {
	if len(value) != len("sha256:")+sha256.Size*2 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, character := range value[len("sha256:"):] {
		if character < '0' || character > '9' && character < 'a' || character > 'f' {
			return false
		}
	}
	return true
}

func digestBytes(content []byte) string {
	digest := sha256.Sum256(content)
	return fmt.Sprintf("sha256:%x", digest)
}
