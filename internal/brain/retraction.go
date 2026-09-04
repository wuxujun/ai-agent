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
	"sort"
	"strings"
	"unicode"
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
	root string
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
	projectRoot, err := resolveProjectRoot(l.root, ref)
	if err != nil {
		return nil, err
	}
	ledgerPath, err := safeJoin(projectRoot, "retractions.jsonl")
	if err != nil {
		return nil, err
	}
	exists, err := inspectPathComponents(ledgerPath)
	if err != nil {
		return nil, err
	}
	if !exists {
		return map[string]Retraction{}, nil
	}
	file, info, err := openRegularNoFollow(ledgerPath)
	if err != nil {
		return nil, fmt.Errorf("open brain retraction ledger: %w", ErrRetractionLedger)
	}
	defer file.Close()
	if info.Size() > maxRetractionFileBytes {
		return nil, fmt.Errorf("brain retraction ledger exceeds limit: %w", ErrRetractionLedger)
	}

	records := make(map[string]Retraction)
	scanner := bufio.NewScanner(io.LimitReader(file, maxRetractionFileBytes+1))
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
	return records, nil
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
