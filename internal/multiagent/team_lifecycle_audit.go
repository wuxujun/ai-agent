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
	Healthy          bool   `json:"healthy"`
	RecordCount      int    `json:"record_count"`
	ProtectedRecords int    `json:"protected_records"`
	LegacyRecords    int    `json:"legacy_records"`
	FileSizeBytes    int64  `json:"file_size_bytes"`
	MaxBytes         int64  `json:"max_bytes"`
	UsagePercent     int    `json:"usage_percent"`
	CapacityStatus   string `json:"capacity_status"`
	Error            string `json:"error,omitempty"`
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

func teamLifecycleAuditPath() string {
	if configured := strings.TrimSpace(os.Getenv("AI_AGENT_TEAM_LIFECYCLE_AUDIT_FILE")); configured != "" {
		return filepath.Clean(configured)
	}
	return defaultTeamLifecycleAuditPath
}

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
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return result
	}
	if err != nil || !info.Mode().IsRegular() {
		result.Healthy = false
		result.Error = "audit file is unavailable or not a regular file"
		return result
	}
	result.FileSizeBytes = info.Size()
	if result.MaxBytes > 0 {
		result.UsagePercent = int((result.FileSizeBytes * 100) / result.MaxBytes)
		if result.FileSizeBytes >= result.MaxBytes {
			result.CapacityStatus = "full"
		} else if result.UsagePercent >= 80 {
			result.CapacityStatus = "warning"
		}
	}
	file, err := os.Open(path)
	if err != nil {
		result.Healthy = false
		result.Error = "audit file could not be opened"
		return result
	}
	defer file.Close()
	previousHash := ""
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 256*1024)
	for scanner.Scan() {
		result.RecordCount++
		var record TeamLifecycleAuditRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			result.Healthy = false
			result.Error = fmt.Sprintf("record %d is invalid JSON", result.RecordCount)
			return result
		}
		if record.Hash == "" {
			result.LegacyRecords++
			continue
		}
		result.ProtectedRecords++
		if record.PreviousHash != previousHash || record.Hash != teamLifecycleAuditRecordHash(record) {
			result.Healthy = false
			result.Error = fmt.Sprintf("hash chain mismatch at record %d", result.RecordCount)
			return result
		}
		previousHash = record.Hash
	}
	if err := scanner.Err(); err != nil {
		result.Healthy = false
		result.Error = "audit file could not be read"
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
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return []TeamLifecycleAuditRecord{}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("Team lifecycle audit path is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	records := make([]TeamLifecycleAuditRecord, 0)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 256*1024)
	for scanner.Scan() {
		var record TeamLifecycleAuditRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return nil, false, fmt.Errorf("decode Team lifecycle audit: %w", err)
		}
		if filter.Team != "" && record.Team != filter.Team {
			continue
		}
		if filter.Changed != nil && record.Changed != *filter.Changed {
			continue
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		return nil, false, err
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
