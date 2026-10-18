package types

type ConflictSource struct {
	ID      string `json:"source_id"`
	Origin  string `json:"origin"`
	Content string `json:"content"`
}

type EvidenceConflict struct {
	SourceIDs   []string `json:"source_ids"`
	Severity    string   `json:"severity"`
	Topic       string   `json:"topic"`
	Explanation string   `json:"explanation"`
}

const (
	TraceActionEvidenceConflict   = "evidence_conflict_resolve"
	TraceActionSourceCredibility  = "source_credibility_score"
	TraceActionFactFreshness      = "fact_freshness_check"
	TraceActionNumericConsistency = "numeric_consistency_check"
	TraceActionAnswerUncertainty  = "answer_uncertainty_calibrate"
)

func IsExternalAction(action string) bool {
	switch action {
	case "http_fetch", "web_search", "rag_search", "rag_fetch", "memory_search", "memory_get", "wiki_search", "wiki_fetch", "wiki_graph", "wiki_graph_fetch", "wiki_suggest":
		return true
	default:
		return false
	}
}

const QuarantinedEvidenceMessage = "[QUARANTINED: potential prompt injection in external content]"
