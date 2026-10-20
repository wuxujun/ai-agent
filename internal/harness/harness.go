package harness

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/wuxujun/ai-agent/internal/multiagent"
)

// Snapshot is a complete, reproducible harness at a point in time.
// ID is the sha256 of the canonical JSON encoding of Teams + SkillDigests,
// so identical harnesses always share the same ID.
type Snapshot struct {
	ID           string                 `json:"id"`
	CreatedAt    time.Time              `json:"created_at"`
	Source       string                 `json:"source"` // "teams.yaml" | "applied:<author>" | ...
	Teams        multiagent.TeamsConfig `json:"teams"`
	SkillDigests map[string]string      `json:"skill_digests"` // skill name -> sha256(SKILL.md)
}

// Edit is one atomic, reviewable harness change.
type Edit struct {
	Target string `json:"target"` // see target grammar in doc.go
	Op     string `json:"op"`     // "set" | "add-tools" | "remove-tools"
	Before string `json:"before"` // canonical old value ("" when unknown)
	After  string `json:"after"`  // canonical new value
	Reason string `json:"reason"` // why (filled by the editor / human)
}

// EditSet is a batch of edits against one base snapshot.
type EditSet struct {
	SnapshotID string `json:"snapshot_id"` // base snapshot ID the edits were made against
	Edits      []Edit `json:"edits"`
	Author     string `json:"author"` // "editor:<run-id>" | human name
}

// canonicalJSON returns a deterministic JSON encoding: struct fields keep
// declaration order and encoding/json sorts map keys, so the digest is stable
// across processes for identical input.
func canonicalJSON(v any) ([]byte, error) {
	return json.Marshal(v)
}

// digestSnapshot computes the content ID over the harness-relevant parts.
func digestSnapshot(teams multiagent.TeamsConfig, skillDigests map[string]string) (string, error) {
	// Normalize nil map to empty for digest stability.
	if skillDigests == nil {
		skillDigests = map[string]string{}
	}
	payload := struct {
		Teams  multiagent.TeamsConfig `json:"teams"`
		Skills map[string]string      `json:"skill_digests"`
	}{Teams: teams, Skills: skillDigests}
	data, err := canonicalJSON(payload)
	if err != nil {
		return "", fmt.Errorf("canonicalize harness: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// sortedKeys returns map keys in ascending order for deterministic iteration.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
