package braineval

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
	"github.com/wuxujun/ai-agent/internal/wiki"
)

const (
	BranchCandidateLimit = 8
	FinalEvidenceLimit   = 3
	FinalEvidenceBytes   = 8000
	RRFK                 = 60.0
)

type Candidate struct {
	URI     string
	Branch  string
	Snippet string
	Rank    int
	Score   float64
}

type Retriever interface {
	Search(context.Context, Scope, string, int) ([]Candidate, error)
	Fetch(context.Context, Scope, Candidate) (types.Evidence, error)
}

// MergeRRF combines independently ranked branches with reciprocal-rank
// fusion. A URI contributes at most once per branch.
func MergeRRF(branches [][]Candidate, k float64) []Candidate {
	merged := make(map[string]Candidate)
	for _, branch := range branches {
		seen := make(map[string]struct{}, len(branch))
		for position, candidate := range branch {
			uri := canonicalURI(candidate.URI)
			if _, duplicate := seen[uri]; duplicate {
				continue
			}
			seen[uri] = struct{}{}

			rank := candidate.Rank
			if rank <= 0 {
				rank = position + 1
			}
			if k+float64(rank) <= 0 {
				continue
			}
			contribution := 1 / (k + float64(rank))
			current, exists := merged[uri]
			if !exists {
				candidate.URI = uri
				candidate.Score = contribution
				merged[uri] = candidate
				continue
			}
			current.Score += contribution
			if current.Snippet == "" {
				current.Snippet = candidate.Snippet
			}
			if current.Branch == "" {
				current.Branch = candidate.Branch
			}
			merged[uri] = current
		}
	}

	result := make([]Candidate, 0, len(merged))
	for _, candidate := range merged {
		result = append(result, candidate)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Score == result[j].Score {
			return result[i].URI < result[j].URI
		}
		return result[i].Score > result[j].Score
	})
	for i := range result {
		result[i].Rank = i + 1
	}
	return result
}

// SelectEvidence fetches candidates in rank order and applies an item limit
// and a byte limit to the joined evidence lines. Oversized first items are
// truncated at a UTF-8 rune boundary; later items that do not fit are skipped.
func SelectEvidence(ctx context.Context, retriever Retriever, scope Scope, ranked []Candidate, maxItems, maxBytes int) ([]types.Evidence, error) {
	if maxItems <= 0 || maxBytes <= 0 {
		return nil, nil
	}
	maxItems = min(maxItems, FinalEvidenceLimit)
	maxBytes = min(maxBytes, FinalEvidenceBytes)
	selected := make([]types.Evidence, 0, min(maxItems, len(ranked)))
	usedBytes := 0
	for _, candidate := range ranked {
		if len(selected) >= maxItems {
			break
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		evidence, err := retriever.Fetch(ctx, scope, candidate)
		if err != nil {
			return nil, err
		}
		lines, size := capEvidenceLines(evidence.Lines, maxBytes)
		if size == 0 || size > maxBytes-usedBytes {
			continue
		}
		evidence.Lines = lines
		selected = append(selected, evidence)
		usedBytes += size
	}
	return selected, nil
}

type memoryProject struct {
	store    *store.MemoryStore
	memories map[string]*types.Memory
}

// MemoryRetriever adapts one isolated in-memory Store per corpus project.
// The project-specific stores compensate for types.Memory lacking ProjectID.
type MemoryRetriever struct {
	projects map[string]memoryProject
}

func NewMemoryRetriever(ctx context.Context, corpus *Corpus) (*MemoryRetriever, error) {
	if err := corpus.Validate(); err != nil {
		return nil, fmt.Errorf("validate corpus: %w", err)
	}
	retriever := &MemoryRetriever{projects: make(map[string]memoryProject, len(corpus.Projects))}
	for key, project := range corpus.Projects {
		memoryStore := store.NewMemoryStore()
		memories := make(map[string]*types.Memory, len(project.Memories))
		for _, fixture := range project.Memories {
			timestamp, err := parseTimestamp("memory", fixture.RecordedAt)
			if err != nil {
				return nil, err
			}
			memory := &types.Memory{
				ID: fixture.ID, TenantID: project.Fixture.Scope.TenantID,
				SessionID: fixture.SessionID, TaskID: fixture.TaskID,
				Goal: fixture.Goal, FinalAnswer: fixture.FinalAnswer,
				KeyFindings: strings.Join(fixture.KeyFindings, "\n"), Timestamp: timestamp,
			}
			if err := memoryStore.SaveMemory(ctx, memory); err != nil {
				return nil, fmt.Errorf("save memory %q: %w", fixture.ID, err)
			}
			memories[fixture.ID] = memory
		}
		retriever.projects[key] = memoryProject{store: memoryStore, memories: memories}
	}
	return retriever, nil
}

func (r *MemoryRetriever) Search(ctx context.Context, scope Scope, query string, limit int) ([]Candidate, error) {
	project, ok := r.projects[scope.Key()]
	if !ok {
		return nil, fmt.Errorf("unknown retrieval scope %q", scope.Key())
	}
	memories, err := project.store.QueryMemories(store.WithTenantScope(ctx, scope.TenantID), query, nil, BranchCandidateLimit)
	if err != nil {
		return nil, err
	}
	queryTokens := normalizedTokens(query)
	candidates := make([]Candidate, 0, len(memories))
	for _, memory := range memories {
		if memory == nil || memory.TenantID != scope.TenantID {
			continue
		}
		uri := (EvidenceRef{Scheme: "memory", ID: memory.ID}).URI()
		if err := validateMemoryURI(uri, project.memories); err != nil {
			continue
		}
		score := lexicalOverlap(queryTokens, normalizedTokens(memory.Goal+" "+memory.KeyFindings+" "+memory.FinalAnswer))
		if score == 0 {
			continue
		}
		candidates = append(candidates, Candidate{
			URI: uri, Branch: "memory", Snippet: strings.Join(memoryEvidenceLines(memory), "\n"), Score: score,
		})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Score == candidates[j].Score {
			return candidates[i].URI < candidates[j].URI
		}
		return candidates[i].Score > candidates[j].Score
	})
	limit = boundedBranchLimit(limit)
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}
	for i := range candidates {
		candidates[i].Rank = i + 1
	}
	return candidates, nil
}

func (r *MemoryRetriever) Fetch(ctx context.Context, scope Scope, candidate Candidate) (types.Evidence, error) {
	if err := ctx.Err(); err != nil {
		return types.Evidence{}, err
	}
	project, ok := r.projects[scope.Key()]
	if !ok {
		return types.Evidence{}, fmt.Errorf("unknown retrieval scope %q", scope.Key())
	}
	ref, err := ParseEvidenceURI(candidate.URI)
	if err != nil || ref.Scheme != "memory" || ref.URI() != candidate.URI {
		return types.Evidence{}, fmt.Errorf("invalid memory candidate URI %q", candidate.URI)
	}
	memory, ok := project.memories[ref.ID]
	if !ok || memory.TenantID != scope.TenantID {
		return types.Evidence{}, fmt.Errorf("memory URI %q is outside scope %q", candidate.URI, scope.Key())
	}
	return types.Evidence{Path: ref.URI(), Lines: memoryEvidenceLines(memory)}, nil
}

type wikiProject struct {
	client *wiki.DirectoryClient
	space  string
}

// WikiRetriever adapts one read-only Wiki Directory client per project.
type WikiRetriever struct {
	projects map[string]wikiProject
}

func NewWikiRetriever(ctx context.Context, corpus *Corpus) (*WikiRetriever, error) {
	if err := corpus.Validate(); err != nil {
		return nil, fmt.Errorf("validate corpus: %w", err)
	}
	retriever := &WikiRetriever{projects: make(map[string]wikiProject, len(corpus.Projects))}
	for key, project := range corpus.Projects {
		client, err := wiki.NewDirectory(filepath.Join(project.Fixture.Root, "brain"))
		if err != nil {
			return nil, fmt.Errorf("create Wiki directory for scope %q: %w", key, err)
		}
		if err := client.Initialize(ctx); err != nil {
			return nil, fmt.Errorf("initialize Wiki directory for scope %q: %w", key, err)
		}
		retriever.projects[key] = wikiProject{client: client, space: project.Fixture.Space}
	}
	return retriever, nil
}

func (r *WikiRetriever) Search(ctx context.Context, scope Scope, query string, limit int) ([]Candidate, error) {
	project, ok := r.projects[scope.Key()]
	if !ok {
		return nil, fmt.Errorf("unknown retrieval scope %q", scope.Key())
	}
	documents, err := project.client.Search(ctx, query, BranchCandidateLimit, project.space)
	if err != nil {
		return nil, err
	}
	limit = boundedBranchLimit(limit)
	candidates := make([]Candidate, 0, min(limit, len(documents)))
	for _, document := range documents {
		if isWikiNavigationDocument(document.Slug) {
			continue
		}
		if err := validateWikiURI(document.URI, project.space); err != nil {
			return nil, err
		}
		candidates = append(candidates, Candidate{
			URI: document.URI, Branch: "brain", Snippet: firstNonBlank(document.Excerpt, document.Summary, document.Title), Score: document.Score,
		})
		if len(candidates) == limit {
			break
		}
	}
	for i := range candidates {
		candidates[i].Rank = i + 1
	}
	return candidates, nil
}

func isWikiNavigationDocument(slug string) bool {
	base := strings.ToLower(filepath.Base(slug))
	return base == "index" || base == "_index" || base == "log"
}

func (r *WikiRetriever) Fetch(ctx context.Context, scope Scope, candidate Candidate) (types.Evidence, error) {
	project, ok := r.projects[scope.Key()]
	if !ok {
		return types.Evidence{}, fmt.Errorf("unknown retrieval scope %q", scope.Key())
	}
	ref, err := ParseEvidenceURI(candidate.URI)
	if err != nil || ref.Scheme != "wiki" || ref.Space != project.space || ref.URI() != candidate.URI {
		return types.Evidence{}, fmt.Errorf("Wiki URI %q is outside scope %q", candidate.URI, scope.Key())
	}
	document, err := project.client.Read(ctx, wiki.Document{URI: ref.URI(), Slug: ref.Kind + "/" + ref.ID}, project.space)
	if err != nil {
		return types.Evidence{}, err
	}
	if err := validateWikiURI(document.URI, project.space); err != nil {
		return types.Evidence{}, err
	}
	return types.Evidence{Path: document.URI, Lines: strings.Split(document.Content, "\n")}, nil
}

func boundedBranchLimit(limit int) int {
	if limit <= 0 || limit > BranchCandidateLimit {
		return BranchCandidateLimit
	}
	return limit
}

func canonicalURI(uri string) string {
	ref, err := ParseEvidenceURI(uri)
	if err != nil {
		return uri
	}
	return ref.URI()
}

func validateMemoryURI(uri string, memories map[string]*types.Memory) error {
	ref, err := ParseEvidenceURI(uri)
	if err != nil || ref.Scheme != "memory" || ref.URI() != uri {
		return fmt.Errorf("invalid memory URI %q", uri)
	}
	if _, ok := memories[ref.ID]; !ok {
		return fmt.Errorf("memory URI %q is outside project", uri)
	}
	return nil
}

func validateWikiURI(uri, space string) error {
	ref, err := ParseEvidenceURI(uri)
	if err != nil || ref.Scheme != "wiki" || ref.Space != space || ref.URI() != uri {
		return fmt.Errorf("Wiki URI %q is outside space %q", uri, space)
	}
	return nil
}

func normalizedTokens(text string) map[string]struct{} {
	tokens := make(map[string]struct{})
	for _, token := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	}) {
		if token != "" {
			tokens[token] = struct{}{}
		}
	}
	return tokens
}

func lexicalOverlap(query, document map[string]struct{}) float64 {
	if len(query) == 0 {
		return 0
	}
	matches := 0
	for token := range query {
		if _, ok := document[token]; ok {
			matches++
		}
	}
	return float64(matches) / float64(len(query))
}

func memoryEvidenceLines(memory *types.Memory) []string {
	lines := make([]string, 0, 3)
	for _, value := range []string{memory.Goal, memory.FinalAnswer, memory.KeyFindings} {
		if value != "" {
			lines = append(lines, strings.Split(value, "\n")...)
		}
	}
	return lines
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func capEvidenceLines(lines []string, maxBytes int) ([]string, int) {
	if maxBytes <= 0 {
		return nil, 0
	}
	content := strings.ToValidUTF8(strings.Join(lines, "\n"), "\uFFFD")
	if len(content) > maxBytes {
		end := maxBytes
		for end > 0 && !utf8.ValidString(content[:end]) {
			end--
		}
		content = content[:end]
	}
	if content == "" {
		return nil, 0
	}
	return strings.Split(content, "\n"), len(content)
}
