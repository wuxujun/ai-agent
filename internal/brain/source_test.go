package brain

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

var cutoff = time.Date(2026, time.September, 2, 12, 0, 0, 0, time.UTC)

func TestSourceReaderKeepsOnlyPublishableCompletedProjectTasks(t *testing.T) {
	got, err := newFixtureReader(t).Read(t.Context(), atlasRef(), cutoff)
	if err != nil {
		t.Fatal(err)
	}
	ids := sourceTaskIDs(got)
	if len(ids) != 1 || ids[0] != "eligible" {
		t.Fatalf("task ids = %v", ids)
	}
}

func TestSourceReaderRequiresTraceEvidenceForClaims(t *testing.T) {
	got, err := newFixtureReaderWithAnswerOnly(t).Read(t.Context(), atlasRef(), cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Evidence) != 0 {
		t.Fatalf("answer became evidence: %+v", got.Evidence)
	}
	if len(got.DiscoveryHints) != 1 || got.DiscoveryHints[0] != "answer only" {
		t.Fatalf("discovery hints = %#v", got.DiscoveryHints)
	}
}

func TestSourceReaderPagesWithExactScopeAndCutoff(t *testing.T) {
	reader := NewSourceReader(&fixtureStore{tasks: []*types.Task{
		fixtureTask("first"),
		fixtureTask("second"),
		withProject(fixtureTask("other-project"), "other"),
		withTenant(fixtureTask("other-tenant"), "tenant-b"),
		withUpdatedAt(fixtureTask("after-cutoff"), cutoff.Add(time.Second)),
	}})
	reader.PageSize = 1

	got, err := reader.Read(t.Context(), atlasRef(), cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if ids := sourceTaskIDs(got); strings.Join(ids, ",") != "first,second" {
		t.Fatalf("task ids = %v", ids)
	}
}

func TestSourceReaderRequiresSuccessfulEligibleNonemptyTrace(t *testing.T) {
	unsuccessful := fixtureTask("unsuccessful")
	unsuccessful.Trace[0].Error = "upstream failed"
	nonEligible := fixtureTask("non-eligible")
	nonEligible.Trace[0].Action = "write_file"
	emptyEvidence := fixtureTask("empty-evidence")
	emptyEvidence.Trace[0].Evidence = []types.Evidence{{Lines: []string{" \t "}}}
	quarantined := fixtureTask("quarantined")
	quarantined.Trace[0].Observation = "external content quarantined"
	reader := NewSourceReader(&fixtureStore{tasks: []*types.Task{unsuccessful, nonEligible, emptyEvidence, quarantined}})

	got, err := reader.Read(t.Context(), atlasRef(), cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Evidence) != 0 {
		t.Fatalf("ineligible evidence = %+v", got.Evidence)
	}
}

func TestSourceReaderCanonicalRecordsAreStableSortedAndCopySafe(t *testing.T) {
	b := fixtureTask("b")
	b.Trace[0].Step = 9
	b.Trace[0].Evidence = []types.Evidence{{Lines: []string{"Authorization: Bearer a-secret-token-value", "bravo"}}}
	a := fixtureTask("a")
	a.Trace[0].Step = 2
	a.Trace[0].Evidence = []types.Evidence{{Lines: []string{"alpha"}}}
	store := &fixtureStore{tasks: []*types.Task{b, a}}
	reader := NewSourceReader(store)

	first, err := reader.Read(t.Context(), atlasRef(), cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Evidence) != 2 {
		t.Fatalf("evidence = %+v", first.Evidence)
	}
	if first.Evidence[0].URI != "brain-evidence://tenant-a/atlas/tasks/a#trace/2" || first.Evidence[1].URI != "brain-evidence://tenant-a/atlas/tasks/b#trace/9" {
		t.Fatalf("uris = %#v", first.Evidence)
	}
	if !strings.HasPrefix(first.Evidence[0].ID, "sha256:") || first.Evidence[0].ID == first.Evidence[1].ID {
		t.Fatalf("ids = %#v", first.Evidence)
	}
	if strings.Contains(first.Evidence[1].Content, "a-secret-token-value") || !strings.Contains(first.Evidence[1].Content, "[REDACTED]") {
		t.Fatalf("content was not sanitized: %q", first.Evidence[1].Content)
	}
	first.Evidence[0].Content = "caller mutation"
	first.SourceIDs[0] = "caller mutation"
	second, err := reader.Read(t.Context(), atlasRef(), cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if second.Evidence[0].Content != "alpha" || second.SourceIDs[0] == "caller mutation" {
		t.Fatalf("source result aliased caller mutation: %+v", second)
	}
}

func TestSourceReaderHonorsCancellationAndEvidenceBounds(t *testing.T) {
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := newFixtureReader(t).Read(cancelled, atlasRef(), cutoff); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled read error = %v", err)
	}

	tasks := make([]*types.Task, maxEvidenceRecords+1)
	for i := range tasks {
		tasks[i] = fixtureTask(fmt.Sprintf("task-%04d", i))
	}
	got, err := NewSourceReader(&fixtureStore{tasks: tasks}).Read(t.Context(), atlasRef(), cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Evidence) != maxEvidenceRecords || len(got.SourceIDs) != maxEvidenceRecords || len(got.SourceHashes) != maxEvidenceRecords {
		t.Fatalf("evidence bounds = evidence:%d ids:%d hashes:%d", len(got.Evidence), len(got.SourceIDs), len(got.SourceHashes))
	}
}

func TestSourceReaderBoundsAndSanitizesDiscoveryHintsWithoutMakingEvidence(t *testing.T) {
	task := fixtureTask("hints")
	task.Trace = nil
	task.FinalAnswer = "final Authorization: Bearer answer-secret-token"
	task.Memories = []types.Memory{{KeyFindings: strings.Repeat("x", maxDiscoveryHintBytes+100)}, {FinalAnswer: "must not be used"}}
	reader := NewSourceReader(&fixtureStore{tasks: []*types.Task{task}})

	got, err := reader.Read(t.Context(), atlasRef(), cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Evidence) != 0 || len(got.DiscoveryHints) != 2 {
		t.Fatalf("sources = %+v", got)
	}
	joined := strings.Join(got.DiscoveryHints, "\n")
	longHint := got.DiscoveryHints[0]
	if len(got.DiscoveryHints[1]) > len(longHint) {
		longHint = got.DiscoveryHints[1]
	}
	if len(longHint) != maxDiscoveryHintBytes || strings.Contains(joined, "answer-secret-token") || !strings.Contains(joined, "[REDACTED]") || strings.Contains(joined, "must not be used") {
		t.Fatalf("discovery hints = %#v", got.DiscoveryHints)
	}
}

func TestSourceReaderHydratesTraceEvidenceFromSQLiteStore(t *testing.T) {
	sqlite, err := store.NewSQLiteStore(filepath.Join(t.TempDir(), "brain-sources.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlite.Close() })
	if err := sqlite.SaveFullTask(t.Context(), fixtureTask("sqlite-trace")); err != nil {
		t.Fatal(err)
	}

	got, err := NewSourceReader(sqlite).Read(t.Context(), atlasRef(), time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if ids := sourceTaskIDs(got); len(ids) != 1 || ids[0] != "sqlite-trace" {
		t.Fatalf("persisted trace evidence task ids = %v", ids)
	}
}

func TestSourceReaderContinuesPastSparseShortPages(t *testing.T) {
	reader := NewSourceReader(&sparseFixtureStore{pages: map[int][]*types.Task{
		0: {fixtureTask("first")},
		2: {fixtureTask("later")},
	}})
	reader.PageSize = 2

	got, err := reader.Read(t.Context(), atlasRef(), cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if ids := sourceTaskIDs(got); strings.Join(ids, ",") != "first,later" {
		t.Fatalf("sparse page task ids = %v", ids)
	}
}

func TestSourceReaderNeverExceedsEvidenceByteLimits(t *testing.T) {
	first := fixtureTask("byte-first")
	first.Trace[0].Evidence = []types.Evidence{{Lines: []string{strings.Repeat("a", maxEvidenceBytes-1), "b"}}}
	got, err := NewSourceReader(&fixtureStore{tasks: []*types.Task{first}}).Read(t.Context(), atlasRef(), cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Evidence) != 1 || len(got.Evidence[0].Content) > maxEvidenceBytes {
		t.Fatalf("per-record evidence bytes = %d", len(got.Evidence[0].Content))
	}

	tasks := make([]*types.Task, maxSourceEvidenceBytes/maxEvidenceBytes+1)
	for i := range tasks {
		tasks[i] = fixtureTask(fmt.Sprintf("byte-%03d", i))
		tasks[i].Trace[0].Evidence = []types.Evidence{{Lines: []string{strings.Repeat("x", maxEvidenceBytes-1), "y"}}}
	}
	got, err = NewSourceReader(&fixtureStore{tasks: tasks}).Read(t.Context(), atlasRef(), cutoff)
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, evidence := range got.Evidence {
		total += len(evidence.Content)
	}
	if total > maxSourceEvidenceBytes {
		t.Fatalf("aggregate evidence bytes = %d", total)
	}
}

func newFixtureReader(t *testing.T) *SourceReader {
	t.Helper()
	return NewSourceReader(&fixtureStore{tasks: []*types.Task{
		fixtureTask("eligible"),
		withStatus(fixtureTask("partial"), types.StatusPartial),
		withoutAudit(fixtureTask("unaudited")),
		withAudit(fixtureTask("unpublishable"), false),
	}})
}

func newFixtureReaderWithAnswerOnly(t *testing.T) *SourceReader {
	t.Helper()
	task := fixtureTask("answer-only")
	task.Trace = nil
	task.FinalAnswer = "answer only"
	return NewSourceReader(&fixtureStore{tasks: []*types.Task{task}})
}

func atlasRef() ProjectRef {
	return ProjectRef{TenantID: "tenant-a", ProjectID: "atlas", WikiSpace: "brain-atlas", StorageKey: "sha256:tenant"}
}

func sourceTaskIDs(sources SourceSet) []string {
	ids := make([]string, len(sources.Evidence))
	for i, evidence := range sources.Evidence {
		ids[i] = evidence.TaskID
	}
	return ids
}

type fixtureStore struct{ tasks []*types.Task }

func (s *fixtureStore) ListTasks(ctx context.Context, filter store.ListFilter) ([]*types.Task, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var result []*types.Task
	for _, task := range s.tasks {
		if filter.TenantID != "" && task.TenantID != filter.TenantID || filter.Status != "" && task.Status != filter.Status {
			continue
		}
		result = append(result, types.CloneTask(task))
	}
	if filter.Offset >= len(result) {
		return []*types.Task{}, nil
	}
	result = result[filter.Offset:]
	if len(result) > filter.Limit {
		result = result[:filter.Limit]
	}
	return result, nil
}

type sparseFixtureStore struct{ pages map[int][]*types.Task }

func (s *sparseFixtureStore) ListTasks(ctx context.Context, filter store.ListFilter) ([]*types.Task, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	page := s.pages[filter.Offset]
	result := make([]*types.Task, len(page))
	for i, task := range page {
		result[i] = types.CloneTask(task)
	}
	return result, nil
}

func fixtureTask(id string) *types.Task {
	return &types.Task{
		ID:             id,
		TenantID:       "tenant-a",
		BrainProjectID: "atlas",
		Status:         types.StatusCompleted,
		UpdatedAt:      cutoff,
		AnswerAudit:    &types.AnswerAuditReport{Publishable: true},
		Trace: []types.StepTrace{{
			Step:        1,
			Action:      "web_search",
			Observation: "found external evidence",
			Evidence:    []types.Evidence{{Lines: []string{"source " + id}}},
		}},
	}
}

func withProject(task *types.Task, project string) *types.Task {
	task.BrainProjectID = project
	return task
}
func withTenant(task *types.Task, tenant string) *types.Task   { task.TenantID = tenant; return task }
func withUpdatedAt(task *types.Task, at time.Time) *types.Task { task.UpdatedAt = at; return task }
func withStatus(task *types.Task, status types.TaskStatus) *types.Task {
	task.Status = status
	return task
}
func withoutAudit(task *types.Task) *types.Task { task.AnswerAudit = nil; return task }
func withAudit(task *types.Task, publishable bool) *types.Task {
	task.AnswerAudit = &types.AnswerAuditReport{Publishable: publishable}
	return task
}
