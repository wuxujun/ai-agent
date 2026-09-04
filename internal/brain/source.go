package brain

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/wuxujun/ai-agent/internal/evidencefilter"
	"github.com/wuxujun/ai-agent/internal/sanitize"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

const (
	defaultSourcePageSize      = 100
	maxSourcePageSize          = 500
	maxEvidenceRecords         = 1000
	maxEvidenceBytes           = 16 * 1024
	maxSourceEvidenceBytes     = 1024 * 1024
	maxSourcePages             = 10000
	maxDiscoveryHints          = 128
	maxDiscoveryHintBytes      = 1000
	maxDiscoveryHintTotalBytes = 64 * 1024
)

// TaskSourceStore narrows the store dependency to the bounded task listing
// required for source collection.
type TaskSourceStore interface {
	ListTasks(context.Context, store.ListFilter) ([]*types.Task, error)
}

type taskSourceGetter interface {
	GetTask(context.Context, string) (*types.Task, error)
}

// SourceReader normalizes eligible, project-scoped task traces into provenance
// records. It deliberately does not derive evidence from answers or memories.
type SourceReader struct {
	Store    TaskSourceStore
	PageSize int
}

func NewSourceReader(source TaskSourceStore) *SourceReader {
	return &SourceReader{Store: source, PageSize: defaultSourcePageSize}
}

func (r *SourceReader) Read(ctx context.Context, ref ProjectRef, cutoff time.Time) (SourceSet, error) {
	if err := ctx.Err(); err != nil {
		return SourceSet{}, err
	}
	if r == nil || r.Store == nil {
		return SourceSet{}, fmt.Errorf("brain source store is required")
	}
	if err := validateSourceScope(ref); err != nil {
		return SourceSet{}, err
	}
	if cutoff.IsZero() {
		return SourceSet{}, fmt.Errorf("brain source cutoff is required")
	}

	pageSize := sourcePageSize(r.PageSize)
	set := SourceSet{Cutoff: cutoff.UTC()}
	seen := make(map[string]struct{})
	usedEvidenceBytes := 0
	discoveryBytes := 0
	exhausted := false
	for pageNumber := 0; pageNumber < maxSourcePages; pageNumber++ {
		if err := ctx.Err(); err != nil {
			return SourceSet{}, err
		}
		offset := pageNumber * pageSize
		page, err := r.Store.ListTasks(ctx, store.ListFilter{
			TenantID: ref.TenantID,
			Status:   types.StatusCompleted,
			Limit:    pageSize,
			Offset:   offset,
		})
		if err != nil {
			return SourceSet{}, fmt.Errorf("list completed brain source tasks: %w", err)
		}
		if len(page) == 0 {
			exhausted = true
			break
		}
		for _, task := range page {
			if err := ctx.Err(); err != nil {
				return SourceSet{}, err
			}
			if !eligibleTask(task, ref, cutoff) {
				continue
			}
			task, err = r.hydrateTask(ctx, task)
			if err != nil {
				return SourceSet{}, err
			}
			if !eligibleTask(task, ref, cutoff) {
				continue
			}
			set.DiscoveryHints, discoveryBytes = appendDiscoveryHints(set.DiscoveryHints, discoveryBytes, task)
			for _, trace := range task.Trace {
				if err := ctx.Err(); err != nil {
					return SourceSet{}, err
				}
				if len(set.Evidence) >= maxEvidenceRecords || usedEvidenceBytes >= maxSourceEvidenceBytes || !eligibleTrace(trace) {
					continue
				}
				content := boundedEvidenceContent(trace.Evidence, min(maxEvidenceBytes, maxSourceEvidenceBytes-usedEvidenceBytes))
				if content == "" {
					continue
				}
				record, err := evidenceRecord(ref, task, trace, content)
				if err != nil {
					return SourceSet{}, err
				}
				if _, duplicate := seen[record.ID]; duplicate {
					continue
				}
				seen[record.ID] = struct{}{}
				set.Evidence = append(set.Evidence, record)
				usedEvidenceBytes += len(record.Content)
			}
		}
	}
	if !exhausted {
		return SourceSet{}, fmt.Errorf("brain source scan exceeded page limit")
	}

	sort.Slice(set.Evidence, func(i, j int) bool {
		if set.Evidence[i].URI != set.Evidence[j].URI {
			return set.Evidence[i].URI < set.Evidence[j].URI
		}
		return set.Evidence[i].ID < set.Evidence[j].ID
	})
	sort.Strings(set.DiscoveryHints)
	set.SourceIDs = make([]string, len(set.Evidence))
	set.SourceHashes = make([]string, len(set.Evidence))
	for i, evidence := range set.Evidence {
		set.SourceIDs[i] = evidence.ID
		set.SourceHashes[i] = evidence.ContentHash
	}
	return set, nil
}

func (r *SourceReader) hydrateTask(ctx context.Context, task *types.Task) (*types.Task, error) {
	getter, ok := r.Store.(taskSourceGetter)
	if !ok {
		return task, nil
	}
	full, err := getter.GetTask(ctx, task.ID)
	if err != nil {
		return nil, fmt.Errorf("get brain source task: %w", err)
	}
	if full == nil {
		return nil, fmt.Errorf("get brain source task %q: empty result", task.ID)
	}
	return full, nil
}

func sourcePageSize(requested int) int {
	if requested <= 0 {
		return defaultSourcePageSize
	}
	if requested > maxSourcePageSize {
		return maxSourcePageSize
	}
	return requested
}

func eligibleTask(task *types.Task, ref ProjectRef, cutoff time.Time) bool {
	return task != nil &&
		task.TenantID == ref.TenantID &&
		task.BrainProjectID == ref.ProjectID &&
		task.Status == types.StatusCompleted &&
		!task.UpdatedAt.After(cutoff) &&
		task.AnswerAudit != nil && task.AnswerAudit.Publishable
}

func eligibleTrace(trace types.StepTrace) bool {
	return trace.Error == "" && evidencefilter.Eligible(trace.Action, trace.Observation) && len(trace.Evidence) > 0
}

func evidenceRecord(ref ProjectRef, task *types.Task, trace types.StepTrace, content string) (EvidenceRecord, error) {
	uri, err := canonicalEvidenceURI(ref, task.ID, trace.Step)
	if err != nil {
		return EvidenceRecord{}, err
	}
	return EvidenceRecord{
		ID:          sha256ID(uri),
		URI:         uri,
		TaskID:      task.ID,
		TraceStep:   strconv.Itoa(trace.Step),
		Content:     content,
		ContentHash: sha256ID(content),
		ObservedAt:  task.UpdatedAt.UTC(),
	}, nil
}

func canonicalEvidenceURI(ref ProjectRef, taskID string, step int) (string, error) {
	if step < 0 || !canonicalSegment(ref.TenantID) || !canonicalSegment(ref.ProjectID) || !canonicalSegment(taskID) {
		return "", fmt.Errorf("invalid brain evidence URI components")
	}
	raw := "brain-evidence://" + ref.TenantID + "/" + ref.ProjectID + "/tasks/" + taskID + "#trace/" + strconv.Itoa(step)
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "brain-evidence" || parsed.Host != ref.TenantID || parsed.Path != "/"+ref.ProjectID+"/tasks/"+taskID || parsed.Fragment != "trace/"+strconv.Itoa(step) || parsed.RawQuery != "" || parsed.String() != raw {
		return "", fmt.Errorf("invalid canonical brain evidence URI")
	}
	return raw, nil
}

func validateSourceScope(ref ProjectRef) error {
	if !canonicalSegment(ref.TenantID) || !canonicalSegment(ref.ProjectID) {
		return fmt.Errorf("invalid brain source scope")
	}
	return nil
}

func canonicalSegment(value string) bool {
	if value == "" || strings.TrimSpace(value) != value || value == "." || value == ".." || strings.ContainsAny(value, "/\\?#%@:") {
		return false
	}
	return strings.IndexFunc(value, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) == -1
}

func boundedEvidenceContent(evidence []types.Evidence, limit int) string {
	if limit <= 0 {
		return ""
	}
	var builder strings.Builder
	for _, item := range evidence {
		for _, line := range item.Lines {
			remaining := limit - builder.Len()
			if builder.Len() > 0 {
				remaining-- // Reserve the newline separator before truncating a line.
			}
			line = sanitizeBounded(line, remaining)
			if line == "" {
				continue
			}
			if builder.Len() > 0 {
				if builder.Len()+1 > limit {
					return builder.String()
				}
				builder.WriteByte('\n')
			}
			builder.WriteString(line)
			if builder.Len() >= limit {
				return builder.String()
			}
		}
	}
	return builder.String()
}

func appendDiscoveryHints(hints []string, usedBytes int, task *types.Task) ([]string, int) {
	appendHint := func(value string) {
		if len(hints) >= maxDiscoveryHints || usedBytes >= maxDiscoveryHintTotalBytes {
			return
		}
		value = sanitizeBounded(value, min(maxDiscoveryHintBytes, maxDiscoveryHintTotalBytes-usedBytes))
		if value == "" {
			return
		}
		hints = append(hints, value)
		usedBytes += len(value)
	}
	for _, memory := range task.Memories {
		appendHint(memory.KeyFindings)
	}
	appendHint(task.FinalAnswer)
	return hints, usedBytes
}

func sanitizeBounded(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	value = strings.TrimSpace(strings.ToValidUTF8(sanitize.Secrets(value), "�"))
	return truncateUTF8(value, limit)
}

func truncateUTF8(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	for limit > 0 && !utf8.RuneStart(value[limit]) {
		limit--
	}
	return value[:limit]
}

func sha256ID(value string) string {
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("sha256:%x", sum)
}

func min(left, right int) int {
	if left < right {
		return left
	}
	return right
}
