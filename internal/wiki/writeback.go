package wiki

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const (
	maxWriteProposalBytes = 2 << 20
	maxWriteDiffBytes     = 64 << 10
)

var writableWikiKinds = map[string]bool{
	"comparisons": true,
	"concepts":    true,
	"entities":    true,
	"sources":     true,
}

type writeProposalFrontmatter struct {
	Title string `yaml:"title"`
}

// WriteProposalRequest describes a side-effect-free Wiki write preview.
// ExistingContent is supplied by a trusted read step; this package never reads
// or writes the Wiki itself.
type WriteProposalRequest struct {
	TaskID               string
	TargetURI            string
	Content              string
	ExistingContent      string
	ExpectedExistingHash string
}

// WriteProposal is safe to persist in an approval request. Applying it is a
// separate, high-risk operation that is deliberately not implemented here.
type WriteProposal struct {
	DryRun         bool   `json:"dry_run"`
	Operation      string `json:"operation"`
	TargetURI      string `json:"target_uri"`
	Space          string `json:"space"`
	Slug           string `json:"slug"`
	ExistingHash   string `json:"existing_hash,omitempty"`
	ContentHash    string `json:"content_hash"`
	IdempotencyKey string `json:"idempotency_key"`
	Diff           string `json:"diff"`
	DiffTruncated  bool   `json:"diff_truncated,omitempty"`
}

// BuildWriteProposal validates and previews a potential Wiki page change. It
// has no filesystem, network, registry, or MCP side effects.
func BuildWriteProposal(request WriteProposalRequest) (WriteProposal, error) {
	taskID := strings.TrimSpace(request.TaskID)
	if taskID == "" {
		return WriteProposal{}, errors.New("wiki write proposal requires task_id")
	}
	space, slug, targetURI, err := parseWritableWikiURI(request.TargetURI)
	if err != nil {
		return WriteProposal{}, err
	}
	content := normalizeWikiMarkdown(request.Content)
	if err := validateProposedMarkdown(content); err != nil {
		return WriteProposal{}, err
	}
	existing := normalizeWikiMarkdown(request.ExistingContent)
	existingHash := hashWikiContent(existing)
	if expected := strings.TrimSpace(request.ExpectedExistingHash); expected != "" && expected != existingHash {
		return WriteProposal{}, fmt.Errorf("wiki write conflict: existing content hash is %s, expected %s", existingHash, expected)
	}
	contentHash := hashWikiContent(content)
	operation := "update"
	if existing == "" {
		operation = "create"
	} else if existingHash == contentHash {
		operation = "noop"
	}
	diff, truncated := wikiDiffPreview(targetURI, existing, content)
	idempotencyHash := sha256.Sum256([]byte(taskID + "\x00" + targetURI + "\x00" + contentHash))
	return WriteProposal{
		DryRun: true, Operation: operation, TargetURI: targetURI, Space: space, Slug: slug,
		ExistingHash: existingHash, ContentHash: contentHash,
		IdempotencyKey: fmt.Sprintf("wiki-write-%x", idempotencyHash[:16]),
		Diff:           diff, DiffTruncated: truncated,
	}, nil
}

func parseWritableWikiURI(raw string) (space, slug, canonical string, err error) {
	parsed, parseErr := url.Parse(strings.TrimSpace(raw))
	if parseErr != nil || parsed.Scheme != "wiki" || parsed.Host == "" {
		return "", "", "", errors.New("wiki write target must be an absolute wiki://<space>/<kind>/<slug> URI")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", "", "", errors.New("wiki write target must not contain credentials, query, or fragment")
	}
	space = strings.TrimSpace(parsed.Host)
	slug = strings.Trim(parsed.EscapedPath(), "/")
	decodedSlug, decodeErr := url.PathUnescape(slug)
	if decodeErr != nil || decodedSlug != slug || strings.Contains(slug, "\\") || strings.Contains(slug, "..") {
		return "", "", "", errors.New("wiki write target slug must be a clean unescaped path")
	}
	parts := strings.Split(slug, "/")
	if len(parts) != 2 || !writableWikiKinds[parts[0]] || strings.TrimSpace(parts[1]) == "" || parts[1] == "." {
		return "", "", "", errors.New("wiki write target must use comparisons, concepts, entities, or sources with one page slug")
	}
	canonical = "wiki://" + space + "/" + slug
	return space, slug, canonical, nil
}

func validateProposedMarkdown(content string) error {
	if strings.TrimSpace(content) == "" {
		return errors.New("wiki write proposal content must not be empty")
	}
	if len(content) > maxWriteProposalBytes {
		return fmt.Errorf("wiki write proposal content exceeds %d bytes", maxWriteProposalBytes)
	}
	if strings.ContainsRune(content, '\x00') {
		return errors.New("wiki write proposal content contains a NUL byte")
	}
	if strings.HasPrefix(content, "---\n") {
		end := strings.Index(content[4:], "\n---\n")
		if end < 0 {
			return errors.New("wiki write proposal has unclosed YAML frontmatter")
		}
		var frontmatter writeProposalFrontmatter
		if err := yaml.Unmarshal([]byte(content[4:4+end]), &frontmatter); err != nil {
			return fmt.Errorf("wiki write proposal has invalid YAML frontmatter: %w", err)
		}
		if strings.TrimSpace(frontmatter.Title) != "" {
			return nil
		}
	}
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "# ") && strings.TrimSpace(strings.TrimPrefix(line, "# ")) != "" {
			return nil
		}
	}
	return errors.New("wiki write proposal requires a level-one heading or frontmatter title")
}

func normalizeWikiMarkdown(content string) string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")
	content = strings.TrimSpace(content)
	if content == "" {
		return ""
	}
	return content + "\n"
}

func hashWikiContent(content string) string {
	digest := sha256.Sum256([]byte(content))
	return fmt.Sprintf("sha256:%x", digest[:])
}

func wikiDiffPreview(target, before, after string) (string, bool) {
	if before == after {
		return "", false
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, "--- %s\n+++ %s\n", target, target)
	for _, line := range strings.Split(strings.TrimSuffix(before, "\n"), "\n") {
		if before != "" {
			builder.WriteString("-")
			builder.WriteString(line)
			builder.WriteByte('\n')
		}
	}
	for _, line := range strings.Split(strings.TrimSuffix(after, "\n"), "\n") {
		builder.WriteString("+")
		builder.WriteString(line)
		builder.WriteByte('\n')
	}
	diff := builder.String()
	if len(diff) <= maxWriteDiffBytes {
		return diff, false
	}
	limit := maxWriteDiffBytes
	for limit > 0 && !utf8.RuneStart(diff[limit]) {
		limit--
	}
	return diff[:limit] + "\n... (diff truncated)\n", true
}
