package harness

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/wuxujun/ai-agent/internal/multiagent"
	"github.com/wuxujun/ai-agent/internal/skills"
	"gopkg.in/yaml.v3"
)

// Capture snapshots the harness defined by teamsYAMLPath and the skills
// discovered under skillsRoot. A missing skillsRoot is not an error: skills
// are optional and then SkillDigests is simply empty.
func Capture(teamsYAMLPath, skillsRoot string) (Snapshot, error) {
	data, err := os.ReadFile(teamsYAMLPath)
	if err != nil {
		return Snapshot{}, fmt.Errorf("read teams config %s: %w", teamsYAMLPath, err)
	}
	var teams multiagent.TeamsConfig
	if err := yaml.Unmarshal(data, &teams); err != nil {
		return Snapshot{}, fmt.Errorf("parse teams config %s: %w", teamsYAMLPath, err)
	}
	if len(teams.Teams) == 0 {
		return Snapshot{}, fmt.Errorf("%s defines no teams", teamsYAMLPath)
	}

	digests, err := SkillDigests(skillsRoot)
	if err != nil {
		return Snapshot{}, err
	}
	return NewSnapshot(teams, digests, "teams.yaml:"+teamsYAMLPath)
}

// NewSnapshot builds a snapshot from an in-memory teams config. It is the
// programmatic counterpart of Capture, used when the config was already
// resolved (e.g. the online refinement overlay).
func NewSnapshot(teams multiagent.TeamsConfig, skillDigests map[string]string, source string) (Snapshot, error) {
	if len(teams.Teams) == 0 {
		return Snapshot{}, fmt.Errorf("snapshot requires at least one team")
	}
	id, err := digestSnapshot(teams, skillDigests)
	if err != nil {
		return Snapshot{}, err
	}
	if skillDigests == nil {
		skillDigests = map[string]string{}
	}
	return Snapshot{
		ID:           id,
		CreatedAt:    time.Now().UTC(),
		Source:       source,
		Teams:        teams,
		SkillDigests: skillDigests,
	}, nil
}

// SkillDigests hashes every discovered skill's SKILL.md. The registry's
// bodyPath is unexported, but Load documents the layout root/*/SKILL.md,
// so the digest reads that path directly. A missing root is not an error.
func SkillDigests(root string) (map[string]string, error) {
	out := map[string]string{}
	if root == "" {
		return out, nil
	}
	reg := skills.NewRegistry(root)
	if err := reg.Load(); err != nil {
		return nil, fmt.Errorf("load skills from %s: %w", root, err)
	}
	for _, sk := range reg.List() {
		body, err := os.ReadFile(filepath.Join(sk.Dir, "SKILL.md"))
		if err != nil {
			return nil, fmt.Errorf("read skill %q body: %w", sk.Name, err)
		}
		sum := sha256.Sum256(body)
		out[sk.Name] = hex.EncodeToString(sum[:])
	}
	return out, nil
}
