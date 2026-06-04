package skills

import (
	"bufio"
	"strings"
)

// frontmatter holds the fields we read from a SKILL.md YAML header. Only a tiny,
// fixed subset is supported, so we parse it with a minimal hand-written reader
// rather than pulling in a full YAML dependency. Supported keys:
//
//	name:          string (required)
//	description:   string (required for prompt usefulness)
//	allowed-tools: comma-separated list, OR a YAML block-list of "- item" lines
type frontmatter struct {
	Name         string
	Description  string
	AllowedTools []string
}

// parseFrontmatter extracts the YAML frontmatter delimited by leading and
// trailing "---" lines. Returns ok=false when no frontmatter block is present.
//
// This intentionally handles only flat "key: value" pairs plus a single
// block-list for allowed-tools. SKILL.md authors who need richer YAML should
// keep the frontmatter simple; the body is free-form Markdown.
func parseFrontmatter(content string) (frontmatter, bool) {
	block, ok := extractFrontmatterBlock(content)
	if !ok {
		return frontmatter{}, false
	}

	var fm frontmatter
	sc := bufio.NewScanner(strings.NewReader(block))
	var (
		inList   bool
		listInto *[]string
	)
	for sc.Scan() {
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		// Continuation of a YAML block-list ("  - item").
		if inList {
			if strings.HasPrefix(trimmed, "- ") {
				*listInto = append(*listInto, strings.TrimSpace(strings.TrimPrefix(trimmed, "- ")))
				continue
			}
			inList = false
			listInto = nil
		}

		key, val, found := strings.Cut(trimmed, ":")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		val = strings.Trim(val, `"'`)

		switch key {
		case "name":
			fm.Name = val
		case "description":
			fm.Description = val
		case "allowed-tools", "allowed_tools":
			if val == "" {
				// Block-list form follows on subsequent "- item" lines.
				inList = true
				listInto = &fm.AllowedTools
			} else {
				// Inline comma-separated form.
				for _, t := range strings.Split(val, ",") {
					if t = strings.TrimSpace(t); t != "" {
						fm.AllowedTools = append(fm.AllowedTools, t)
					}
				}
			}
		}
	}
	return fm, true
}

// stripFrontmatter returns the Markdown body with the leading frontmatter block
// removed. If there is no frontmatter, the original content is returned intact.
func stripFrontmatter(content string) string {
	if _, ok := extractFrontmatterBlock(content); !ok {
		return content
	}
	// Drop everything up to and including the closing delimiter.
	rest := strings.TrimLeft(content, "?") // tolerate BOM
	rest = strings.TrimLeft(rest, "\r\n")
	rest = strings.TrimPrefix(rest, "---")
	if idx := strings.Index(rest, "\n---"); idx >= 0 {
		body := rest[idx+len("\n---"):]
		return strings.TrimLeft(body, "\r\n")
	}
	return content
}

// extractFrontmatterBlock returns the inner text between the opening and closing
// "---" delimiters. The document must begin with "---" (after an optional BOM
// and blank lines) for a frontmatter block to be recognized.
func extractFrontmatterBlock(content string) (string, bool) {
	s := strings.TrimLeft(content, "?")
	s = strings.TrimLeft(s, "\r\n")
	if !strings.HasPrefix(s, "---") {
		return "", false
	}
	s = strings.TrimPrefix(s, "---")
	s = strings.TrimLeft(s, "\r\n")
	idx := strings.Index(s, "\n---")
	if idx < 0 {
		return "", false
	}
	return s[:idx], true
}
