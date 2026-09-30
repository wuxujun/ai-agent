//go:build windows

package brain

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
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
	maxStatusSnapshotIDs        = 128
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

type Repository struct {
	root   string
	ledger RetractionView

	rename                  func(string, string) error
	beforeFileCreate        func()
	beforeReleaseRename     func()
	beforeCurrentCommit     func()
	syncCurrentBeforeRename func() error
	syncCurrentParent       func() error
}

type RepositoryStatus struct {
	Current         string   `json:"current_snapshot_id"`
	Staging         []string `json:"staging_snapshot_ids"`
	Releases        []string `json:"release_snapshot_ids"`
	RevocationState string   `json:"revocation_state"`
}

type secureDir struct {
	path string
}

type projectLock struct {
	directory *secureDir
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
	if _, err := openAbsoluteDirectory(absRoot, true); err != nil {
		return nil, err
	}
	return &Repository{root: absRoot, ledger: ledger}, nil
}

func (r *Repository) Current(ctx context.Context, ref ProjectRef) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("brain snapshot repository is not supported on windows: %w", ErrUnsafePath)
}

func (r *Repository) CreateStage(ctx context.Context, ref ProjectRef, draft SnapshotDraft) (Manifest, error) {
	if err := ctx.Err(); err != nil {
		return Manifest{}, err
	}
	if _, err := prepareSnapshot(ref, draft); err != nil {
		return Manifest{}, err
	}
	return Manifest{}, fmt.Errorf("brain snapshot repository is not supported on windows: %w", ErrUnsafePath)
}

func (r *Repository) OpenRelease(ctx context.Context, ref ProjectRef, snapshotID string) (Release, error) {
	if err := validateLifecycleIdentifiers(ctx, snapshotID, ""); err != nil {
		return Release{}, err
	}
	return Release{}, fmt.Errorf("brain snapshot repository is not supported on windows: %w", ErrUnsafePath)
}

func (r *Repository) OpenStaging(ctx context.Context, ref ProjectRef, snapshotID string) (Release, error) {
	if err := validateLifecycleIdentifiers(ctx, snapshotID, ""); err != nil {
		return Release{}, err
	}
	return Release{}, fmt.Errorf("brain snapshot repository is not supported on windows: %w", ErrUnsafePath)
}

func (r *Repository) Status(ctx context.Context, ref ProjectRef) (RepositoryStatus, error) {
	if err := ctx.Err(); err != nil {
		return RepositoryStatus{}, err
	}
	return RepositoryStatus{}, fmt.Errorf("brain snapshot repository is not supported on windows: %w", ErrUnsafePath)
}

func (r *Repository) Publish(ctx context.Context, ref ProjectRef, snapshotID, expectedCurrent string) (Manifest, error) {
	if err := validateLifecycleIdentifiers(ctx, snapshotID, expectedCurrent); err != nil {
		return Manifest{}, err
	}
	return Manifest{}, fmt.Errorf("brain snapshot repository is not supported on windows: %w", ErrUnsafePath)
}

func (r *Repository) Rollback(ctx context.Context, ref ProjectRef, snapshotID, expectedCurrent string) (Manifest, error) {
	if err := validateLifecycleIdentifiers(ctx, snapshotID, expectedCurrent); err != nil {
		return Manifest{}, err
	}
	return Manifest{}, fmt.Errorf("brain snapshot repository is not supported on windows: %w", ErrUnsafePath)
}

func (r *Repository) projectRoot(ref ProjectRef) (string, error) {
	return resolveProjectRoot(r.root, ref)
}

func prepareSnapshot(ref ProjectRef, draft SnapshotDraft) (preparedSnapshot, error) {
	if err := validateDraftScope(ref, draft); err != nil {
		return preparedSnapshot{}, err
	}
	return prepareSnapshotContent(draft)
}

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

func openAbsoluteDirectory(path string, create bool) (*secureDir, error) {
	if strings.TrimSpace(path) == "" || path != filepath.Clean(path) || !filepath.IsAbs(path) {
		return nil, ErrUnsafePath
	}
	if create {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return nil, ErrSnapshotCorrupt
		}
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, errSecurePathMissing
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, ErrUnsafePath
	}
	return &secureDir{path: path}, nil
}

func openProjectHandle(root string, ref ProjectRef, create bool) (*projectHandle, error) {
	if _, err := resolveProjectRoot(root, ref); err != nil {
		return nil, err
	}
	rootDirectory, err := openAbsoluteDirectory(root, false)
	if err != nil {
		return nil, err
	}
	tenant, err := rootDirectory.openChildDirectory(ref.StorageKey, create, false)
	if err != nil {
		return nil, err
	}
	project, err := tenant.openChildDirectory(ref.ProjectID, create, false)
	if err != nil {
		return nil, err
	}
	return &projectHandle{dir: project, tenant: tenant, projectName: ref.ProjectID}, nil
}

func (p *projectHandle) close() {}

func (p *projectHandle) verify() error {
	return p.tenant.verifyChildIdentity(p.projectName, p.dir)
}

func (d *secureDir) close() {}

func (d *secureDir) openChildDirectory(name string, create, exclusive bool) (*secureDir, error) {
	if d == nil || d.path == "" || !safeSingleComponent(name) {
		return nil, ErrUnsafePath
	}
	path := filepath.Join(d.path, name)
	if create {
		if err := os.Mkdir(path, 0o700); errors.Is(err, os.ErrExist) && exclusive {
			return nil, ErrSnapshotExists
		} else if err != nil && !errors.Is(err, os.ErrExist) {
			return nil, ErrSnapshotCorrupt
		}
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, errSecurePathMissing
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, ErrUnsafePath
	}
	return &secureDir{path: path}, nil
}

func (d *secureDir) verifyChildIdentity(name string, child *secureDir) error {
	if d == nil || child == nil || !safeSingleComponent(name) {
		return ErrUnsafePath
	}
	named := filepath.Join(d.path, name)
	namedReal, err := filepath.EvalSymlinks(named)
	if err != nil {
		return ErrUnsafePath
	}
	childReal, err := filepath.EvalSymlinks(child.path)
	if err != nil {
		return ErrUnsafePath
	}
	if !strings.EqualFold(filepath.Clean(namedReal), filepath.Clean(childReal)) {
		return ErrUnsafePath
	}
	return nil
}

func acquireProjectLock(ctx context.Context, directory *secureDir) (*projectLock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if directory == nil || directory.path == "" {
		return nil, ErrUnsafePath
	}
	return &projectLock{directory: directory}, nil
}

func (lock *projectLock) verify() error {
	if lock == nil || lock.directory == nil || lock.directory.path == "" {
		return ErrUnsafePath
	}
	return nil
}

func (lock *projectLock) release() {}

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
