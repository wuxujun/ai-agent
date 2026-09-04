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
	"net/url"
	"os"
	"sort"
	"strings"
	"unicode"

	"golang.org/x/sys/unix"
)

const (
	maxRetractionLineBytes = 64 * 1024
	maxRetractionFileBytes = 16 * 1024 * 1024
)

var ErrRetractionLedger = errors.New("invalid brain retraction ledger")

// RetractionView exposes the live, project-scoped retraction state used by
// validation and release reads.
type RetractionView interface {
	Watermark(context.Context, ProjectRef) (string, error)
	Contains(context.Context, ProjectRef, string) (bool, error)
}

// FileRetractionLedger reads the operator-managed append-only ledger rooted at
// each resolved Brain project.
type FileRetractionLedger struct {
	root      string
	afterRead func()
}

func NewFileRetractionLedger(root string) *FileRetractionLedger {
	return &FileRetractionLedger{root: root}
}

func (l *FileRetractionLedger) Watermark(ctx context.Context, ref ProjectRef) (string, error) {
	records, err := l.read(ctx, ref)
	if err != nil {
		return "", err
	}
	canonical := make([]string, 0, len(records))
	for encoded := range records {
		canonical = append(canonical, encoded)
	}
	sort.Strings(canonical)
	digest := sha256.Sum256([]byte(strings.Join(canonical, "\n")))
	return fmt.Sprintf("sha256:%x", digest), nil
}

func (l *FileRetractionLedger) Contains(ctx context.Context, ref ProjectRef, evidenceURI string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if !validRetractionURI(evidenceURI) {
		return false, fmt.Errorf("brain retraction evidence URI is invalid: %w", ErrRetractionLedger)
	}
	records, err := l.read(ctx, ref)
	if err != nil {
		return false, err
	}
	for _, record := range records {
		if record.EvidenceURI == evidenceURI {
			return true, nil
		}
	}
	return false, nil
}

func (l *FileRetractionLedger) read(ctx context.Context, ref ProjectRef) (map[string]Retraction, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if l == nil || strings.TrimSpace(l.root) == "" {
		return nil, fmt.Errorf("brain retraction root is invalid: %w", ErrUnsafePath)
	}
	project, err := openProjectHandle(l.root, ref, false)
	if errors.Is(err, errSecurePathMissing) {
		return map[string]Retraction{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer project.close()
	content, err := l.readStableFile(project.dir)
	if errors.Is(err, errSecurePathMissing) {
		return map[string]Retraction{}, nil
	}
	if err != nil {
		return nil, err
	}
	if len(content) > 0 && content[len(content)-1] != '\n' {
		return nil, fmt.Errorf("brain retraction ledger has an unterminated record: %w", ErrRetractionLedger)
	}

	records := make(map[string]Retraction)
	scanner := bufio.NewScanner(bytes.NewReader(content))
	scanner.Buffer(make([]byte, 4096), maxRetractionLineBytes)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		record, encoded, err := decodeRetraction(line)
		if err != nil {
			return nil, err
		}
		records[string(encoded)] = record
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan brain retraction ledger: %w", ErrRetractionLedger)
	}
	if err := project.verify(); err != nil {
		return nil, err
	}
	return records, nil
}

// WithProjectMutationLock exposes the filesystem-visible coordination contract
// for the future operator ledger writer. The callback must complete its append,
// file fsync, and project-directory fsync before returning.
func (l *FileRetractionLedger) WithProjectMutationLock(ctx context.Context, ref ProjectRef, mutate func() error) error {
	if mutate == nil {
		return ErrRetractionLedger
	}
	project, err := openProjectHandle(l.root, ref, false)
	if err != nil {
		return err
	}
	defer project.close()
	lock, err := acquireProjectLock(ctx, project.dir)
	if err != nil {
		return err
	}
	defer lock.release()
	if err := mutate(); err != nil {
		return err
	}
	if err := lock.verify(); err != nil {
		return err
	}
	return project.verify()
}

func (l *FileRetractionLedger) readStableFile(project *secureDir) ([]byte, error) {
	fd, err := unix.Openat(project.fd, "retractions.jsonl", unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, errSecurePathMissing
	}
	if err != nil {
		return nil, fmt.Errorf("open brain retraction ledger: %w", ErrUnsafePath)
	}
	file := os.NewFile(uintptr(fd), "brain-retraction-ledger")
	defer file.Close()
	var before unix.Stat_t
	if err := unix.Fstat(fd, &before); err != nil || before.Mode&unix.S_IFMT != unix.S_IFREG || before.Size > maxRetractionFileBytes {
		return nil, fmt.Errorf("inspect brain retraction ledger: %w", ErrRetractionLedger)
	}
	content, err := io.ReadAll(io.LimitReader(file, maxRetractionFileBytes+1))
	if err != nil || len(content) > maxRetractionFileBytes || int64(len(content)) != before.Size {
		return nil, fmt.Errorf("read brain retraction ledger: %w", ErrRetractionLedger)
	}
	if l.afterRead != nil {
		l.afterRead()
	}
	var after unix.Stat_t
	if err := unix.Fstat(fd, &after); err != nil || before.Dev != after.Dev || before.Ino != after.Ino || before.Size != after.Size || before.Mtim != after.Mtim || before.Ctim != after.Ctim {
		return nil, fmt.Errorf("brain retraction ledger changed during read: %w", ErrRetractionLedger)
	}
	return content, nil
}

func decodeRetraction(line []byte) (Retraction, []byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	var record Retraction
	if err := decoder.Decode(&record); err != nil {
		return Retraction{}, nil, fmt.Errorf("decode brain retraction record: %w", ErrRetractionLedger)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return Retraction{}, nil, fmt.Errorf("decode brain retraction record: %w", ErrRetractionLedger)
	}
	encoded, err := canonicalRetraction(record)
	if err != nil {
		return Retraction{}, nil, err
	}
	var canonicalRecord Retraction
	if err := json.Unmarshal(encoded, &canonicalRecord); err != nil {
		return Retraction{}, nil, fmt.Errorf("canonicalize brain retraction record: %w", ErrRetractionLedger)
	}
	return canonicalRecord, encoded, nil
}

func canonicalRetraction(record Retraction) ([]byte, error) {
	if !validRetractionURI(record.EvidenceURI) || strings.TrimSpace(record.Reason) == "" || strings.TrimSpace(record.Reason) != record.Reason || record.RetractedAt.IsZero() {
		return nil, fmt.Errorf("brain retraction record fields are invalid: %w", ErrRetractionLedger)
	}
	if strings.IndexFunc(record.Reason, unicode.IsControl) >= 0 {
		return nil, fmt.Errorf("brain retraction reason is invalid: %w", ErrRetractionLedger)
	}
	record.RetractedAt = record.RetractedAt.UTC()
	encoded, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("encode brain retraction record: %w", ErrRetractionLedger)
	}
	return encoded, nil
}

func validRetractionURI(value string) bool {
	if value == "" || strings.TrimSpace(value) != value || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return false
	}
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme != "" && parsed.String() == value
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("multiple JSON values")
	}
	return err
}
