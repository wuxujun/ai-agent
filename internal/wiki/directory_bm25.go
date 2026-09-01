package wiki

import (
	"container/heap"
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode"
)

const directoryBM25FieldCount = 5

var directoryBM25FieldWeights = [directoryBM25FieldCount]float64{8, 6, 4, 3, 1}

type directoryBM25Posting struct {
	termFrequency [directoryBM25FieldCount]uint16
}

type directoryBM25Index struct {
	postings       map[string]map[int]directoryBM25Posting
	documentLength [][directoryBM25FieldCount]int
	averageLength  [directoryBM25FieldCount]float64
	documentBySlug map[string]int
}

type directoryBM25Score struct {
	score         float64
	matchedTerms  []string
	fieldScores   map[string]float64
	phraseMatches []string
	linkBoost     float64
	lexicalScore  float64
	matchCount    int
	active        bool
	qualified     bool
}

type directoryBM25Rank struct {
	documentID int
	score      float64
	slug       string
}

// directoryBM25TopK is a min-heap whose root is the worst retained result.
type directoryBM25TopK []directoryBM25Rank

func (ranks directoryBM25TopK) Len() int { return len(ranks) }
func (ranks directoryBM25TopK) Less(left, right int) bool {
	if ranks[left].score == ranks[right].score {
		return ranks[left].slug > ranks[right].slug
	}
	return ranks[left].score < ranks[right].score
}
func (ranks directoryBM25TopK) Swap(left, right int) {
	ranks[left], ranks[right] = ranks[right], ranks[left]
}
func (ranks *directoryBM25TopK) Push(value any) { *ranks = append(*ranks, value.(directoryBM25Rank)) }
func (ranks *directoryBM25TopK) Pop() any {
	old := *ranks
	last := old[len(old)-1]
	*ranks = old[:len(old)-1]
	return last
}

var directoryBM25FieldNames = [directoryBM25FieldCount]string{"title", "slug", "heading", "metadata", "body"}

func buildDirectoryBM25Index(pages []directoryIndexPage) *directoryBM25Index {
	index := &directoryBM25Index{
		postings:       make(map[string]map[int]directoryBM25Posting),
		documentLength: make([][directoryBM25FieldCount]int, len(pages)),
		documentBySlug: make(map[string]int, len(pages)),
	}
	for documentID, page := range pages {
		index.documentBySlug[page.slug] = documentID
		fields := [directoryBM25FieldCount]string{page.titleText, page.slugText, page.headingText, page.metadataText, page.bodyText}
		for field, text := range fields {
			tokens := directoryBM25Tokens(text)
			index.documentLength[documentID][field] = len(tokens)
			index.averageLength[field] += float64(len(tokens))
			frequencies := make(map[string]uint16)
			for _, token := range tokens {
				if frequencies[token] < ^uint16(0) {
					frequencies[token]++
				}
			}
			for token, frequency := range frequencies {
				byDocument := index.postings[token]
				if byDocument == nil {
					byDocument = make(map[int]directoryBM25Posting)
					index.postings[token] = byDocument
				}
				posting := byDocument[documentID]
				posting.termFrequency[field] = frequency
				byDocument[documentID] = posting
			}
		}
	}
	if len(pages) > 0 {
		for field := range index.averageLength {
			index.averageLength[field] /= float64(len(pages))
		}
	}
	return index
}

func directoryBM25Tokens(value string) []string {
	value = strings.ToLower(value)
	tokens := make([]string, 0, len(value)/3)
	var word []rune
	var han []rune
	flushWord := func() {
		if len(word) > 0 {
			tokens = append(tokens, string(word))
			word = word[:0]
		}
	}
	flushHan := func() {
		if len(han) == 1 {
			tokens = append(tokens, string(han))
		} else {
			for index := 0; index+1 < len(han); index++ {
				tokens = append(tokens, string(han[index:index+2]))
			}
		}
		han = han[:0]
	}
	for _, current := range value {
		switch {
		case unicode.Is(unicode.Han, current):
			flushWord()
			han = append(han, current)
		case unicode.IsLetter(current) || unicode.IsNumber(current):
			flushHan()
			word = append(word, current)
		default:
			flushWord()
			flushHan()
		}
	}
	flushWord()
	flushHan()
	return tokens
}

func uniqueDirectoryBM25Tokens(value string) []string {
	seen := make(map[string]bool)
	var tokens []string
	for _, token := range directoryBM25Tokens(value) {
		if token != "" && !seen[token] {
			seen[token] = true
			tokens = append(tokens, token)
		}
	}
	return tokens
}

func (c *DirectoryClient) searchBM25(query string, _ []string, phrase, compactPhrase string, topK int, space string, pages []directoryIndexPage, index *directoryBM25Index, indexVersion string, explain bool) ([]Document, []SearchExplanation, error) {
	if index == nil {
		return nil, nil, fmt.Errorf("wiki BM25 index is unavailable")
	}
	queryTokens := uniqueDirectoryBM25Tokens(query)
	if len(queryTokens) == 0 {
		return nil, nil, fmt.Errorf("wiki search query has no indexable terms")
	}
	scores := make([]directoryBM25Score, len(pages))
	activeDocuments := make([]int, 0, min(len(pages), 256))
	for _, token := range queryTokens {
		postings := index.postings[token]
		if len(postings) == 0 {
			continue
		}
		idf := math.Log(1 + (float64(len(pages)-len(postings))+0.5)/(float64(len(postings))+0.5))
		for documentID, posting := range postings {
			detail := &scores[documentID]
			if !detail.active {
				detail.active = true
				activeDocuments = append(activeDocuments, documentID)
				if explain {
					detail.fieldScores = make(map[string]float64)
				}
			}
			matched := false
			for field, frequency := range posting.termFrequency {
				if frequency == 0 {
					continue
				}
				matched = true
				length := float64(index.documentLength[documentID][field])
				average := index.averageLength[field]
				if average <= 0 {
					average = 1
				}
				const k1, b = 1.2, 0.75
				tf := float64(frequency)
				fieldScore := directoryBM25FieldWeights[field] * idf * (tf * (k1 + 1)) / (tf + k1*(1-b+b*length/average))
				detail.score += fieldScore
				if explain {
					detail.fieldScores[directoryBM25FieldNames[field]] += fieldScore
				}
			}
			if matched {
				detail.matchCount++
				if explain {
					detail.matchedTerms = append(detail.matchedTerms, token)
				}
			}
		}
	}
	minimumMatches := directoryBM25MinimumMatches(query, len(queryTokens))
	qualifiedDocuments := activeDocuments[:0]
	for _, documentID := range activeDocuments {
		detail := &scores[documentID]
		if detail.matchCount < minimumMatches {
			continue
		}
		detail.qualified = true
		applyDirectoryBM25PhraseBoost(pages[documentID], phrase, compactPhrase, detail, explain)
		detail.lexicalScore = detail.score
		qualifiedDocuments = append(qualifiedDocuments, documentID)
	}
	for _, documentID := range qualifiedDocuments {
		page := pages[documentID]
		detail := &scores[documentID]
		if isDirectoryNavigationPage(page.slug) {
			continue
		}
		boost := math.Min(detail.lexicalScore*0.08, 2.5)
		for _, linkedSlug := range page.links {
			linkedID, exists := index.documentBySlug[linkedSlug]
			linked := &scores[linkedID]
			if exists && linked.qualified {
				linked.score += boost
				linked.linkBoost += boost
			}
		}
	}
	ranks := make(directoryBM25TopK, 0, min(topK, len(qualifiedDocuments)))
	for _, documentID := range qualifiedDocuments {
		detail := &scores[documentID]
		page := pages[documentID]
		score := detail.score * directoryBM25NavigationMultiplier(page) * directoryBM25SourceMultiplier(page, phrase, compactPhrase)
		rank := directoryBM25Rank{documentID: documentID, score: score, slug: page.slug}
		if len(ranks) < topK {
			heap.Push(&ranks, rank)
		} else if directoryBM25RankBetter(rank, ranks[0]) {
			ranks[0] = rank
			heap.Fix(&ranks, 0)
		}
	}
	sort.Slice(ranks, func(left, right int) bool { return directoryBM25RankBetter(ranks[left], ranks[right]) })
	searchTerms := directorySearchTerms(query)
	documents := make([]Document, 0, len(ranks))
	for _, rank := range ranks {
		page := pages[rank.documentID]
		documents = append(documents, Document{
			Slug: page.slug, URI: "wiki://" + space + "/" + page.slug, Title: page.title,
			Summary: page.summary, Excerpt: bestDirectoryExcerpt(page, searchTerms, phrase, compactPhrase), Score: rank.score,
		})
	}
	if !explain {
		return documents, nil, nil
	}
	explanations := make([]SearchExplanation, 0, len(documents))
	for _, document := range documents {
		documentID := index.documentBySlug[document.Slug]
		detail := &scores[documentID]
		page := pages[documentID]
		navigationMultiplier := directoryBM25NavigationMultiplier(page)
		sourceMultiplier := directoryBM25SourceMultiplier(page, phrase, compactPhrase)
		explanations = append(explanations, SearchExplanation{
			URI: document.URI, Slug: document.Slug, IndexVersion: indexVersion,
			BaseScore: detail.score - detail.linkBoost, LinkBoost: detail.linkBoost,
			NavigationMultiplier: navigationMultiplier, SourceMultiplier: sourceMultiplier, FinalScore: document.Score,
			FieldScores: detail.fieldScores, PhraseMatches: detail.phraseMatches,
			MatchedTerms: detail.matchedTerms,
		})
	}
	return documents, explanations, nil
}

func directoryBM25NavigationMultiplier(page directoryIndexPage) float64 {
	if isDirectoryNavigationPage(page.slug) {
		return 0.25
	}
	return 1
}

func directoryBM25SourceMultiplier(page directoryIndexPage, phrase, compactPhrase string) float64 {
	if !strings.HasPrefix(page.slug, "sources/raw-doc-") {
		return 1
	}
	if phrase != "" && strings.Contains(page.titleText, phrase) {
		return 1
	}
	if compactPhrase != "" && strings.Contains(page.titleCompact, compactPhrase) {
		return 1
	}
	// Generic raw-document source notes are useful recall candidates but are
	// less entity-specific than compiled concept/entity/course-source pages.
	return 0.2
}

func directoryBM25RankBetter(left, right directoryBM25Rank) bool {
	if left.score == right.score {
		return left.slug < right.slug
	}
	return left.score > right.score
}

func directoryBM25MinimumMatches(query string, tokens int) int {
	if tokens <= 2 {
		return tokens
	}
	minimum := int(math.Ceil(float64(tokens) * 0.6))
	for _, current := range query {
		if unicode.Is(unicode.Han, current) {
			return min(minimum, 2)
		}
	}
	return minimum
}

func applyDirectoryBM25PhraseBoost(page directoryIndexPage, phrase, compactPhrase string, detail *directoryBM25Score, explain bool) {
	add := func(name, field string, score float64) {
		if explain {
			detail.phraseMatches = append(detail.phraseMatches, name)
			detail.fieldScores[field] += score
		}
		detail.score += score
	}
	if phrase != "" {
		if strings.Contains(page.titleText, phrase) {
			add("title", "title", 12)
		}
		if strings.Contains(page.slugText, phrase) {
			add("slug", "slug", 8)
		}
		if strings.Contains(page.headingText, phrase) {
			add("heading", "heading", 5)
		}
		if strings.Contains(page.metadataText, phrase) {
			add("metadata", "metadata", 4)
		}
		if strings.Contains(page.bodyText, phrase) {
			add("body", "body", 2)
		}
	}
	if compactPhrase != "" {
		if strings.Contains(page.titleCompact, compactPhrase) {
			add("compact_title", "title", 6)
		}
		if strings.Contains(page.slugCompact, compactPhrase) {
			add("compact_slug", "slug", 4)
		}
		if strings.Contains(page.metadataCompact, compactPhrase) {
			add("compact_metadata", "metadata", 2)
		}
		if strings.Contains(page.bodyCompact, compactPhrase) {
			add("compact_body", "body", 1)
		}
	}
}
