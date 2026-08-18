package multiagent

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const defaultTeamLifecycleAuditPath = "data/team-lifecycle-audit.jsonl"
const defaultTeamLifecycleAuditMaxBytes int64 = 64 << 20

var teamLifecycleAuditMu sync.Mutex

var ErrTeamLifecycleAuditFull = fmt.Errorf("Team lifecycle audit capacity is full")
var ErrTeamLifecycleAuditArchiveConflict = fmt.Errorf("Team lifecycle audit archive revision conflict")

type TeamLifecycleAuditRecord struct {
	ID               string        `json:"id"`
	Timestamp        time.Time     `json:"timestamp"`
	ActorTenant      string        `json:"actor_tenant"`
	Team             string        `json:"team"`
	Previous         TeamLifecycle `json:"previous"`
	Current          TeamLifecycle `json:"current"`
	PreviousRevision string        `json:"previous_revision"`
	Revision         string        `json:"revision"`
	Changed          bool          `json:"changed"`
	PreviousHash     string        `json:"previous_hash,omitempty"`
	Hash             string        `json:"hash,omitempty"`
}

type TeamLifecycleAuditIntegrity struct {
	Healthy           bool   `json:"healthy"`
	RecordCount       int    `json:"record_count"`
	ProtectedRecords  int    `json:"protected_records"`
	LegacyRecords     int    `json:"legacy_records"`
	FileSizeBytes     int64  `json:"file_size_bytes"`
	MaxBytes          int64  `json:"max_bytes"`
	UsagePercent      int    `json:"usage_percent"`
	CapacityStatus    string `json:"capacity_status"`
	FileDigest        string `json:"file_digest,omitempty"`
	ArchiveCount      int    `json:"archive_count"`
	ArchivedRecords   int    `json:"archived_records"`
	ArchivedSizeBytes int64  `json:"archived_size_bytes"`
	Error             string `json:"error,omitempty"`
}

type TeamLifecycleAuditArchive struct {
	Name             string    `json:"name"`
	CreatedAt        time.Time `json:"created_at"`
	FileDigest       string    `json:"file_digest"`
	RecordCount      int       `json:"record_count"`
	ProtectedRecords int       `json:"protected_records"`
	LegacyRecords    int       `json:"legacy_records"`
	FileSizeBytes    int64     `json:"file_size_bytes"`
}

type TeamLifecycleAuditFilter struct {
	Team    string
	Changed *bool
}

func AppendTeamLifecycleAudit(actorTenant string, change TeamLifecycleChange) (TeamLifecycleAuditRecord, error) {
	record := TeamLifecycleAuditRecord{
		ID: uuid.NewString(), Timestamp: time.Now().UTC(), ActorTenant: strings.TrimSpace(actorTenant),
		Team: change.Team, Previous: change.Previous, Current: change.Current,
		PreviousRevision: change.PreviousRevision, Revision: change.Revision, Changed: change.Changed,
	}
	sealed, err := appendTeamLifecycleAuditFile(teamLifecycleAuditPath(), record)
	if err != nil {
		return TeamLifecycleAuditRecord{}, err
	}
	return sealed, nil
}

func ListTeamLifecycleAudits(limit, offset int) ([]TeamLifecycleAuditRecord, bool, error) {
	return ListTeamLifecycleAuditsFiltered(TeamLifecycleAuditFilter{}, limit, offset)
}

func ListTeamLifecycleAuditsFiltered(filter TeamLifecycleAuditFilter, limit, offset int) ([]TeamLifecycleAuditRecord, bool, error) {
	return listTeamLifecycleAuditFile(teamLifecycleAuditPath(), filter, limit, offset)
}

func CheckTeamLifecycleAuditIntegrity() TeamLifecycleAuditIntegrity {
	return checkTeamLifecycleAuditFile(teamLifecycleAuditPath())
}

func ArchiveTeamLifecycleAudit(expectedFileDigest string) (TeamLifecycleAuditArchive, error) {
	return archiveTeamLifecycleAuditFile(teamLifecycleAuditPath(), expectedFileDigest, time.Now().UTC())
}

func ListTeamLifecycleAuditArchives() ([]TeamLifecycleAuditArchive, error) {
	return listTeamLifecycleAuditArchives(teamLifecycleAuditPath())
}

func teamLifecycleAuditPath() string {
	if configured := strings.TrimSpace(os.Getenv("AI_AGENT_TEAM_LIFECYCLE_AUDIT_FILE")); configured != "" {
		return filepath.Clean(configured)
	}
	return defaultTeamLifecycleAuditPath
}

func teamLifecycleAuditArchiveDir(path string) string { return path + ".archives" }

func teamLifecycleAuditMaxBytes() int64 {
	raw := strings.TrimSpace(os.Getenv("AI_AGENT_TEAM_LIFECYCLE_AUDIT_MAX_BYTES"))
	if raw == "" {
		return defaultTeamLifecycleAuditMaxBytes
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value <= 0 {
		return defaultTeamLifecycleAuditMaxBytes
	}
	return value
}

func appendTeamLifecycleAuditFile(path string, record TeamLifecycleAuditRecord) (TeamLifecycleAuditRecord, error) {
	teamLifecycleAuditMu.Lock()
	defer teamLifecycleAuditMu.Unlock()
	if strings.TrimSpace(record.ActorTenant) == "" || strings.TrimSpace(record.Team) == "" {
		return TeamLifecycleAuditRecord{}, fmt.Errorf("Team lifecycle audit actor and Team are required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return TeamLifecycleAuditRecord{}, fmt.Errorf("create Team lifecycle audit directory: %w", err)
	}
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return TeamLifecycleAuditRecord{}, fmt.Errorf("Team lifecycle audit path is not a regular file")
	} else if err != nil && !os.IsNotExist(err) {
		return TeamLifecycleAuditRecord{}, err
	}
	previousHash, err := lastProtectedAuditHash(path)
	if err != nil {
		return TeamLifecycleAuditRecord{}, err
	}
	record.PreviousHash = previousHash
	record.Hash = teamLifecycleAuditRecordHash(record)
	line, err := json.Marshal(record)
	if err != nil {
		return TeamLifecycleAuditRecord{}, err
	}
	currentSize := int64(0)
	if info, err := os.Stat(path); err == nil {
		currentSize = info.Size()
	} else if !os.IsNotExist(err) {
		return TeamLifecycleAuditRecord{}, err
	}
	if currentSize+int64(len(line))+1 > teamLifecycleAuditMaxBytes() {
		return TeamLifecycleAuditRecord{}, fmt.Errorf("%w: current=%d projected=%d max=%d", ErrTeamLifecycleAuditFull, currentSize, currentSize+int64(len(line))+1, teamLifecycleAuditMaxBytes())
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return TeamLifecycleAuditRecord{}, fmt.Errorf("open Team lifecycle audit: %w", err)
	}
	defer file.Close()
	if _, err := file.Write(append(line, '\n')); err != nil {
		return TeamLifecycleAuditRecord{}, fmt.Errorf("append Team lifecycle audit: %w", err)
	}
	if err := file.Sync(); err != nil {
		return TeamLifecycleAuditRecord{}, fmt.Errorf("sync Team lifecycle audit: %w", err)
	}
	return record, nil
}

func lastProtectedAuditHash(path string) (string, error) {
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer file.Close()
	lastHash := ""
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 256*1024)
	for scanner.Scan() {
		var record TeamLifecycleAuditRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return "", fmt.Errorf("decode Team lifecycle audit: %w", err)
		}
		if record.Hash != "" {
			if record.PreviousHash != lastHash || record.Hash != teamLifecycleAuditRecordHash(record) {
				return "", fmt.Errorf("Team lifecycle audit hash chain is invalid")
			}
			lastHash = record.Hash
		}
	}
	return lastHash, scanner.Err()
}

func teamLifecycleAuditRecordHash(record TeamLifecycleAuditRecord) string {
	record.Hash = ""
	encoded, _ := json.Marshal(record)
	digest := sha256.Sum256(encoded)
	return fmt.Sprintf("%x", digest[:])
}

func checkTeamLifecycleAuditFile(path string) TeamLifecycleAuditIntegrity {
	teamLifecycleAuditMu.Lock()
	defer teamLifecycleAuditMu.Unlock()
	result := TeamLifecycleAuditIntegrity{Healthy: true, MaxBytes: teamLifecycleAuditMaxBytes(), CapacityStatus: "ok"}
	current, err := inspectTeamLifecycleAuditPath(path)
	if err != nil {
		result.Healthy = false
		result.Error = err.Error()
		return result
	}
	result.RecordCount = current.RecordCount
	result.ProtectedRecords = current.ProtectedRecords
	result.LegacyRecords = current.LegacyRecords
	result.FileSizeBytes = current.FileSizeBytes
	result.FileDigest = current.FileDigest
	archives, err := teamLifecycleAuditArchivePaths(path)
	if err != nil {
		result.Healthy = false
		result.Error = "audit archive directory could not be read"
		return result
	}
	result.ArchiveCount = len(archives)
	for _, archivePath := range archives {
		archive, inspectErr := inspectTeamLifecycleAuditPath(archivePath)
		if inspectErr != nil {
			result.Healthy = false
			result.Error = "an audit archive failed integrity validation"
			return result
		}
		result.ArchivedRecords += archive.RecordCount
		result.ArchivedSizeBytes += archive.FileSizeBytes
		result.RecordCount += archive.RecordCount
		result.ProtectedRecords += archive.ProtectedRecords
		result.LegacyRecords += archive.LegacyRecords
	}
	if result.MaxBytes > 0 {
		result.UsagePercent = int((result.FileSizeBytes * 100) / result.MaxBytes)
		if result.FileSizeBytes >= result.MaxBytes {
			result.CapacityStatus = "full"
		} else if result.UsagePercent >= 80 {
			result.CapacityStatus = "warning"
		}
	}
	return result
}

func listTeamLifecycleAuditFile(path string, filter TeamLifecycleAuditFilter, limit, offset int) ([]TeamLifecycleAuditRecord, bool, error) {
	teamLifecycleAuditMu.Lock()
	defer teamLifecycleAuditMu.Unlock()
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	records := make([]TeamLifecycleAuditRecord, 0)
	paths, err := teamLifecycleAuditArchivePaths(path)
	if err != nil {
		return nil, false, err
	}
	paths = append(paths, path)
	for _, auditPath := range paths {
		loaded, loadErr := loadTeamLifecycleAuditRecords(auditPath)
		if os.IsNotExist(loadErr) {
			continue
		}
		if loadErr != nil {
			return nil, false, loadErr
		}
		for _, record := range loaded {
			if filter.Team != "" && record.Team != filter.Team {
				continue
			}
			if filter.Changed != nil && record.Changed != *filter.Changed {
				continue
			}
			records = append(records, record)
		}
	}
	sort.SliceStable(records, func(i, j int) bool { return records[i].Timestamp.After(records[j].Timestamp) })
	if offset >= len(records) {
		return []TeamLifecycleAuditRecord{}, false, nil
	}
	end := offset + limit
	hasMore := end < len(records)
	if end > len(records) {
		end = len(records)
	}
	return records[offset:end], hasMore, nil
}

func loadTeamLifecycleAuditRecords(path string) ([]TeamLifecycleAuditRecord, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("Team lifecycle audit path is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	records := make([]TeamLifecycleAuditRecord, 0)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 256*1024)
	for scanner.Scan() {
		var record TeamLifecycleAuditRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return nil, fmt.Errorf("decode Team lifecycle audit: %w", err)
		}
		records = append(records, record)
	}
	return records, scanner.Err()
}

func inspectTeamLifecycleAuditPath(path string) (TeamLifecycleAuditArchive, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return TeamLifecycleAuditArchive{}, nil
	}
	if err != nil || !info.Mode().IsRegular() {
		return TeamLifecycleAuditArchive{}, fmt.Errorf("audit file is unavailable or not a regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return TeamLifecycleAuditArchive{}, fmt.Errorf("audit file could not be read")
	}
	records, err := loadTeamLifecycleAuditRecords(path)
	if err != nil {
		return TeamLifecycleAuditArchive{}, err
	}
	result := TeamLifecycleAuditArchive{Name: filepath.Base(path), FileDigest: teamsFileRevision(data), FileSizeBytes: info.Size(), RecordCount: len(records)}
	previousHash := ""
	for index, record := range records {
		if record.Hash == "" {
			result.LegacyRecords++
			continue
		}
		result.ProtectedRecords++
		if record.PreviousHash != previousHash || record.Hash != teamLifecycleAuditRecordHash(record) {
			return TeamLifecycleAuditArchive{}, fmt.Errorf("hash chain mismatch at record %d", index+1)
		}
		previousHash = record.Hash
	}
	return result, nil
}

func teamLifecycleAuditArchivePaths(path string) ([]string, error) {
	dir := teamLifecycleAuditArchiveDir(path)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ".jsonl") {
			paths = append(paths, filepath.Join(dir, entry.Name()))
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func listTeamLifecycleAuditArchives(path string) ([]TeamLifecycleAuditArchive, error) {
	teamLifecycleAuditMu.Lock()
	defer teamLifecycleAuditMu.Unlock()
	paths, err := teamLifecycleAuditArchivePaths(path)
	if err != nil {
		return nil, err
	}
	archives := make([]TeamLifecycleAuditArchive, 0, len(paths))
	for _, archivePath := range paths {
		archive, err := inspectTeamLifecycleAuditPath(archivePath)
		if err != nil {
			return nil, err
		}
		archive.Name = filepath.Base(archivePath)
		archive.CreatedAt = archiveTimestamp(archive.Name)
		if archive.CreatedAt.IsZero() {
			if info, statErr := os.Stat(archivePath); statErr == nil {
				archive.CreatedAt = info.ModTime().UTC()
			}
		}
		archives = append(archives, archive)
	}
	sort.SliceStable(archives, func(i, j int) bool { return archives[i].CreatedAt.After(archives[j].CreatedAt) })
	return archives, nil
}

func archiveTimestamp(name string) time.Time {
	const prefix = "team-lifecycle-audit-"
	if !strings.HasPrefix(name, prefix) {
		return time.Time{}
	}
	remainder := strings.TrimPrefix(name, prefix)
	dash := strings.Index(remainder, "-")
	if dash <= 0 {
		return time.Time{}
	}
	parsed, err := time.Parse("20060102T150405.000000000Z", remainder[:dash])
	if err != nil {
		return time.Time{}
	}
	return parsed.UTC()
}

func archiveTeamLifecycleAuditFile(path, expectedFileDigest string, now time.Time) (TeamLifecycleAuditArchive, error) {
	teamLifecycleAuditMu.Lock()
	defer teamLifecycleAuditMu.Unlock()
	current, err := inspectTeamLifecycleAuditPath(path)
	if err != nil {
		return TeamLifecycleAuditArchive{}, err
	}
	if current.RecordCount == 0 {
		return TeamLifecycleAuditArchive{}, fmt.Errorf("Team lifecycle audit has no records to archive")
	}
	if strings.TrimSpace(expectedFileDigest) == "" || expectedFileDigest != current.FileDigest {
		return TeamLifecycleAuditArchive{}, fmt.Errorf("%w: expected %q, current %q", ErrTeamLifecycleAuditArchiveConflict, expectedFileDigest, current.FileDigest)
	}
	dir := teamLifecycleAuditArchiveDir(path)
	if info, statErr := os.Lstat(dir); statErr == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return TeamLifecycleAuditArchive{}, fmt.Errorf("audit archive path is not a regular directory")
		}
	} else if os.IsNotExist(statErr) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return TeamLifecycleAuditArchive{}, err
		}
	} else {
		return TeamLifecycleAuditArchive{}, statErr
	}
	name := fmt.Sprintf("team-lifecycle-audit-%s-%s.jsonl", now.UTC().Format("20060102T150405.000000000Z"), current.FileDigest)
	destination := filepath.Join(dir, name)
	if _, err := os.Lstat(destination); err == nil {
		return TeamLifecycleAuditArchive{}, fmt.Errorf("audit archive %q already exists", name)
	} else if !os.IsNotExist(err) {
		return TeamLifecycleAuditArchive{}, err
	}
	if err := os.Rename(path, destination); err != nil {
		return TeamLifecycleAuditArchive{}, fmt.Errorf("move Team lifecycle audit to archive: %w", err)
	}
	newFile, createErr := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if createErr == nil {
		createErr = newFile.Sync()
		if closeErr := newFile.Close(); createErr == nil {
			createErr = closeErr
		}
	}
	if createErr != nil {
		if rollbackErr := os.Rename(destination, path); rollbackErr != nil {
			return TeamLifecycleAuditArchive{}, fmt.Errorf("create new audit: %v; rollback failed: %v", createErr, rollbackErr)
		}
		return TeamLifecycleAuditArchive{}, fmt.Errorf("create new Team lifecycle audit: %w", createErr)
	}
	current.Name = name
	current.CreatedAt = now.UTC()
	return current, nil
}
