package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSkill(t *testing.T, root, name, content string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
}

func TestRegistryLoadAndList(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "code-review", `---
name: code-review
description: Review Go code.
allowed-tools: read_file, git_diff
---
# Body
Do the review.
`)
	writeSkill(t, root, "pdf", `---
name: pdf
description: Fill PDF forms.
---
Body here.
`)
	// A directory without SKILL.md must be ignored, not fatal.
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}

	r := NewRegistry(root)
	if err := r.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}

	list := r.List()
	if len(list) != 2 {
		t.Fatalf("want 2 skills, got %d", len(list))
	}
	// Deterministic, name-sorted ordering.
	if list[0].Name != "code-review" || list[1].Name != "pdf" {
		t.Fatalf("unexpected order: %s, %s", list[0].Name, list[1].Name)
	}
	if got := list[0].AllowedTools; len(got) != 2 || got[0] != "read_file" || got[1] != "git_diff" {
		t.Fatalf("allowed-tools parse failed: %v", got)
	}
}

func TestRegistryBodyStripsFrontmatter(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "code-review", `---
name: code-review
description: Review Go code.
---
# Code Review
Step one.
`)
	// Add a resource file alongside SKILL.md.
	if err := os.WriteFile(filepath.Join(root, "code-review", "checklist.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := NewRegistry(root)
	if err := r.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}

	body, resources, err := r.Body("code-review")
	if err != nil {
		t.Fatalf("Body: %v", err)
	}
	if strings.Contains(body, "name: code-review") {
		t.Fatalf("frontmatter leaked into body:\n%s", body)
	}
	if !strings.Contains(body, "# Code Review") {
		t.Fatalf("body missing heading:\n%s", body)
	}
	// checklist.txt should be enumerated; SKILL.md should not.
	var sawChecklist, sawSkillMd bool
	for _, res := range resources {
		if res == "checklist.txt" {
			sawChecklist = true
		}
		if res == "SKILL.md" {
			sawSkillMd = true
		}
	}
	if !sawChecklist {
		t.Fatalf("checklist.txt not listed as resource: %v", resources)
	}
	if sawSkillMd {
		t.Fatalf("SKILL.md must not be listed as a resource: %v", resources)
	}
}

func TestBodyUnknownSkill(t *testing.T) {
	r := NewRegistry(t.TempDir())
	if err := r.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, _, err := r.Body("nope"); err == nil {
		t.Fatal("expected error for unknown skill")
	}
}

func TestMissingRootIsNotFatal(t *testing.T) {
	r := NewRegistry(filepath.Join(t.TempDir(), "does-not-exist"))
	if err := r.Load(); err != nil {
		t.Fatalf("missing root should not error, got: %v", err)
	}
	if len(r.List()) != 0 {
		t.Fatal("expected empty registry")
	}
}

func TestPromptSection(t *testing.T) {
	if s := PromptSection(nil); s != "" {
		t.Fatalf("nil registry must yield empty section, got %q", s)
	}
	root := t.TempDir()
	writeSkill(t, root, "pdf", "---\nname: pdf\ndescription: Fill PDFs.\n---\nbody")
	r := NewRegistry(root)
	if err := r.Load(); err != nil {
		t.Fatal(err)
	}
	s := PromptSection(r)
	if !strings.Contains(s, "1. pdf: Fill PDFs.") {
		t.Fatalf("prompt section missing skill line:\n%s", s)
	}
}
