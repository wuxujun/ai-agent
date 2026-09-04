package brain

import (
	"bytes"
	"context"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/wuxujun/ai-agent/internal/sanitize"
	"github.com/wuxujun/ai-agent/internal/wiki"
	"gopkg.in/yaml.v3"
)

// Validate applies deterministic, fail-closed publication gates to a rendered
// snapshot. Findings deliberately contain paths and stable codes only; they
// never include page bodies, evidence, prompts, credentials, or filesystem paths.
func Validate(ctx context.Context, ref ProjectRef, draft SnapshotDraft, ledger RetractionView) ValidationReport {
	validator := snapshotValidator{ctx: ctx, ref: ref, draft: draft, ledger: ledger}
	validator.validate()
	sort.Slice(validator.findings, func(i, j int) bool {
		if validator.findings[i].Code != validator.findings[j].Code {
			return validator.findings[i].Code < validator.findings[j].Code
		}
		if validator.findings[i].Path != validator.findings[j].Path {
			return validator.findings[i].Path < validator.findings[j].Path
		}
		return validator.findings[i].Message < validator.findings[j].Message
	})
	return ValidationReport{Publishable: len(validator.findings) == 0, Findings: validator.findings}
}

type snapshotValidator struct {
	ctx             context.Context
	ref             ProjectRef
	draft           SnapshotDraft
	ledger          RetractionView
	retractionReady bool
	findings        []ValidationFinding
}

func (v *snapshotValidator) validate() {
	if err := v.ctx.Err(); err != nil {
		v.add("context_canceled", "snapshot")
		return
	}
	if strings.TrimSpace(v.ref.TenantID) == "" || strings.TrimSpace(v.ref.ProjectID) == "" || strings.TrimSpace(v.ref.WikiSpace) == "" {
		v.add("invalid_project_scope", "snapshot")
		return
	}
	if v.draft.Manifest.TenantID != "" && v.draft.Manifest.TenantID != v.ref.TenantID || v.draft.Manifest.ProjectID != "" && v.draft.Manifest.ProjectID != v.ref.ProjectID {
		v.add("project_scope_mismatch", "manifest")
	}
	v.validateRetractionLedger()
	v.validateOutputBounds()
	evidence := v.validateEvidence()
	claims := v.validateFiles()
	v.validateClaimCoverage(claims, evidence)
	v.validateManifestHashes()
}

func (v *snapshotValidator) validateRetractionLedger() {
	if v.ledger == nil {
		v.add("retraction_unavailable", "snapshot")
		return
	}
	watermark, err := v.ledger.Watermark(v.ctx, v.ref)
	if err != nil {
		v.add("retraction_unavailable", "snapshot")
		return
	}
	v.retractionReady = true
	if v.draft.Manifest.RetractionWatermark != watermark {
		v.add("retraction_changed", "manifest")
	}
}

func (v *snapshotValidator) validateOutputBounds() {
	if len(v.draft.Files) == 0 || len(v.draft.Files)+1 > maxSnapshotFiles || len(v.draft.Evidence) > maxSnapshotEvidenceRecords {
		v.add("oversize_output", "wiki")
	}
	totalBytes := 0
	filesWithinBounds := true
	for _, content := range v.draft.Files {
		if len(content) > maxSnapshotFileBytes || totalBytes > maxSnapshotTreeBytes-len(content) {
			v.add("oversize_output", "wiki")
			filesWithinBounds = false
			continue
		}
		totalBytes += len(content)
	}
	evidenceBytes, err := encodeEvidence(v.draft.Evidence)
	if err != nil || !filesWithinBounds || totalBytes > maxSnapshotTreeBytes-len(evidenceBytes) {
		v.add("oversize_output", "evidence")
	} else {
		totalBytes += len(evidenceBytes)
	}
	manifestBytes, err := encodeManifest(v.draft.Manifest)
	if err != nil || len(manifestBytes) > maxSnapshotManifestBytes || !filesWithinBounds || totalBytes > maxSnapshotTreeBytes-len(manifestBytes) {
		v.add("oversize_output", "manifest")
	}
}

func (v *snapshotValidator) validateEvidence() map[string]EvidenceRecord {
	evidence := make(map[string]EvidenceRecord, len(v.draft.Evidence))
	for _, record := range v.draft.Evidence {
		if err := v.ctx.Err(); err != nil {
			v.add("context_canceled", "evidence")
			break
		}
		if record.ID == "" || record.URI == "" {
			v.add("invalid_evidence", "evidence")
			continue
		}
		if _, exists := evidence[record.ID]; exists {
			v.add("duplicate_evidence_id", "evidence")
			continue
		}
		evidence[record.ID] = record
		step, err := strconv.Atoi(record.TraceStep)
		canonical, canonicalErr := canonicalEvidenceURI(v.ref, record.TaskID, step)
		if err != nil || canonicalErr != nil || record.URI != canonical {
			v.add("cross_space_uri", "evidence")
		}
		if record.ID != sha256ID(record.URI) || record.ContentHash != sha256ID(record.Content) {
			v.add("hash_mismatch", "evidence")
		}
		v.validateUnsafeContent("evidence", record.Content)
		if !v.retractionReady {
			continue
		}
		retracted, ledgerErr := v.ledger.Contains(v.ctx, v.ref, record.URI)
		if ledgerErr != nil {
			v.add("retraction_unavailable", "evidence")
		} else if retracted {
			v.add("retracted_source", "evidence")
		}
	}
	return evidence
}

func (v *snapshotValidator) validateFiles() []claimFrontmatter {
	claims := make([]claimFrontmatter, 0)
	files := make([]string, 0, len(v.draft.Files))
	for name := range v.draft.Files {
		files = append(files, name)
	}
	sort.Strings(files)
	seenClaims := make(map[string]bool)
	for _, name := range files {
		content := v.draft.Files[name]
		if len(name) > maxSnapshotPathBytes || len(content) > maxSnapshotFileBytes {
			v.add("oversize_output", "wiki")
			continue
		}
		v.validateUnsafeContent(name, string(content))
		if name == "_index.md" {
			if !validNormalizedMarkdown(content) {
				v.add("malformed_markdown", name)
			}
			continue
		}
		kind, slug, ok := parseRenderedPageName(name)
		if !ok {
			v.add("unsafe_path", name)
			continue
		}
		if _, err := wiki.BuildWriteProposal(wiki.WriteProposalRequest{TaskID: "brain-validate", TargetURI: "wiki://" + v.ref.WikiSpace + "/" + kind + "/" + slug, Content: string(content)}); err != nil {
			v.add("invalid_write_proposal", name)
		}
		frontmatter, ok := parsePageFrontmatter(content)
		if !ok || frontmatter.Kind != kind || frontmatter.Slug != slug || strings.TrimSpace(frontmatter.Title) == "" {
			v.add("malformed_frontmatter", name)
			continue
		}
		if !validNormalizedMarkdown(content) {
			v.add("malformed_markdown", name)
		}
		if !bodyClaimsMatchFrontmatter(content, frontmatter.Claims) {
			v.add("body_claim_drift", name)
		}
		for _, link := range frontmatter.Links {
			if _, _, _, err := parseBrainWikiURI(link, v.ref.WikiSpace); err != nil {
				if _, _, _, crossErr := parseBrainWikiURI(link, ""); crossErr == nil {
					v.add("cross_space_uri", name)
				} else {
					v.add("unsafe_link", name)
				}
			}
		}
		if !validMarkdownLinks(content, v.ref.WikiSpace) {
			v.add("unsafe_link", name)
		}
		for _, claim := range frontmatter.Claims {
			if strings.TrimSpace(claim.ID) == "" || seenClaims[claim.ID] {
				v.add("duplicate_claim_id", name)
				continue
			}
			seenClaims[claim.ID] = true
			claims = append(claims, claim)
		}
	}
	return claims
}

func (v *snapshotValidator) validateClaimCoverage(claims []claimFrontmatter, evidence map[string]EvidenceRecord) {
	used := make(map[string]bool, len(evidence))
	for _, claim := range claims {
		if len(claim.EvidenceIDs) == 0 {
			v.add("unreferenced_claim", "claim")
			continue
		}
		seen := make(map[string]bool, len(claim.EvidenceIDs))
		for _, id := range claim.EvidenceIDs {
			if id == "" || seen[id] || evidence[id].ID == "" {
				v.add("missing_evidence", "claim")
				continue
			}
			seen[id] = true
			used[id] = true
		}
	}
	for id := range evidence {
		if !used[id] {
			v.add("unreferenced_evidence", "evidence")
		}
	}
	if len(v.draft.Manifest.SourceIDs) != len(v.draft.Manifest.SourceHashes) || len(v.draft.Manifest.SourceIDs) != len(evidence) {
		v.add("hash_mismatch", "manifest")
		return
	}
	seen := make(map[string]bool, len(v.draft.Manifest.SourceIDs))
	for index, id := range v.draft.Manifest.SourceIDs {
		record, exists := evidence[id]
		if !exists || seen[id] || v.draft.Manifest.SourceHashes[index] != record.ContentHash {
			v.add("hash_mismatch", "manifest")
		}
		seen[id] = true
	}
}

func (v *snapshotValidator) validateManifestHashes() {
	if len(v.draft.Manifest.FileHashes) == 0 {
		v.add("hash_mismatch", "manifest")
		return
	}
	if len(v.draft.Manifest.FileHashes) != len(v.draft.Files) {
		v.add("hash_mismatch", "manifest")
		return
	}
	for name, content := range v.draft.Files {
		digest, exists := v.draft.Manifest.FileHashes["wiki/"+name]
		if !exists || !validSHA256Digest(digest) || digest != digestBytes(content) {
			v.add("hash_mismatch", "manifest")
		}
	}
}

func (v *snapshotValidator) validateUnsafeContent(location, content string) {
	lower := strings.ToLower(content)
	if strings.Contains(lower, "ignore previous instructions") || strings.Contains(lower, "disregard previous instructions") || strings.Contains(lower, "reveal the system prompt") || strings.Contains(lower, "system prompt") {
		v.add("prompt_injection", location)
	}
	if sanitize.Secrets(content) != content {
		v.add("secret", location)
	}
}

func (v *snapshotValidator) add(code, location string) {
	v.findings = append(v.findings, ValidationFinding{Code: code, Path: findingPath(location), Message: "brain snapshot validation failed", Hard: true})
}

func findingPath(location string) string {
	switch {
	case strings.HasPrefix(location, "evidence"):
		return "evidence"
	case strings.HasPrefix(location, "claim"):
		return "claim"
	case strings.HasPrefix(location, "manifest"):
		return "manifest"
	case strings.HasPrefix(location, "snapshot"):
		return "snapshot"
	case strings.HasPrefix(location, "_index"):
		return "index"
	default:
		return "wiki"
	}
}

func parseRenderedPageName(name string) (kind, slug string, ok bool) {
	if strings.Contains(name, "\\") || path.Clean(name) != name {
		return "", "", false
	}
	parts := strings.Split(name, "/")
	if len(parts) != 2 || !strings.HasSuffix(parts[1], ".md") {
		return "", "", false
	}
	slug = strings.TrimSuffix(parts[1], ".md")
	if _, err := renderedPageName(parts[0], slug); err != nil {
		return "", "", false
	}
	return parts[0], slug, true
}

func parsePageFrontmatter(content []byte) (pageFrontmatter, bool) {
	text := string(content)
	if !strings.HasPrefix(text, "---\n") {
		return pageFrontmatter{}, false
	}
	end := strings.Index(text[4:], "\n---\n")
	if end < 0 {
		return pageFrontmatter{}, false
	}
	decoder := yaml.NewDecoder(bytes.NewReader([]byte(text[4 : 4+end])))
	decoder.KnownFields(true)
	var frontmatter pageFrontmatter
	if err := decoder.Decode(&frontmatter); err != nil {
		return pageFrontmatter{}, false
	}
	return frontmatter, true
}

func validNormalizedMarkdown(content []byte) bool {
	return len(content) > 0 && !strings.Contains(string(content), "\r") && content[len(content)-1] == '\n' && (len(content) == 1 || content[len(content)-2] != '\n')
}

func validMarkdownLinks(content []byte, space string) bool {
	text := string(content)
	for rest := text; ; {
		start := strings.Index(rest, "](")
		if start < 0 {
			return true
		}
		rest = rest[start+2:]
		end := strings.IndexByte(rest, ')')
		if end < 0 {
			return false
		}
		link := rest[:end]
		if strings.HasPrefix(link, "wiki://") {
			if _, _, _, err := parseBrainWikiURI(link, space); err != nil {
				return false
			}
		} else if !validRelativeMarkdownLink(link) {
			return false
		}
		rest = rest[end+1:]
	}
}

func validRelativeMarkdownLink(link string) bool {
	if !strings.HasPrefix(link, "../") || strings.Contains(link, "\\") || strings.Contains(link, "?") || strings.Contains(link, "#") {
		return false
	}
	_, _, ok := parseRenderedPageName(strings.TrimPrefix(link, "../"))
	return ok
}

func bodyClaimsMatchFrontmatter(content []byte, claims []claimFrontmatter) bool {
	lines := strings.Split(string(content), "\n")
	actual := make([]string, 0, len(claims))
	inClaims, seenClaimsSection := false, false
	for _, line := range lines {
		if line == "## Claims" {
			if seenClaimsSection {
				return false
			}
			seenClaimsSection = true
			inClaims = true
			continue
		}
		if inClaims && strings.HasPrefix(line, "## ") {
			inClaims = false
			continue
		}
		if inClaims && strings.HasPrefix(line, "### ") {
			actual = append(actual, strings.TrimSpace(strings.TrimPrefix(line, "### ")))
		}
	}
	if len(actual) != len(claims) {
		return false
	}
	for index, claim := range claims {
		if actual[index] != claim.ID {
			return false
		}
	}
	return true
}
