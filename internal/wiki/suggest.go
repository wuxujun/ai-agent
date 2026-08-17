package wiki

import (
	"errors"
	"sort"
	"strings"
)

const suggestToolName = "wiki_suggest"

const maxSuggestions = 10

type Suggestion struct {
	Kind   string  `json:"kind"`
	URI    string  `json:"uri"`
	Title  string  `json:"title,omitempty"`
	Reason string  `json:"reason"`
	Score  float64 `json:"score,omitempty"`
}

type SuggestResult struct {
	RootURI     string       `json:"root_uri"`
	Suggestions []Suggestion `json:"suggestions"`
}

func validateSuggestRequest(document Document, space string, limit int) (string, string, int, error) {
	slug, _, err := validateGraphRequest(document, space, 1, "both")
	if err != nil {
		return "", "", 0, err
	}
	space = strings.Trim(strings.TrimSpace(space), "/")
	if space == "" {
		space = strings.TrimSpace(strings.Split(strings.TrimPrefix(document.URI, "wiki://"), "/")[0])
	}
	if limit == 0 {
		limit = 5
	}
	if limit < 1 || limit > maxSuggestions {
		return "", "", 0, errors.New("wiki suggest limit must be between 1 and 10")
	}
	return slug, space, limit, nil
}

func normalizeSuggestResult(result SuggestResult, limit int) SuggestResult {
	seen := make(map[string]bool)
	filtered := make([]Suggestion, 0, len(result.Suggestions))
	for _, item := range result.Suggestions {
		item.Kind = strings.ToLower(strings.TrimSpace(item.Kind))
		item.URI = strings.TrimSpace(item.URI)
		item.Reason = strings.TrimSpace(item.Reason)
		if item.URI == "" || item.URI == result.RootURI || seen[item.Kind+"\x00"+item.URI] {
			continue
		}
		switch item.Kind {
		case "related", "missing_link", "possible_duplicate":
		default:
			continue
		}
		seen[item.Kind+"\x00"+item.URI] = true
		filtered = append(filtered, item)
	}
	priority := map[string]int{"related": 0, "possible_duplicate": 1, "missing_link": 2}
	sort.SliceStable(filtered, func(i, j int) bool {
		if priority[filtered[i].Kind] != priority[filtered[j].Kind] {
			return priority[filtered[i].Kind] < priority[filtered[j].Kind]
		}
		if filtered[i].Score != filtered[j].Score {
			return filtered[i].Score > filtered[j].Score
		}
		return filtered[i].URI < filtered[j].URI
	})
	if len(filtered) > limit {
		filtered = filtered[:limit]
	}
	result.Suggestions = filtered
	return result
}

func titleSimilarity(left, right string) float64 {
	leftTerms := strings.Fields(normalizeDirectorySearchText(left))
	rightTerms := strings.Fields(normalizeDirectorySearchText(right))
	if len(leftTerms) == 0 || len(rightTerms) == 0 {
		return 0
	}
	leftSet := make(map[string]bool, len(leftTerms))
	union := make(map[string]bool, len(leftTerms)+len(rightTerms))
	for _, term := range leftTerms {
		leftSet[term], union[term] = true, true
	}
	intersection := 0
	for _, term := range rightTerms {
		if !union[term] {
			union[term] = true
		}
		if leftSet[term] {
			intersection++
			delete(leftSet, term)
		}
	}
	return float64(intersection) / float64(len(union))
}
