package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	llmcore "github.com/wuxujun/ai-agent/internal/llm"
	"github.com/wuxujun/ai-agent/internal/types"
)

type CitationVerification struct {
	Supported         bool
	VerifiedAnswer    string
	UnsupportedClaims []string
	CitationIssues    []string
}

type CitationVerifier interface {
	Verify(ctx context.Context, task *types.Task, answer string) (*CitationVerification, types.TokenUsage, error)
}

type LLMCitationVerifier struct {
	Scene string
}

type citationEvidence struct {
	ID      string `json:"id"`
	Source  string `json:"source"`
	Query   string `json:"query,omitempty"`
	Content string `json:"content"`
}

var citationIDPattern = regexp.MustCompile(`\[(?:E|M)[1-9][0-9]*\]`)

func NewLLMCitationVerifier(scene string) *LLMCitationVerifier {
	return &LLMCitationVerifier{Scene: scene}
}

func HasCitationEvidence(task *types.Task) bool {
	return len(buildCitationEvidence(task)) > 0
}

func (v *LLMCitationVerifier) Verify(ctx context.Context, task *types.Task, answer string) (*CitationVerification, types.TokenUsage, error) {
	evidence := buildCitationEvidence(task)
	if len(evidence) == 0 {
		return nil, types.TokenUsage{}, fmt.Errorf("citation verification requires evidence")
	}
	evidenceJSON, err := json.Marshal(evidence)
	if err != nil {
		return nil, types.TokenUsage{}, fmt.Errorf("encode citation evidence: %w", err)
	}

	var output struct {
		Supported         bool     `json:"supported"`
		VerifiedAnswer    string   `json:"verified_answer"`
		UnsupportedClaims []string `json:"unsupported_claims"`
		CitationIssues    []string `json:"citation_issues"`
	}
	schema := map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"supported":          map[string]any{"type": "boolean"},
			"verified_answer":    map[string]any{"type": "string"},
			"unsupported_claims": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": 50},
			"citation_issues":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": 50},
		},
		"required": []string{"supported", "verified_answer", "unsupported_claims", "citation_issues"},
	}
	prompt := fmt.Sprintf("Goal: %s\n\nAnswer to verify:\n%s\n\nAllowed evidence catalog (JSON):\n%s", task.Goal, answer, evidenceJSON)
	usage, err := llmcore.CallJSON(ctx, llmcore.ConfigForScene(v.Scene), `Verify every externally checkable claim in the answer against the supplied evidence catalog. Return a self-contained corrected answer. Cite supported claims with the exact catalog IDs such as [E1] or [M1]. Never invent an ID. Remove or explicitly qualify unsupported claims. Preserve useful non-factual guidance. Return JSON only.`, truncateRunes(prompt, 64000), schema, &output)
	if err != nil {
		return nil, types.TokenUsage{}, err
	}
	output.VerifiedAnswer = strings.TrimSpace(output.VerifiedAnswer)
	if output.VerifiedAnswer == "" {
		return nil, usage, fmt.Errorf("citation verifier returned an empty answer")
	}
	allowed := make(map[string]struct{}, len(evidence))
	for _, item := range evidence {
		allowed["["+item.ID+"]"] = struct{}{}
	}
	answerCitationIDs := citationIDPattern.FindAllString(output.VerifiedAnswer, -1)
	if output.Supported && len(answerCitationIDs) == 0 {
		return nil, usage, fmt.Errorf("citation verifier marked the answer supported without citing evidence")
	}
	for _, id := range answerCitationIDs {
		if _, ok := allowed[id]; !ok {
			return nil, usage, fmt.Errorf("citation verifier returned unknown evidence ID %s", id)
		}
	}
	return &CitationVerification{
		Supported:         output.Supported,
		VerifiedAnswer:    output.VerifiedAnswer,
		UnsupportedClaims: output.UnsupportedClaims,
		CitationIssues:    output.CitationIssues,
	}, usage, nil
}

func buildCitationEvidence(task *types.Task) []citationEvidence {
	var result []citationEvidence
	for _, trace := range task.Trace {
		for _, evidence := range trace.Evidence {
			content := strings.TrimSpace(strings.Join(evidence.Lines, "\n"))
			if content == "" {
				continue
			}
			result = append(result, citationEvidence{
				ID:      fmt.Sprintf("E%d", len(result)+1),
				Source:  strings.TrimSpace(evidence.Path),
				Query:   strings.TrimSpace(evidence.Query),
				Content: truncateRunes(content, 8000),
			})
		}
	}
	evidenceCount := len(result)
	for _, memory := range task.Memories {
		content := strings.TrimSpace(strings.Join([]string{memory.Goal, memory.KeyFindings, memory.FinalAnswer}, "\n"))
		if content == "" {
			continue
		}
		result = append(result, citationEvidence{
			ID:      fmt.Sprintf("M%d", len(result)-evidenceCount+1),
			Source:  "memory:" + memory.ID,
			Content: truncateRunes(content, 8000),
		})
	}
	return result
}
