package brain

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/wuxujun/ai-agent/internal/policy"
)

const (
	maxCurrentBytes      = 4096
	maxSnapshotFileBytes = 16 * 1024 * 1024
	maxSnapshotTreeBytes = 64 * 1024 * 1024
	maxEvidenceLineBytes = 2 * 1024 * 1024
)

var (
	ErrUnsafePath         = errors.New("unsafe brain repository path")
	ErrCurrentConflict    = errors.New("brain current snapshot conflict")
	ErrSnapshotExists     = errors.New("brain snapshot already exists")
	ErrSnapshotNotFound   = errors.New("brain snapshot not found")
	ErrSnapshotRevoked    = errors.New("brain snapshot revoked")
	ErrRetractionChanged  = errors.New("brain retraction watermark changed")
	ErrSnapshotUnverified = errors.New("brain snapshot is not verified")
	ErrSnapshotCorrupt    = errors.New("brain snapshot is corrupt")
	ErrCrossFilesystem    = errors.New("brain snapshot cross-filesystem rename rejected")
)

// Repository persists complete Brain snapshots under one configured root.
// Publication is serialized with a filesystem-visible advisory lock and uses
// an expected-current compare-and-swap before replacing CURRENT atomically.
type Repository struct {
	root   string
	ledger RetractionView
	rename func(string, string) error
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
	if err := ensurePrivateDirectory(absRoot); err != nil {
		return nil, err
	}
	return &Repository{root: absRoot, ledger: ledger, rename: os.Rename}, nil
}

func (r *Repository) Current(ctx context.Context, ref ProjectRef) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	projectRoot, err := r.projectRoot(ref)
	if err != nil {
		return "", err
	}
	currentPath, err := safeJoin(projectRoot, "CURRENT")
	if err != nil {
		return "", err
	}
	exists, err := inspectPathComponents(currentPath)
	if err != nil {
		return "", err
	}
	if !exists {
		return "", nil
	}
	content, err := readBoundedRegularFile(currentPath, maxCurrentBytes)
	if err != nil {
		return "", fmt.Errorf("read brain current snapshot: %w", ErrSnapshotCorrupt)
	}
	value := string(content)
	if strings.HasSuffix(value, "\n") {
		value = strings.TrimSuffix(value, "\n")
	}
	if !safeSingleComponent(value) {
		return "", fmt.Errorf("brain current snapshot is invalid: %w", ErrSnapshotCorrupt)
	}
	return value, nil
}

func (r *Repository) CreateStage(ctx context.Context, ref ProjectRef, draft SnapshotDraft) (Manifest, error) {
	if err := ctx.Err(); err != nil {
		return Manifest{}, err
	}
	if err := validateDraftScope(ref, draft); err != nil {
		return Manifest{}, err
	}
	fileNames := make([]string, 0, len(draft.Files))
	for name := range draft.Files {
		if !safeRelativePath(name) {
			return Manifest{}, fmt.Errorf("brain snapshot file name is unsafe: %w", ErrUnsafePath)
		}
		fileNames = append(fileNames, name)
	}
	sort.Strings(fileNames)
	evidenceBytes, err := encodeEvidence(draft.Evidence)
	if err != nil {
		return Manifest{}, err
	}
	manifest := draft.Manifest
	manifest.FileHashes = make(map[string]string, len(draft.Files)+1)
	for _, name := range fileNames {
		manifest.FileHashes[filepath.ToSlash(filepath.Join("wiki", name))] = digestBytes(draft.Files[name])
	}
	manifest.FileHashes["evidence.jsonl"] = digestBytes(evidenceBytes)
	manifestBytes, err := encodeManifest(manifest)
	if err != nil {
		return Manifest{}, err
	}

	projectRoot, err := r.projectRoot(ref)
	if err != nil {
		return Manifest{}, err
	}
	if err := ensurePrivateDirectory(projectRoot); err != nil {
		return Manifest{}, err
	}
	stagingRoot, err := safeJoin(projectRoot, "staging")
	if err != nil {
		return Manifest{}, err
	}
	releasesRoot, err := safeJoin(projectRoot, "releases")
	if err != nil {
		return Manifest{}, err
	}
	if err := ensurePrivateDirectory(stagingRoot); err != nil {
		return Manifest{}, err
	}
	if err := ensurePrivateDirectory(releasesRoot); err != nil {
		return Manifest{}, err
	}

	lock, err := r.acquireProjectLock(ctx, projectRoot)
	if err != nil {
		return Manifest{}, err
	}
	defer releaseProjectLock(lock)
	stageRoot, err := safeJoin(stagingRoot, manifest.SnapshotID)
	if err != nil {
		return Manifest{}, err
	}
	releaseRoot, err := safeJoin(releasesRoot, manifest.SnapshotID)
	if err != nil {
		return Manifest{}, err
	}
	if exists, pathErr := inspectPathComponents(stageRoot); pathErr != nil {
		return Manifest{}, pathErr
	} else if exists {
		return Manifest{}, ErrSnapshotExists
	}
	if exists, pathErr := inspectPathComponents(releaseRoot); pathErr != nil {
		return Manifest{}, pathErr
	} else if exists {
		return Manifest{}, ErrSnapshotExists
	}
	if err := os.Mkdir(stageRoot, 0o700); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return Manifest{}, ErrSnapshotExists
		}
		return Manifest{}, fmt.Errorf("create brain staging snapshot: %w", ErrSnapshotCorrupt)
	}
	if err := syncDirectory(stagingRoot); err != nil {
		return Manifest{}, err
	}
	wikiRoot, err := safeJoin(stageRoot, "wiki")
	if err != nil {
		return Manifest{}, err
	}
	if err := ensurePrivateDirectory(wikiRoot); err != nil {
		return Manifest{}, err
	}
	for _, name := range fileNames {
		if err := ctx.Err(); err != nil {
			return Manifest{}, err
		}
		path, joinErr := safeJoin(wikiRoot, name)
		if joinErr != nil {
			return Manifest{}, joinErr
		}
		if err := ensurePrivateDirectory(filepath.Dir(path)); err != nil {
			return Manifest{}, err
		}
		if err := writeNewSyncedFile(path, draft.Files[name]); err != nil {
			return Manifest{}, err
		}
	}
	evidencePath, err := safeJoin(stageRoot, "evidence.jsonl")
	if err != nil {
		return Manifest{}, err
	}
	if err := writeNewSyncedFile(evidencePath, evidenceBytes); err != nil {
		return Manifest{}, err
	}
	manifestPath, err := safeJoin(stageRoot, "manifest.json")
	if err != nil {
		return Manifest{}, err
	}
	if err := writeNewSyncedFile(manifestPath, manifestBytes); err != nil {
		return Manifest{}, err
	}
	if err := syncDirectory(stageRoot); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func (r *Repository) OpenRelease(ctx context.Context, ref ProjectRef, snapshotID string) (Release, error) {
	if err := ctx.Err(); err != nil {
		return Release{}, err
	}
	if !safeSingleComponent(snapshotID) {
		return Release{}, fmt.Errorf("brain snapshot identifier is unsafe: %w", ErrUnsafePath)
	}
	projectRoot, err := r.projectRoot(ref)
	if err != nil {
		return Release{}, err
	}
	releasesRoot, err := safeJoin(projectRoot, "releases")
	if err != nil {
		return Release{}, err
	}
	return r.openSnapshot(ctx, ref, releasesRoot, snapshotID)
}

func (r *Repository) Publish(ctx context.Context, ref ProjectRef, snapshotID, expectedCurrent string) (Manifest, error) {
	if err := ctx.Err(); err != nil {
		return Manifest{}, err
	}
	if !safeSingleComponent(snapshotID) || expectedCurrent != "" && !safeSingleComponent(expectedCurrent) {
		return Manifest{}, fmt.Errorf("brain publication identifier is unsafe: %w", ErrUnsafePath)
	}
	projectRoot, err := r.projectRoot(ref)
	if err != nil {
		return Manifest{}, err
	}
	if _, _, err := r.publishCandidate(ctx, ref, projectRoot, snapshotID, expectedCurrent); err != nil {
		return Manifest{}, err
	}

	lock, err := r.acquireProjectLock(ctx, projectRoot)
	if err != nil {
		return Manifest{}, err
	}
	defer releaseProjectLock(lock)
	release, staged, err := r.publishCandidate(ctx, ref, projectRoot, snapshotID, expectedCurrent)
	if err != nil {
		return Manifest{}, err
	}
	current, err := r.Current(ctx, ref)
	if err != nil {
		return Manifest{}, err
	}
	if current != expectedCurrent || release.Manifest.ExpectedCurrent != expectedCurrent {
		return Manifest{}, ErrCurrentConflict
	}
	if staged {
		stagingRoot, _ := safeJoin(projectRoot, "staging")
		releasesRoot, _ := safeJoin(projectRoot, "releases")
		target, _ := safeJoin(releasesRoot, snapshotID)
		if exists, pathErr := inspectPathComponents(target); pathErr != nil {
			return Manifest{}, pathErr
		} else if exists {
			return Manifest{}, ErrSnapshotExists
		}
		if err := r.renameSameFilesystem(release.Root, target); err != nil {
			return Manifest{}, err
		}
		if err := syncDirectory(stagingRoot); err != nil {
			return Manifest{}, err
		}
		if err := syncDirectory(releasesRoot); err != nil {
			return Manifest{}, err
		}
		release.Root = target
	}
	if err := r.replaceCurrent(projectRoot, snapshotID); err != nil {
		return Manifest{}, err
	}
	return release.Manifest, nil
}

func (r *Repository) Rollback(ctx context.Context, ref ProjectRef, snapshotID, expectedCurrent string) (Manifest, error) {
	if err := ctx.Err(); err != nil {
		return Manifest{}, err
	}
	if !safeSingleComponent(snapshotID) || expectedCurrent != "" && !safeSingleComponent(expectedCurrent) {
		return Manifest{}, fmt.Errorf("brain rollback identifier is unsafe: %w", ErrUnsafePath)
	}
	// Check revocation before CAS so a revoked target can never be made current,
	// even when the caller also supplied a stale expected-current value.
	if _, err := r.OpenRelease(ctx, ref, snapshotID); err != nil {
		return Manifest{}, err
	}
	projectRoot, err := r.projectRoot(ref)
	if err != nil {
		return Manifest{}, err
	}
	lock, err := r.acquireProjectLock(ctx, projectRoot)
	if err != nil {
		return Manifest{}, err
	}
	defer releaseProjectLock(lock)
	release, err := r.OpenRelease(ctx, ref, snapshotID)
	if err != nil {
		return Manifest{}, err
	}
	current, err := r.Current(ctx, ref)
	if err != nil {
		return Manifest{}, err
	}
	if current != expectedCurrent {
		return Manifest{}, ErrCurrentConflict
	}
	if err := r.replaceCurrent(projectRoot, snapshotID); err != nil {
		return Manifest{}, err
	}
	return release.Manifest, nil
}

func (r *Repository) projectRoot(ref ProjectRef) (string, error) {
	if r == nil || strings.TrimSpace(r.root) == "" || r.ledger == nil {
		return "", fmt.Errorf("brain repository is invalid: %w", ErrUnsafePath)
	}
	return resolveProjectRoot(r.root, ref)
}

func (r *Repository) publishCandidate(ctx context.Context, ref ProjectRef, projectRoot, snapshotID, expectedCurrent string) (Release, bool, error) {
	stagingRoot, err := safeJoin(projectRoot, "staging")
	if err != nil {
		return Release{}, false, err
	}
	releasesRoot, err := safeJoin(projectRoot, "releases")
	if err != nil {
		return Release{}, false, err
	}
	stagePath, err := safeJoin(stagingRoot, snapshotID)
	if err != nil {
		return Release{}, false, err
	}
	staged, err := inspectPathComponents(stagePath)
	if err != nil {
		return Release{}, false, err
	}
	base := stagingRoot
	if !staged {
		base = releasesRoot
	}
	release, err := r.openSnapshot(ctx, ref, base, snapshotID)
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
	return release, staged, nil
}

func (r *Repository) openSnapshot(ctx context.Context, ref ProjectRef, base, snapshotID string) (Release, error) {
	if err := ctx.Err(); err != nil {
		return Release{}, err
	}
	root, err := safeJoin(base, snapshotID)
	if err != nil {
		return Release{}, err
	}
	exists, err := inspectPathComponents(root)
	if err != nil {
		return Release{}, err
	}
	if !exists {
		return Release{}, ErrSnapshotNotFound
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return Release{}, fmt.Errorf("brain snapshot root is invalid: %w", ErrUnsafePath)
	}
	manifestPath, _ := safeJoin(root, "manifest.json")
	manifestBytes, err := readBoundedRegularFile(manifestPath, maxSnapshotFileBytes)
	if err != nil {
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
	evidencePath, _ := safeJoin(root, "evidence.jsonl")
	evidence, err := readEvidence(ctx, evidencePath)
	if err != nil {
		return Release{}, err
	}
	if err := verifySnapshotFiles(ctx, root, manifest.FileHashes); err != nil {
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
	return Release{Root: root, Manifest: manifest}, nil
}

func (r *Repository) acquireProjectLock(ctx context.Context, projectRoot string) (*os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := ensurePrivateDirectory(projectRoot); err != nil {
		return nil, err
	}
	lockPath, err := safeJoin(projectRoot, ".publish.lock")
	if err != nil {
		return nil, err
	}
	existed, err := inspectPathComponents(lockPath)
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR|policy.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open brain publication lock: %w", ErrUnsafePath)
	}
	if !existed {
		if err := file.Sync(); err != nil {
			file.Close()
			return nil, fmt.Errorf("sync brain publication lock: %w", ErrSnapshotCorrupt)
		}
		if err := syncDirectory(projectRoot); err != nil {
			file.Close()
			return nil, err
		}
	}
	for {
		if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			return file, nil
		} else if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			file.Close()
			return nil, fmt.Errorf("lock brain publication: %w", ErrSnapshotCorrupt)
		}
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			file.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func releaseProjectLock(file *os.File) {
	if file == nil {
		return
	}
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	_ = file.Close()
}

func (r *Repository) replaceCurrent(projectRoot, snapshotID string) error {
	currentPath, err := safeJoin(projectRoot, "CURRENT")
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(projectRoot, ".CURRENT.tmp-")
	if err != nil {
		return fmt.Errorf("create brain current snapshot temporary file: %w", ErrSnapshotCorrupt)
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("secure brain current snapshot temporary file: %w", ErrSnapshotCorrupt)
	}
	if _, err := io.WriteString(temporary, snapshotID+"\n"); err != nil {
		temporary.Close()
		return fmt.Errorf("write brain current snapshot: %w", ErrSnapshotCorrupt)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync brain current snapshot: %w", ErrSnapshotCorrupt)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close brain current snapshot: %w", ErrSnapshotCorrupt)
	}
	if err := r.renameSameFilesystem(temporaryPath, currentPath); err != nil {
		return err
	}
	removeTemporary = false
	return syncDirectory(projectRoot)
}

func (r *Repository) renameSameFilesystem(from, to string) error {
	fromInfo, err := os.Lstat(from)
	if err != nil || fromInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("brain rename source is invalid: %w", ErrUnsafePath)
	}
	toParent := filepath.Dir(to)
	toInfo, err := os.Lstat(toParent)
	if err != nil || !toInfo.IsDir() || toInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("brain rename destination is invalid: %w", ErrUnsafePath)
	}
	fromStat, fromOK := fromInfo.Sys().(*syscall.Stat_t)
	toStat, toOK := toInfo.Sys().(*syscall.Stat_t)
	if !fromOK || !toOK || fromStat.Dev != toStat.Dev {
		return ErrCrossFilesystem
	}
	if err := r.rename(from, to); err != nil {
		if errors.Is(err, syscall.EXDEV) {
			return ErrCrossFilesystem
		}
		return fmt.Errorf("atomically rename brain snapshot: %w", ErrSnapshotCorrupt)
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
	return nil
}

func encodeEvidence(records []EvidenceRecord) ([]byte, error) {
	copyOfRecords := append([]EvidenceRecord(nil), records...)
	sort.Slice(copyOfRecords, func(i, j int) bool {
		if copyOfRecords[i].URI != copyOfRecords[j].URI {
			return copyOfRecords[i].URI < copyOfRecords[j].URI
		}
		return copyOfRecords[i].ID < copyOfRecords[j].ID
	})
	var output bytes.Buffer
	for _, record := range copyOfRecords {
		encoded, err := json.Marshal(record)
		if err != nil {
			return nil, fmt.Errorf("encode brain snapshot evidence: %w", ErrSnapshotCorrupt)
		}
		output.Write(encoded)
		output.WriteByte('\n')
	}
	return output.Bytes(), nil
}

func encodeManifest(manifest Manifest) ([]byte, error) {
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("encode brain snapshot manifest: %w", ErrSnapshotCorrupt)
	}
	return append(encoded, '\n'), nil
}

func readEvidence(ctx context.Context, path string) ([]EvidenceRecord, error) {
	file, info, err := openRegularNoFollow(path)
	if err != nil {
		return nil, fmt.Errorf("open brain snapshot evidence: %w", ErrSnapshotCorrupt)
	}
	defer file.Close()
	if info.Size() > maxSnapshotFileBytes {
		return nil, fmt.Errorf("brain snapshot evidence exceeds limit: %w", ErrSnapshotCorrupt)
	}
	var records []EvidenceRecord
	scanner := bufio.NewScanner(io.LimitReader(file, maxSnapshotFileBytes+1))
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
			return nil, fmt.Errorf("decode brain snapshot evidence: %w", ErrSnapshotCorrupt)
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan brain snapshot evidence: %w", ErrSnapshotCorrupt)
	}
	return records, nil
}

func verifySnapshotFiles(ctx context.Context, root string, hashes map[string]string) error {
	if len(hashes) == 0 {
		return ErrSnapshotCorrupt
	}
	actual := make(map[string]string, len(hashes))
	totalBytes := int64(0)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return ErrSnapshotCorrupt
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return ErrSnapshotCorrupt
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return ErrUnsafePath
		}
		if entry.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return ErrUnsafePath
		}
		relative, err := filepath.Rel(root, path)
		if err != nil || !safeRelativePath(relative) {
			return ErrUnsafePath
		}
		relative = filepath.ToSlash(relative)
		if relative == "manifest.json" {
			return nil
		}
		if info.Size() > maxSnapshotFileBytes {
			return ErrSnapshotCorrupt
		}
		totalBytes += info.Size()
		if totalBytes > maxSnapshotTreeBytes {
			return ErrSnapshotCorrupt
		}
		content, err := readBoundedRegularFile(path, maxSnapshotFileBytes)
		if err != nil {
			return ErrSnapshotCorrupt
		}
		actual[relative] = digestBytes(content)
		return nil
	})
	if err != nil {
		return fmt.Errorf("verify brain snapshot files: %w", err)
	}
	if len(actual) != len(hashes) {
		return ErrSnapshotCorrupt
	}
	for name, want := range hashes {
		if !safeRelativePath(filepath.FromSlash(name)) || actual[name] != want {
			return ErrSnapshotCorrupt
		}
	}
	return nil
}

func resolveProjectRoot(root string, ref ProjectRef) (string, error) {
	if strings.TrimSpace(root) == "" || strings.TrimSpace(ref.TenantID) == "" || strings.TrimSpace(ref.TenantID) != ref.TenantID || !isProjectSlug(ref.ProjectID) || !safeStorageKey(ref.StorageKey) {
		return "", fmt.Errorf("brain project scope is unsafe: %w", ErrUnsafePath)
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve brain project root: %w", ErrUnsafePath)
	}
	absRoot = filepath.Clean(absRoot)
	if _, err := inspectPathComponents(absRoot); err != nil {
		return "", err
	}
	tenantRoot, err := safeJoin(absRoot, ref.StorageKey)
	if err != nil {
		return "", err
	}
	return safeJoin(tenantRoot, ref.ProjectID)
}

func safeStorageKey(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) <= len("sha256:") || strings.TrimSpace(value) != value || strings.ContainsAny(value, "/\\") {
		return false
	}
	return strings.IndexFunc(value, func(r rune) bool {
		return unicode.IsControl(r) || unicode.IsSpace(r)
	}) == -1
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
		return "", fmt.Errorf("brain repository child path is unsafe: %w", ErrUnsafePath)
	}
	joined := filepath.Join(root, relative)
	rel, err := filepath.Rel(root, joined)
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." || filepath.IsAbs(rel) {
		return "", fmt.Errorf("brain repository path escapes root: %w", ErrUnsafePath)
	}
	return joined, nil
}

func inspectPathComponents(path string) (bool, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false, fmt.Errorf("resolve brain repository path: %w", ErrUnsafePath)
	}
	root, components := absolutePathComponents(filepath.Clean(absPath))
	current := root
	for index, component := range components {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("inspect brain repository path: %w", ErrUnsafePath)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return false, fmt.Errorf("brain repository symlink rejected: %w", ErrUnsafePath)
		}
		if index < len(components)-1 && !info.IsDir() {
			return false, fmt.Errorf("brain repository path component is not a directory: %w", ErrUnsafePath)
		}
	}
	return true, nil
}

func ensurePrivateDirectory(path string) error {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve brain repository directory: %w", ErrUnsafePath)
	}
	root, components := absolutePathComponents(filepath.Clean(absPath))
	current := root
	for _, component := range components {
		parent := current
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			if err := os.Mkdir(current, 0o700); err != nil {
				if !errors.Is(err, fs.ErrExist) {
					return fmt.Errorf("create brain repository directory: %w", ErrSnapshotCorrupt)
				}
				info, err = os.Lstat(current)
				if err != nil {
					return fmt.Errorf("inspect created brain repository directory: %w", ErrUnsafePath)
				}
			} else {
				if err := syncDirectory(parent); err != nil {
					return err
				}
				info, err = os.Lstat(current)
				if err != nil {
					return fmt.Errorf("inspect created brain repository directory: %w", ErrUnsafePath)
				}
			}
		} else if err != nil {
			return fmt.Errorf("inspect brain repository directory: %w", ErrUnsafePath)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("brain repository directory is unsafe: %w", ErrUnsafePath)
		}
	}
	return nil
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

func openRegularNoFollow(path string) (*os.File, os.FileInfo, error) {
	exists, err := inspectPathComponents(path)
	if err != nil || !exists {
		if err == nil {
			err = fs.ErrNotExist
		}
		return nil, nil, err
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, nil, ErrUnsafePath
	}
	file, err := os.OpenFile(path, os.O_RDONLY|policy.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, err
	}
	openedInfo, err := file.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		file.Close()
		return nil, nil, ErrUnsafePath
	}
	return file, openedInfo, nil
}

func readBoundedRegularFile(path string, limit int64) ([]byte, error) {
	file, info, err := openRegularNoFollow(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if info.Size() > limit {
		return nil, ErrSnapshotCorrupt
	}
	content, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(content)) > limit {
		return nil, ErrSnapshotCorrupt
	}
	return content, nil
}

func writeNewSyncedFile(path string, content []byte) error {
	if exists, err := inspectPathComponents(path); err != nil {
		return err
	} else if exists {
		return ErrSnapshotExists
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|policy.O_NOFOLLOW, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return ErrSnapshotExists
		}
		return fmt.Errorf("create brain snapshot file: %w", ErrSnapshotCorrupt)
	}
	if _, err := file.Write(content); err != nil {
		file.Close()
		return fmt.Errorf("write brain snapshot file: %w", ErrSnapshotCorrupt)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("sync brain snapshot file: %w", ErrSnapshotCorrupt)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close brain snapshot file: %w", ErrSnapshotCorrupt)
	}
	return syncDirectory(filepath.Dir(path))
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open brain repository directory for sync: %w", ErrSnapshotCorrupt)
	}
	err = directory.Sync()
	closeErr := directory.Close()
	if err != nil || closeErr != nil {
		return fmt.Errorf("sync brain repository directory: %w", ErrSnapshotCorrupt)
	}
	return nil
}

func digestBytes(content []byte) string {
	digest := sha256.Sum256(content)
	return fmt.Sprintf("sha256:%x", digest)
}
