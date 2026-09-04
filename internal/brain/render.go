package brain

import (
	"fmt"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"
	"unicode"

	"gopkg.in/yaml.v3"
)

var renderableWikiKinds = map[string]bool{
	"concepts": true,
	"entities": true,
	"projects": true,
	"sources":  true,
}

type pageFrontmatter struct {
	Title   string             `yaml:"title"`
	Summary string             `yaml:"summary,omitempty"`
	Kind    string             `yaml:"kind"`
	Slug    string             `yaml:"slug"`
	Claims  []claimFrontmatter `yaml:"claims,omitempty"`
	Links   []string           `yaml:"links,omitempty"`
}

type claimFrontmatter struct {
	ID          string   `yaml:"id"`
	Text        string   `yaml:"text"`
	EvidenceIDs []string `yaml:"evidence_ids"`
	Confidence  string   `yaml:"confidence,omitempty"`
	State       string   `yaml:"state,omitempty"`
	ObservedAt  string   `yaml:"observed_at,omitempty"`
}

// Render converts an untrusted structured synthesis into canonical Markdown.
// It does not assign project identity or authorize publication; Validate owns
// those gates because it receives the resolved project reference.
func Render(input Synthesis, compactLimit int) (SnapshotDraft, error) {
	if compactLimit <= 0 {
		return SnapshotDraft{}, fmt.Errorf("brain compact index limit must be positive")
	}
	pages := append([]Page(nil), input.Pages...)
	sort.Slice(pages, func(i, j int) bool {
		if pages[i].Kind != pages[j].Kind {
			return pages[i].Kind < pages[j].Kind
		}
		return pages[i].Slug < pages[j].Slug
	})

	draft := SnapshotDraft{Files: make(map[string][]byte, len(pages)+1), Manifest: Manifest{FileHashes: make(map[string]string, len(pages)+1)}}
	seenPaths := make(map[string]bool, len(pages))
	index := strings.Builder{}
	index.WriteString("# Brain index\n")
	for _, page := range pages {
		name, err := renderedPageName(page.Kind, page.Slug)
		if err != nil {
			return SnapshotDraft{}, err
		}
		if seenPaths[name] {
			return SnapshotDraft{}, fmt.Errorf("brain synthesis has duplicate page path %q", name)
		}
		seenPaths[name] = true
		content, err := renderPage(page)
		if err != nil {
			return SnapshotDraft{}, err
		}
		draft.Files[name] = []byte(content)
		draft.Manifest.FileHashes["wiki/"+name] = digestBytes([]byte(content))
		fmt.Fprintf(&index, "- [%s](%s) — %s\n", normalizeMarkdownText(page.Title), name, normalizeMarkdownText(page.Summary))
	}
	indexContent := normalizeMarkdown(index.String())
	if len(indexContent) > compactLimit {
		return SnapshotDraft{}, fmt.Errorf("brain compact index exceeds %d bytes", compactLimit)
	}
	draft.Files["_index.md"] = []byte(indexContent)
	draft.Manifest.FileHashes["wiki/_index.md"] = digestBytes([]byte(indexContent))
	return draft, nil
}

func renderPage(page Page) (string, error) {
	if !safeMarkdownText(page.Title) || strings.TrimSpace(page.Title) == "" {
		return "", fmt.Errorf("brain page title is invalid")
	}
	if !safeMarkdownText(page.Summary) {
		return "", fmt.Errorf("brain page summary is invalid")
	}
	claims := append([]Claim(nil), page.Claims...)
	sort.Slice(claims, func(i, j int) bool { return claims[i].ID < claims[j].ID })
	frontmatter := pageFrontmatter{Title: normalizeMarkdownText(page.Title), Summary: normalizeMarkdownText(page.Summary), Kind: page.Kind, Slug: page.Slug}
	for _, claim := range claims {
		if !safeMarkdownText(claim.ID) || strings.TrimSpace(claim.ID) == "" || !safeMarkdownText(claim.Text) {
			return "", fmt.Errorf("brain claim is invalid")
		}
		evidenceIDs := append([]string(nil), claim.EvidenceIDs...)
		sort.Strings(evidenceIDs)
		frontmatter.Claims = append(frontmatter.Claims, claimFrontmatter{
			ID: claim.ID, Text: normalizeMarkdownText(claim.Text), EvidenceIDs: evidenceIDs, Confidence: normalizeMarkdownText(claim.Confidence), State: normalizeMarkdownText(claim.State), ObservedAt: formatObservedAt(claim.ObservedAt),
		})
	}
	links := append([]string(nil), page.Links...)
	sort.Strings(links)
	for _, link := range links {
		if _, _, _, err := parseBrainWikiURI(link, ""); err != nil {
			return "", fmt.Errorf("brain page link is invalid")
		}
		frontmatter.Links = append(frontmatter.Links, link)
	}
	return renderCanonicalPage(frontmatter)
}

func renderCanonicalPage(frontmatter pageFrontmatter) (string, error) {
	encoded, err := yaml.Marshal(frontmatter)
	if err != nil {
		return "", fmt.Errorf("encode brain page frontmatter: %w", err)
	}
	var output strings.Builder
	output.WriteString("---\n")
	output.Write(encoded)
	output.WriteString("---\n\n# ")
	output.WriteString(normalizeMarkdownText(frontmatter.Title))
	output.WriteString("\n\n")
	if summary := normalizeMarkdownText(frontmatter.Summary); summary != "" {
		output.WriteString(summary)
		output.WriteString("\n\n")
	}
	if len(frontmatter.Claims) > 0 {
		output.WriteString("## Claims\n\n")
		for _, claim := range frontmatter.Claims {
			output.WriteString("### ")
			output.WriteString(normalizeMarkdownText(claim.ID))
			output.WriteString("\n\n")
			output.WriteString(normalizeMarkdownText(claim.Text))
			output.WriteString("\n\n")
			if claim.Confidence != "" {
				fmt.Fprintf(&output, "- Confidence: %s\n", normalizeMarkdownText(claim.Confidence))
			}
			if claim.State != "" {
				fmt.Fprintf(&output, "- State: %s\n", normalizeMarkdownText(claim.State))
			}
			if claim.ObservedAt != "" {
				fmt.Fprintf(&output, "- Observed at: %s\n", normalizeMarkdownText(claim.ObservedAt))
			}
			if len(claim.EvidenceIDs) > 0 {
				output.WriteString("- Evidence IDs: ")
				output.WriteString(strings.Join(claim.EvidenceIDs, ", "))
				output.WriteString("\n")
			}
			output.WriteString("\n")
		}
	}
	if len(frontmatter.Links) > 0 {
		output.WriteString("## Links\n\n")
		for _, link := range frontmatter.Links {
			_, kind, slug, _ := parseBrainWikiURI(link, "")
			fmt.Fprintf(&output, "- [%s](%s) ([Markdown](../%s/%s.md))\n", link, link, kind, slug)
		}
	}
	return normalizeMarkdown(output.String()), nil
}

func renderedPageName(kind, slug string) (string, error) {
	if !renderableWikiKinds[kind] || !isProjectSlug(slug) {
		return "", fmt.Errorf("brain page path must use a safe supported kind and slug")
	}
	return path.Join(kind, slug+".md"), nil
}

func parseBrainWikiURI(raw, expectedSpace string) (space, kind, slug string, err error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "wiki" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || strings.TrimSpace(raw) != raw {
		return "", "", "", fmt.Errorf("invalid wiki URI")
	}
	if expectedSpace != "" && parsed.Host != expectedSpace {
		return "", "", "", fmt.Errorf("cross-space wiki URI")
	}
	parts := strings.Split(strings.Trim(parsed.EscapedPath(), "/"), "/")
	if len(parts) != 2 || strings.Contains(parsed.EscapedPath(), "%") || !renderableWikiKinds[parts[0]] || !isProjectSlug(parts[1]) {
		return "", "", "", fmt.Errorf("invalid wiki URI path")
	}
	return parsed.Host, parts[0], parts[1], nil
}

func normalizeMarkdown(content string) string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")
	content = strings.TrimSpace(content)
	if content == "" {
		return ""
	}
	return content + "\n"
}

func normalizeMarkdownText(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	return strings.TrimSpace(value)
}

func safeMarkdownText(value string) bool {
	return strings.IndexFunc(value, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' }) < 0
}

func formatObservedAt(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}
