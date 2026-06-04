// Package skills implements a lightweight "Agent Skills" capability layer for
// the planner. A skill is a directory containing a SKILL.md file whose YAML
// frontmatter declares the skill's name and description; the Markdown body
// holds the full, on-demand instructions.
//
// Progressive disclosure: only the frontmatter (name + description) is loaded
// into memory at startup and injected into the planner prompt. The full body is
// read on demand when the planner invokes the use_skill tool. This keeps the
// per-turn prompt small no matter how many skills are installed, mirroring the
// design of Claude Agent Skills.
//
// The Registry mirrors the style of tools.Registry (sorted, thread-safe,
// deterministic ordering) so the generated prompt stays stable across runs.
package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Skill holds the metadata parsed from a SKILL.md frontmatter. The body is NOT
// retained in memory; it is read on demand via Registry.Body.
type Skill struct {
	Name         string   // frontmatter "name"; the unique key and use_skill argument
	Description  string   // frontmatter "description"; one-line summary injected into the prompt
	AllowedTools []string // optional frontmatter "allowed-tools"; advisory tool whitelist
	Dir          string   // absolute path to the skill directory
	bodyPath     string   // absolute path to SKILL.md (body read lazily)
}

// Registry is a thread-safe, name-keyed collection of skills discovered under a
// root directory.
type Registry struct {
	root  string
	mu    sync.RWMutex
	items map[string]Skill
}

// NewRegistry creates an empty registry rooted at the given directory. Call
// Load to scan the directory.
func NewRegistry(root string) *Registry {
	return &Registry{
		root:  root,
		items: make(map[string]Skill),
	}
}

// Load scans root/*/SKILL.md, parsing each frontmatter into a Skill. A missing
// root directory is not an error (skills are optional); it yields an empty
// registry. Individual malformed skills are skipped, not fatal, so one bad
// skill cannot disable the whole layer.
func (r *Registry) Load() error {
	entries, err := os.ReadDir(r.root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // skills are optional
		}
		return fmt.Errorf("skills: read root %q: %w", r.root, err)
	}

	loaded := make(map[string]Skill)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(r.root, e.Name())
		mdPath := filepath.Join(dir, "SKILL.md")
		raw, err := os.ReadFile(mdPath)
		if err != nil {
			continue // no SKILL.md in this dir; skip silently
		}
		meta, ok := parseFrontmatter(string(raw))
		if !ok || strings.TrimSpace(meta.Name) == "" {
			continue // malformed or nameless; skip
		}
		loaded[meta.Name] = Skill{
			Name:         meta.Name,
			Description:  meta.Description,
			AllowedTools: meta.AllowedTools,
			Dir:          dir,
			bodyPath:     mdPath,
		}
	}

	r.mu.Lock()
	r.items = loaded
	r.mu.Unlock()
	return nil
}

// List returns all skills ordered by name. Deterministic ordering keeps the
// generated prompt section stable across runs (same rationale as
// tools.Registry.List).
func (r *Registry) List() []Skill {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Skill, 0, len(r.items))
	for _, s := range r.items {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get returns the skill with the given name.
func (r *Registry) Get(name string) (Skill, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.items[name]
	return s, ok
}

// Body reads the SKILL.md body (everything after the frontmatter) on demand and
// returns it together with the list of sibling resource files (paths relative
// to the skill directory, excluding SKILL.md itself). This is the
// progressive-disclosure step: full instructions are only paid for when the
// planner actually loads the skill.
func (r *Registry) Body(name string) (body string, resources []string, err error) {
	s, ok := r.Get(name)
	if !ok {
		return "", nil, fmt.Errorf("skills: unknown skill %q", name)
	}

	raw, err := os.ReadFile(s.bodyPath)
	if err != nil {
		return "", nil, fmt.Errorf("skills: read body %q: %w", s.bodyPath, err)
	}
	body = stripFrontmatter(string(raw))

	// Enumerate resource files alongside SKILL.md (one level; recurse if needed).
	_ = filepath.WalkDir(s.Dir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return nil
		}
		if path == s.bodyPath {
			return nil
		}
		rel, relErr := filepath.Rel(s.Dir, path)
		if relErr == nil {
			resources = append(resources, rel)
		}
		return nil
	})
	sort.Strings(resources)
	return body, resources, nil
}

// PromptSection renders the installed skills as a prompt fragment for the
// planner. Returns an empty string when no skills are installed so callers can
// append unconditionally. Shared by the single-agent planner and the
// multiagent PlannerAgent to keep both prompt paths in sync.
func PromptSection(r *Registry) string {
	if r == nil {
		return ""
	}
	list := r.List()
	if len(list) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nAvailable skills (call use_skill{name} to load full instructions before a specialized task):\n")
	for i, s := range list {
		fmt.Fprintf(&b, "%d. %s: %s\n", i+1, s.Name, s.Description)
	}
	return strings.TrimRight(b.String(), "\n")
}
