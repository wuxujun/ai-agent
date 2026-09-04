package brain

import (
	"time"

	"github.com/wuxujun/ai-agent/internal/types"
)

// EvidenceRecord is the normalized, bounded provenance for one successful
// external trace. URI is the canonical immutable evidence address and ID is a
// SHA-256 digest of that URI.
type EvidenceRecord struct {
	ID          string    `json:"id"`
	URI         string    `json:"uri"`
	TaskID      string    `json:"task_id"`
	TraceStep   string    `json:"trace_step"`
	Content     string    `json:"content"`
	ContentHash string    `json:"content_hash"`
	ObservedAt  time.Time `json:"observed_at"`
}

type Claim struct {
	ID          string    `json:"id"`
	Text        string    `json:"text"`
	Confidence  string    `json:"confidence"`
	State       string    `json:"state"`
	EvidenceIDs []string  `json:"evidence_ids"`
	ObservedAt  time.Time `json:"observed_at"`
}

type Page struct {
	Kind    string   `json:"kind"`
	Slug    string   `json:"slug"`
	Title   string   `json:"title"`
	Summary string   `json:"summary"`
	Claims  []Claim  `json:"claims"`
	Links   []string `json:"links"`
}

type Synthesis struct {
	Pages []Page `json:"pages"`
}

// SourceSet is an immutable-by-convention source snapshot. DiscoveryHints are
// sanitized topic leads only; they never establish evidence for a Claim.
type SourceSet struct {
	Evidence       []EvidenceRecord `json:"evidence"`
	SourceIDs      []string         `json:"source_ids"`
	SourceHashes   []string         `json:"source_hashes"`
	DiscoveryHints []string         `json:"discovery_hints"`
	Cutoff         time.Time        `json:"cutoff"`
}

type ValidationFinding struct {
	Code    string `json:"code"`
	Path    string `json:"path"`
	Message string `json:"message"`
	Hard    bool   `json:"hard"`
}

type ValidationReport struct {
	Publishable bool                `json:"publishable"`
	Findings    []ValidationFinding `json:"findings"`
}

type Manifest struct {
	SnapshotID          string            `json:"snapshot_id"`
	ParentID            string            `json:"parent_id"`
	TenantID            string            `json:"tenant_id"`
	ProjectID           string            `json:"project_id"`
	ExpectedCurrent     string            `json:"expected_current"`
	SourceCutoff        time.Time         `json:"source_cutoff"`
	SourceIDs           []string          `json:"source_ids"`
	SourceHashes        []string          `json:"source_hashes"`
	RetractionWatermark string            `json:"retraction_watermark"`
	Model               string            `json:"model"`
	PromptVersion       string            `json:"prompt_version"`
	ConfigDigest        string            `json:"config_digest"`
	Usage               types.TokenUsage  `json:"usage"`
	EstimatedCostUSD    float64           `json:"estimated_cost_usd"`
	FileHashes          map[string]string `json:"file_hashes"`
	Validation          ValidationReport  `json:"validation"`
}

type SnapshotDraft struct {
	Files    map[string][]byte `json:"files"`
	Evidence []EvidenceRecord  `json:"evidence"`
	Manifest Manifest          `json:"manifest"`
}

type Release struct {
	Root     string   `json:"root"`
	Manifest Manifest `json:"manifest"`
}

type Retraction struct {
	EvidenceURI string    `json:"evidence_uri"`
	Reason      string    `json:"reason"`
	RetractedAt time.Time `json:"retracted_at"`
}
