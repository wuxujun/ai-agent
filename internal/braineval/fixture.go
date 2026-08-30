package braineval

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type EvidenceRef struct {
	Scheme string `json:"scheme"`
	Space  string `json:"space"`
	Kind   string `json:"kind"`
	ID     string `json:"id"`
}

func (r EvidenceRef) URI() string {
	if r.Scheme == "wiki" {
		return "wiki://" + r.Space + "/" + r.Kind + "/" + r.ID
	}
	return r.Scheme + "://" + r.ID
}

type TaskFixture struct {
	ID          string `json:"id"`
	RecordedAt  string `json:"recorded_at"`
	Summary     string `json:"summary"`
	EvidenceURI string `json:"evidence_uri"`
}

type SessionFixture struct {
	ID         string        `json:"id"`
	RecordedAt string        `json:"recorded_at"`
	Tasks      []TaskFixture `json:"tasks"`
}

type MemoryFixture struct {
	ID          string   `json:"id"`
	SessionID   string   `json:"session_id"`
	TaskID      string   `json:"task_id"`
	RecordedAt  string   `json:"recorded_at"`
	Goal        string   `json:"goal"`
	FinalAnswer string   `json:"final_answer"`
	KeyFindings []string `json:"key_findings"`
}

type RetractionFixture struct {
	URI         string `json:"uri"`
	RetractedAt string `json:"retracted_at"`
	Reason      string `json:"reason"`
}

type GoldClaim struct {
	Text         string   `json:"text"`
	Scope        Scope    `json:"scope"`
	PageURI      string   `json:"page_uri"`
	EvidenceURIs []string `json:"evidence_uris"`
	Supersedes   string   `json:"supersedes"`
}

type ProjectCorpus struct {
	Fixture     ProjectFixture      `json:"fixture"`
	Sessions    []SessionFixture    `json:"sessions"`
	Memories    []MemoryFixture     `json:"memories"`
	Retractions []RetractionFixture `json:"retractions"`
	Claims      []GoldClaim         `json:"claims"`
}

type Corpus struct {
	Projects map[string]*ProjectCorpus `json:"projects"`
}

var (
	evidenceLinkPattern = regexp.MustCompile(`\[evidence\]\(([^\s()]+)\)`)
	supersedesPattern   = regexp.MustCompile(`(?:^|\s)supersedes:\s*([^\s]+)`)
)

// ParseEvidenceURI accepts only canonical synthetic evidence references and
// absolute Wiki page references. Keeping the accepted grammar narrow prevents
// equivalent but differently encoded paths from bypassing scope checks.
func ParseEvidenceURI(raw string) (EvidenceRef, error) {
	if raw == "" || strings.TrimSpace(raw) != raw || strings.ContainsAny(raw, "?#%") {
		return EvidenceRef{}, fmt.Errorf("invalid evidence URI %q", raw)
	}
	parts := strings.SplitN(raw, "://", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return EvidenceRef{}, fmt.Errorf("invalid evidence URI %q", raw)
	}
	scheme, rest := parts[0], parts[1]
	switch scheme {
	case "session", "task", "memory":
		if !validURISegment(rest) {
			return EvidenceRef{}, fmt.Errorf("invalid %s evidence URI %q", scheme, raw)
		}
		return EvidenceRef{Scheme: scheme, ID: rest}, nil
	case "wiki":
		segments := strings.Split(rest, "/")
		if len(segments) != 3 || !validURISegment(segments[0]) || !validURISegment(segments[1]) || !validURISegment(segments[2]) {
			return EvidenceRef{}, fmt.Errorf("wiki evidence URI must be absolute wiki://<space>/<kind>/<slug>: %q", raw)
		}
		return EvidenceRef{Scheme: scheme, Space: segments[0], Kind: segments[1], ID: segments[2]}, nil
	default:
		return EvidenceRef{}, fmt.Errorf("unsupported evidence URI scheme %q", scheme)
	}
}

func validURISegment(segment string) bool {
	return segment != "" &&
		segment != "." &&
		segment != ".." &&
		!strings.ContainsAny(segment, "\\/:@") &&
		strings.IndexFunc(segment, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) == -1
}

func LoadCorpus(ctx context.Context, dataset Dataset) (*Corpus, error) {
	if err := dataset.Validate(); err != nil {
		return nil, err
	}
	corpus := &Corpus{Projects: make(map[string]*ProjectCorpus, len(dataset.Projects))}
	for _, fixture := range dataset.Projects {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		project, err := loadProjectCorpus(ctx, fixture)
		if err != nil {
			return nil, fmt.Errorf("load fixture %s: %w", fixture.Scope.Key(), err)
		}
		corpus.Projects[fixture.Scope.Key()] = project
	}
	if err := corpus.Validate(); err != nil {
		return nil, err
	}
	if err := validateCaseEvidence(dataset.Cases, corpus); err != nil {
		return nil, err
	}
	return corpus, nil
}

func loadProjectCorpus(ctx context.Context, fixture ProjectFixture) (*ProjectCorpus, error) {
	project := &ProjectCorpus{Fixture: fixture}
	if err := loadJSONL(filepath.Join(fixture.Root, "sessions.jsonl"), &project.Sessions); err != nil {
		return nil, fmt.Errorf("decode sessions.jsonl: %w", err)
	}
	if err := loadJSONL(filepath.Join(fixture.Root, "memories.jsonl"), &project.Memories); err != nil {
		return nil, fmt.Errorf("decode memories.jsonl: %w", err)
	}
	if err := loadJSONL(filepath.Join(fixture.Root, "retractions.jsonl"), &project.Retractions); err != nil {
		return nil, fmt.Errorf("decode retractions.jsonl: %w", err)
	}
	claims, err := loadGoldClaims(ctx, fixture)
	if err != nil {
		return nil, err
	}
	project.Claims = claims
	return project, nil
}

func loadJSONL[T any](path string, target *[]T) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		var item T
		decoder := json.NewDecoder(strings.NewReader(scanner.Text()))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&item); err != nil {
			return fmt.Errorf("line %d: %w", line, err)
		}
		if err := ensureJSONEOF(decoder); err != nil {
			return fmt.Errorf("line %d: %w", line, err)
		}
		*target = append(*target, item)
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("must contain one JSON value")
		}
		return err
	}
	return nil
}

func loadGoldClaims(ctx context.Context, fixture ProjectFixture) ([]GoldClaim, error) {
	brainRoot := filepath.Join(fixture.Root, "brain")
	indexPath := filepath.Join(brainRoot, "_index.md")
	index, err := os.ReadFile(indexPath)
	if err != nil {
		return nil, fmt.Errorf("read _index.md: %w", err)
	}
	if len(index) > 4000 {
		return nil, fmt.Errorf("_index.md exceeds 4000 UTF-8 bytes")
	}
	if !utf8.Valid(index) {
		return nil, fmt.Errorf("_index.md is not valid UTF-8")
	}
	var claims []GoldClaim
	err = filepath.WalkDir(brainRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("brain fixture must not contain symlinks: %s", path)
		}
		if entry.IsDir() || filepath.Clean(path) == filepath.Clean(indexPath) {
			return nil
		}
		if filepath.Ext(path) != ".md" {
			return fmt.Errorf("brain fixture contains non-Markdown file %s", path)
		}
		relative, err := filepath.Rel(brainRoot, path)
		if err != nil {
			return err
		}
		pieces := strings.Split(filepath.ToSlash(strings.TrimSuffix(relative, ".md")), "/")
		if len(pieces) != 2 || !validURISegment(pieces[0]) || !validURISegment(pieces[1]) {
			return fmt.Errorf("brain page path must be <kind>/<slug>.md: %s", relative)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !utf8.Valid(content) {
			return fmt.Errorf("brain page is not valid UTF-8: %s", relative)
		}
		pageURI := EvidenceRef{Scheme: "wiki", Space: fixture.Space, Kind: pieces[0], ID: pieces[1]}.URI()
		parsed, err := parseGoldClaims(content, fixture.Scope, pageURI)
		if err != nil {
			return fmt.Errorf("parse %s: %w", relative, err)
		}
		claims = append(claims, parsed...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return claims, nil
}

func parseGoldClaims(content []byte, scope Scope, pageURI string) ([]GoldClaim, error) {
	var claims []GoldClaim
	for lineNumber, line := range strings.Split(string(content), "\n") {
		if !strings.HasPrefix(line, "- ") {
			continue
		}
		body := strings.TrimPrefix(line, "- ")
		matches := evidenceLinkPattern.FindAllStringSubmatch(body, -1)
		claim := GoldClaim{Scope: scope, PageURI: pageURI}
		for _, match := range matches {
			claim.EvidenceURIs = append(claim.EvidenceURIs, match[1])
		}
		withoutEvidence := evidenceLinkPattern.ReplaceAllString(body, "")
		if strings.Contains(withoutEvidence, "[evidence]") {
			return nil, fmt.Errorf("line %d has malformed evidence citation", lineNumber+1)
		}
		supersedes := supersedesPattern.FindAllStringSubmatch(body, -1)
		if len(supersedes) > 1 {
			return nil, fmt.Errorf("line %d has multiple supersedes URIs", lineNumber+1)
		}
		if len(supersedes) == 1 {
			claim.Supersedes = supersedes[0][1]
		}
		text := supersedesPattern.ReplaceAllString(withoutEvidence, "")
		claim.Text = strings.TrimSpace(text)
		claims = append(claims, claim)
	}
	return claims, nil
}

type corpusSource struct {
	scope    Scope
	uri      string
	recorded time.Time
	hasTime  bool
}

func (c *Corpus) Validate() error {
	if c == nil || len(c.Projects) == 0 {
		return fmt.Errorf("corpus requires at least one project")
	}
	sources := make(map[string][]corpusSource)
	for key, project := range c.Projects {
		if project == nil {
			return fmt.Errorf("corpus project %q is nil", key)
		}
		if project.Fixture.Scope.Key() != key {
			return fmt.Errorf("corpus project key %q does not match fixture scope", key)
		}
		if err := collectProjectSources(project, sources); err != nil {
			return err
		}
	}
	if err := validateSourceScopeIsolation(sources); err != nil {
		return err
	}
	retracted := make(map[string]bool)
	for _, project := range c.Projects {
		for _, retraction := range project.Retractions {
			ref, err := ParseEvidenceURI(retraction.URI)
			if err != nil {
				return fmt.Errorf("retraction URI: %w", err)
			}
			source, err := resolveSource(sources, ref.URI(), project.Fixture.Scope)
			if err != nil {
				if strings.Contains(err.Error(), "unknown") {
					return fmt.Errorf("unknown retraction URI %q", retraction.URI)
				}
				return err
			}
			retractedAt, err := parseTimestamp("retraction", retraction.RetractedAt)
			if err != nil {
				return err
			}
			if !source.hasTime || !retractedAt.After(source.recorded) {
				return fmt.Errorf("retraction timestamp must be later than source %q", retraction.URI)
			}
			retracted[project.Fixture.Scope.Key()+"\x00"+ref.URI()] = true
		}
	}
	for _, project := range c.Projects {
		for _, claim := range project.Claims {
			if claim.Scope != project.Fixture.Scope {
				return fmt.Errorf("claim has cross-scope scope")
			}
			page, err := ParseEvidenceURI(claim.PageURI)
			if err != nil || page.Scheme != "wiki" || page.Space != project.Fixture.Space {
				return fmt.Errorf("claim page URI is invalid or cross-scope")
			}
			if len(claim.EvidenceURIs) == 0 {
				return fmt.Errorf("claim %q requires at least one evidence URI", claim.Text)
			}
			var newest time.Time
			for _, uri := range claim.EvidenceURIs {
				ref, err := ParseEvidenceURI(uri)
				if err != nil {
					return fmt.Errorf("claim evidence URI: %w", err)
				}
				source, err := resolveSource(sources, ref.URI(), project.Fixture.Scope)
				if err != nil {
					return err
				}
				if retracted[project.Fixture.Scope.Key()+"\x00"+ref.URI()] {
					return fmt.Errorf("claim %q cites retracted evidence %q", claim.Text, uri)
				}
				if source.hasTime && source.recorded.After(newest) {
					newest = source.recorded
				}
			}
			if claim.Supersedes == "" {
				continue
			}
			ref, err := ParseEvidenceURI(claim.Supersedes)
			if err != nil {
				return fmt.Errorf("supersedes URI: %w", err)
			}
			old, err := resolveSource(sources, ref.URI(), project.Fixture.Scope)
			if err != nil {
				return err
			}
			if !old.hasTime || newest.IsZero() || !newest.After(old.recorded) {
				return fmt.Errorf("timeline requires superseding evidence newer than %q", claim.Supersedes)
			}
		}
	}
	return nil
}

func collectProjectSources(project *ProjectCorpus, sources map[string][]corpusSource) error {
	scope := project.Fixture.Scope
	if strings.TrimSpace(scope.TenantID) == "" || strings.TrimSpace(scope.ProjectID) == "" || strings.TrimSpace(project.Fixture.Space) == "" {
		return fmt.Errorf("project fixture requires scope and space")
	}
	sessions := make(map[string]time.Time)
	tasks := make(map[string]time.Time)
	taskSessions := make(map[string]string)
	for _, session := range project.Sessions {
		if strings.TrimSpace(session.ID) == "" {
			return fmt.Errorf("session requires ID")
		}
		if _, exists := sessions[session.ID]; exists {
			return fmt.Errorf("duplicate session ID %q", session.ID)
		}
		sessionURI := (EvidenceRef{Scheme: "session", ID: session.ID}).URI()
		if _, err := ParseEvidenceURI(sessionURI); err != nil {
			return fmt.Errorf("session evidence URI is invalid for %q", session.ID)
		}
		recorded, err := parseTimestamp("session", session.RecordedAt)
		if err != nil {
			return err
		}
		sessions[session.ID] = recorded
		addSource(sources, scope, sessionURI, recorded, true)
		for _, task := range session.Tasks {
			if strings.TrimSpace(task.ID) == "" {
				return fmt.Errorf("task requires ID")
			}
			if _, exists := tasks[task.ID]; exists {
				return fmt.Errorf("duplicate task ID %q", task.ID)
			}
			taskAt, err := parseTimestamp("task", task.RecordedAt)
			if err != nil {
				return err
			}
			if taskAt.Before(recorded) {
				return fmt.Errorf("task timestamp precedes session for %q", task.ID)
			}
			ref, err := ParseEvidenceURI(task.EvidenceURI)
			if err != nil || ref.URI() != (EvidenceRef{Scheme: "task", ID: task.ID}).URI() {
				return fmt.Errorf("task evidence URI is invalid for %q", task.ID)
			}
			tasks[task.ID] = taskAt
			taskSessions[task.ID] = session.ID
			addSource(sources, scope, EvidenceRef{Scheme: "task", ID: task.ID}.URI(), taskAt, true)
		}
	}
	memories := make(map[string]struct{})
	for _, memory := range project.Memories {
		if strings.TrimSpace(memory.ID) == "" {
			return fmt.Errorf("memory requires ID")
		}
		if _, exists := memories[memory.ID]; exists {
			return fmt.Errorf("duplicate memory ID %q", memory.ID)
		}
		memoryURI := (EvidenceRef{Scheme: "memory", ID: memory.ID}).URI()
		if _, err := ParseEvidenceURI(memoryURI); err != nil {
			return fmt.Errorf("memory evidence URI is invalid for %q", memory.ID)
		}
		sessionAt, exists := sessions[memory.SessionID]
		if !exists {
			return fmt.Errorf("memory %q references unknown session %q", memory.ID, memory.SessionID)
		}
		taskAt, exists := tasks[memory.TaskID]
		if !exists {
			return fmt.Errorf("memory %q references unknown task %q", memory.ID, memory.TaskID)
		}
		if taskSessions[memory.TaskID] != memory.SessionID {
			return fmt.Errorf("memory %q task %q does not belong to session %q", memory.ID, memory.TaskID, memory.SessionID)
		}
		recorded, err := parseTimestamp("memory", memory.RecordedAt)
		if err != nil {
			return err
		}
		if recorded.Before(sessionAt) {
			return fmt.Errorf("memory timestamp precedes session for %q", memory.ID)
		}
		if recorded.Before(taskAt) {
			return fmt.Errorf("memory timestamp precedes task for %q", memory.ID)
		}
		memories[memory.ID] = struct{}{}
		addSource(sources, scope, memoryURI, recorded, true)
	}
	for _, claim := range project.Claims {
		page, err := ParseEvidenceURI(claim.PageURI)
		if err != nil {
			return fmt.Errorf("claim page URI: %w", err)
		}
		addSource(sources, scope, page.URI(), time.Time{}, false)
	}
	return nil
}

func addSource(sources map[string][]corpusSource, scope Scope, uri string, recorded time.Time, hasTime bool) {
	sources[uri] = append(sources[uri], corpusSource{scope: scope, uri: uri, recorded: recorded, hasTime: hasTime})
}

func validateSourceScopeIsolation(sources map[string][]corpusSource) error {
	for uri, candidates := range sources {
		if len(candidates) < 2 {
			continue
		}
		first := candidates[0].scope
		for _, candidate := range candidates[1:] {
			if candidate.scope != first {
				return fmt.Errorf("cross-scope URI sharing is not allowed for %q", uri)
			}
		}
	}
	return nil
}

func parseTimestamp(kind, raw string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid %s timestamp %q", kind, raw)
	}
	return parsed, nil
}

func resolveSource(sources map[string][]corpusSource, uri string, scope Scope) (corpusSource, error) {
	candidates := sources[uri]
	for _, candidate := range candidates {
		if candidate.scope == scope {
			return candidate, nil
		}
	}
	if len(candidates) > 0 {
		return corpusSource{}, fmt.Errorf("cross-scope reference to %q", uri)
	}
	return corpusSource{}, fmt.Errorf("unknown evidence URI %q", uri)
}

func validateCaseEvidence(cases []Case, corpus *Corpus) error {
	sources := make(map[string][]corpusSource)
	for _, project := range corpus.Projects {
		if err := collectProjectSources(project, sources); err != nil {
			return err
		}
	}
	for _, c := range cases {
		for _, uri := range c.ExpectedEvidenceURIs {
			ref, err := ParseEvidenceURI(uri)
			if err != nil {
				return fmt.Errorf("case %q expected evidence URI: %w", c.Name, err)
			}
			if _, err := resolveSource(sources, ref.URI(), c.Scope); err != nil {
				return fmt.Errorf("case %q: %w", c.Name, err)
			}
		}
	}
	return nil
}
