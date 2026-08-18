package multiagent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpdateTeamLifecycleUsesOptimisticAtomicWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "teams.yaml")
	content := "# lifecycle fixture\nactive_team: default\nteams:\n  default:\n    lifecycle: active\n    workflow: planner_researcher_writer\n  candidate:\n    lifecycle: active\n    workflow: planner_researcher_writer\n"
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
	revision := teamsFileRevision([]byte(content))
	change, err := updateTeamLifecycleFile(path, "candidate", TeamLifecycleRetired, revision, func(string) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	if !change.Changed || change.Previous != TeamLifecycleActive || change.Current != TeamLifecycleRetired || change.Revision == revision {
		t.Fatalf("change = %+v", change)
	}
	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(updated), "# lifecycle fixture") || !strings.Contains(string(updated), "lifecycle: \"retired\"") {
		t.Fatalf("updated configuration = %s", updated)
	}
	want := strings.Replace(content, "  candidate:\n    lifecycle: active", "  candidate:\n    lifecycle: \"retired\"", 1)
	if string(updated) != want {
		t.Fatalf("lifecycle update changed unrelated bytes:\n%s", updated)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("file mode = %v err=%v", info.Mode().Perm(), err)
	}
	if _, err := updateTeamLifecycleFile(path, "candidate", TeamLifecycleActive, revision, func(string) bool { return false }); !errors.Is(err, ErrTeamLifecycleConflict) {
		t.Fatalf("stale revision error = %v", err)
	}
	noChange, err := updateTeamLifecycleFile(path, "candidate", TeamLifecycleRetired, change.Revision, func(string) bool { return false })
	if err != nil || noChange.Changed || noChange.Revision != change.Revision {
		t.Fatalf("no-op change = %+v err=%v", noChange, err)
	}
}

func TestUpdateTeamLifecycleRejectsProcessDefault(t *testing.T) {
	dir := t.TempDir()
	content := []byte("active_team: wiki_suggest\nteams:\n  wiki_suggest:\n    lifecycle: active\n    workflow: planner_researcher_writer\n")
	path := filepath.Join(dir, "teams.yaml")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := updateTeamLifecycleFile(path, "wiki_suggest", TeamLifecycleDraining, teamsFileRevision(content), func(team string) bool { return team == "wiki_suggest" }); !errors.Is(err, ErrTeamLifecycleDefault) {
		t.Fatalf("default lifecycle error = %v", err)
	}
}
