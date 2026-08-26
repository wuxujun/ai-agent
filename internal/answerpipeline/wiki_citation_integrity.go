package answerpipeline

import (
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/wuxujun/ai-agent/internal/types"
)

const wikiCitationIntegrityStage = "wiki_citation_integrity"

var wikiCitationTokenPattern = regexp.MustCompile(`wiki:[^\s<>"'\]\)]+`)

func (p *DefaultPipeline) runWikiCitationIntegrity(task *types.Task, report *types.AnswerAuditReport, prior *types.AnswerAuditReport) {
	started := time.Now()
	fingerprint := stageFingerprint(wikiCitationIntegrityStage, task.FinalAnswer, report.EvidenceHash, report.Enforcement)
	if cached, ok := reusableStage(prior, wikiCitationIntegrityStage, fingerprint); ok {
		report.Stages = append(report.Stages, cached)
		return
	}
	if task.Team == "wiki_suggest" {
		report.Stages = append(report.Stages, v2Stage(wikiCitationIntegrityStage, "not_applicable", "curation_suggestions_may_reference_unfetched_targets", types.TokenUsage{}, nil, started, fingerprint))
		return
	}

	allowed := fetchedWikiEvidence(task)
	citations := wikiCitations(task.FinalAnswer)
	if len(allowed) == 0 && len(citations) == 0 {
		report.Stages = append(report.Stages, v2Stage(wikiCitationIntegrityStage, "not_applicable", "no_wiki_evidence", types.TokenUsage{}, nil, started, fingerprint))
		return
	}

	findings := make([]types.AnswerAuditFinding, 0)
	if len(allowed) > 0 && len(citations) == 0 {
		findings = append(findings, types.AnswerAuditFinding{Kind: "missing_wiki_citation", Detail: "answer contains fetched Wiki evidence but no wiki:// citation"})
	}
	for _, citation := range citations {
		if !validWikiCitation(citation) {
			findings = append(findings, types.AnswerAuditFinding{Kind: "invalid_wiki_uri", SourceID: boundedAuditValue(citation, 500)})
			continue
		}
		if _, ok := allowed[citation]; !ok {
			findings = append(findings, types.AnswerAuditFinding{Kind: "unfetched_wiki_citation", Detail: "citation was not present in Wiki fetch evidence for this task", SourceID: boundedAuditValue(citation, 500)})
		}
	}
	status := "passed"
	reason := ""
	if len(findings) > 0 {
		status = "failed"
		if report.Enforcement == "observe" {
			status = "warned"
		}
		reason = "wiki citation integrity findings"
	}
	report.Stages = append(report.Stages, v2Stage(wikiCitationIntegrityStage, status, reason, types.TokenUsage{}, findings, started, fingerprint))
}

func fetchedWikiEvidence(task *types.Task) map[string]struct{} {
	allowed := make(map[string]struct{})
	for _, trace := range task.Trace {
		if trace.Action != "wiki_fetch" && trace.Action != "wiki_graph_fetch" {
			continue
		}
		for _, evidence := range trace.Evidence {
			path := strings.TrimSpace(evidence.Path)
			if validWikiCitation(path) {
				allowed[path] = struct{}{}
			}
		}
	}
	return allowed
}

func wikiCitations(answer string) []string {
	seen := make(map[string]struct{})
	for _, token := range wikiCitationTokenPattern.FindAllString(answer, -1) {
		token = strings.TrimRight(token, ".,;:!?\uff0c\u3002\uff1b\uff1a\uff01\uff1f")
		if token != "" {
			seen[token] = struct{}{}
		}
	}
	items := make([]string, 0, len(seen))
	for item := range seen {
		items = append(items, item)
	}
	sort.Strings(items)
	return items
}

func validWikiCitation(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "wiki" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Port() != "" {
		return false
	}
	path := strings.TrimPrefix(parsed.EscapedPath(), "/")
	if path == "" || strings.Contains(path, "//") {
		return false
	}
	for _, segment := range strings.Split(path, "/") {
		decoded, err := url.PathUnescape(segment)
		if err != nil || decoded == "" || decoded == "." || decoded == ".." || strings.ContainsAny(decoded, `\\`) {
			return false
		}
	}
	return true
}
