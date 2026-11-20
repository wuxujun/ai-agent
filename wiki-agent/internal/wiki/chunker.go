package wiki

import "strings"

// ParagraphChunker splits on blank lines and packs paragraphs into chunks of
// at most MaxChars characters. Oversized paragraphs are hard-split.
type ParagraphChunker struct {
	MaxChars int
}

// Split implements Chunker.
func (c ParagraphChunker) Split(content string) []string {
	max := c.MaxChars
	if max <= 0 {
		max = 2000
	}
	var chunks []string
	var cur strings.Builder
	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			chunks = append(chunks, s)
		}
		cur.Reset()
	}
	for _, p := range strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n\n") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		for len(p) > max {
			flush()
			chunks = append(chunks, p[:max])
			p = p[max:]
		}
		if cur.Len() > 0 && cur.Len()+2+len(p) > max {
			flush()
		}
		if cur.Len() > 0 {
			cur.WriteString("\n\n")
		}
		cur.WriteString(p)
	}
	flush()
	return chunks
}
