package multiagent

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/wuxujun/ai-agent/internal/config"
	"gopkg.in/yaml.v3"
)

var (
	ErrTeamLifecycleConflict = errors.New("Team lifecycle revision conflict")
	ErrTeamLifecycleDefault  = errors.New("default Team must remain active")
	teamLifecycleWriteMu     sync.Mutex
)

type TeamLifecycleChange struct {
	Team             string        `json:"team"`
	Previous         TeamLifecycle `json:"previous"`
	Current          TeamLifecycle `json:"current"`
	PreviousRevision string        `json:"previous_revision"`
	Revision         string        `json:"revision"`
	Changed          bool          `json:"changed"`
}

func TeamsConfigRevision() (string, error) {
	_, data, err := readTeamsConfigFile()
	if err != nil {
		return "", err
	}
	return teamsFileRevision(data), nil
}

// UpdateTeamLifecycle performs an optimistic, atomic update of the repository
// teams.yaml. expectedRevision must come from GET /api/teams.
func UpdateTeamLifecycle(teamName string, lifecycle TeamLifecycle, expectedRevision string) (TeamLifecycleChange, error) {
	teamLifecycleWriteMu.Lock()
	defer teamLifecycleWriteMu.Unlock()
	path, _, err := readTeamsConfigFile()
	if err != nil {
		return TeamLifecycleChange{}, err
	}
	return updateTeamLifecycleFile(path, teamName, lifecycle, expectedRevision, isDefaultTeam)
}

func updateTeamLifecycleFile(path, teamName string, lifecycle TeamLifecycle, expectedRevision string, defaultTeam func(string) bool) (TeamLifecycleChange, error) {
	teamName = strings.TrimSpace(teamName)
	lifecycle = TeamLifecycle(strings.ToLower(strings.TrimSpace(string(lifecycle))))
	if teamName == "" {
		return TeamLifecycleChange{}, fmt.Errorf("Team name is required")
	}
	if !validTeamLifecycle(lifecycle) || lifecycle == "" {
		return TeamLifecycleChange{}, fmt.Errorf("Team lifecycle must be active, draining, or retired")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return TeamLifecycleChange{}, err
	}
	if !info.Mode().IsRegular() {
		return TeamLifecycleChange{}, fmt.Errorf("Team configuration %s is not a regular file", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return TeamLifecycleChange{}, err
	}
	currentRevision := teamsFileRevision(data)
	if strings.TrimSpace(expectedRevision) == "" || expectedRevision != currentRevision {
		return TeamLifecycleChange{}, fmt.Errorf("%w: expected %q, current %q", ErrTeamLifecycleConflict, expectedRevision, currentRevision)
	}

	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return TeamLifecycleChange{}, fmt.Errorf("parse %s: %w", path, err)
	}
	teamNode, lifecycleKey, lifecycleValue := findTeamLifecycleNode(&document, teamName)
	if teamNode == nil {
		return TeamLifecycleChange{}, fmt.Errorf("multi-agent team %q is not configured", teamName)
	}
	previous := TeamLifecycleActive
	if lifecycleValue != nil {
		previous = normalizeTeamLifecycle(TeamLifecycle(lifecycleValue.Value))
	}
	change := TeamLifecycleChange{Team: teamName, Previous: previous, Current: lifecycle, PreviousRevision: currentRevision, Revision: currentRevision}
	if previous == lifecycle {
		return change, nil
	}
	if lifecycle != TeamLifecycleActive && defaultTeam != nil && defaultTeam(teamName) {
		return TeamLifecycleChange{}, fmt.Errorf("%w: %q", ErrTeamLifecycleDefault, teamName)
	}
	updated, err := replaceTeamLifecycleLine(data, teamNode, lifecycleKey, string(lifecycle))
	if err != nil {
		return TeamLifecycleChange{}, err
	}
	if err := atomicWriteTeamsFile(path, updated); err != nil {
		return TeamLifecycleChange{}, err
	}
	change.Changed = true
	change.Revision = teamsFileRevision(updated)
	return change, nil
}

func replaceTeamLifecycleLine(data []byte, teamNode, lifecycleKey *yaml.Node, lifecycle string) ([]byte, error) {
	lines := strings.SplitAfter(string(data), "\n")
	lineIndex := -1
	indent := ""
	if lifecycleKey != nil {
		lineIndex = lifecycleKey.Line - 1
		if lineIndex < 0 || lineIndex >= len(lines) {
			return nil, fmt.Errorf("lifecycle source position is invalid")
		}
		line := strings.TrimSuffix(lines[lineIndex], "\n")
		indent = line[:len(line)-len(strings.TrimLeft(line, " \t"))]
		comment := ""
		if commentAt := strings.Index(line, " #"); commentAt >= 0 {
			comment = line[commentAt:]
		}
		newline := ""
		if strings.HasSuffix(lines[lineIndex], "\n") {
			newline = "\n"
		}
		lines[lineIndex] = indent + "lifecycle: \"" + lifecycle + "\"" + comment + newline
		return []byte(strings.Join(lines, "")), nil
	}
	if teamNode == nil || len(teamNode.Content) == 0 {
		return nil, fmt.Errorf("Team mapping has no insertion point")
	}
	lineIndex = teamNode.Content[0].Line - 1
	if lineIndex < 0 || lineIndex > len(lines) {
		return nil, fmt.Errorf("Team source position is invalid")
	}
	if lineIndex < len(lines) {
		line := strings.TrimSuffix(lines[lineIndex], "\n")
		indent = line[:len(line)-len(strings.TrimLeft(line, " \t"))]
	}
	lines = append(lines, "")
	copy(lines[lineIndex+1:], lines[lineIndex:])
	lines[lineIndex] = indent + "lifecycle: \"" + lifecycle + "\"\n"
	return []byte(strings.Join(lines, "")), nil
}

func readTeamsConfigFile() (string, []byte, error) {
	for _, candidate := range []string{"teams.yaml", "../teams.yaml", "../../teams.yaml"} {
		info, err := os.Lstat(candidate)
		if err != nil {
			continue
		}
		if !info.Mode().IsRegular() {
			return "", nil, fmt.Errorf("Team configuration %s is not a regular file", candidate)
		}
		data, err := os.ReadFile(candidate)
		if err != nil {
			return "", nil, err
		}
		return candidate, data, nil
	}
	return "", nil, fmt.Errorf("load teams.yaml: file not found")
}

func teamsFileRevision(data []byte) string {
	digest := sha256.Sum256(data)
	return fmt.Sprintf("%x", digest[:12])
}

func findTeamLifecycleNode(document *yaml.Node, teamName string) (team, lifecycleKey, lifecycleValue *yaml.Node) {
	if document == nil || len(document.Content) == 0 {
		return nil, nil, nil
	}
	root := document.Content[0]
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "teams" {
			continue
		}
		teams := root.Content[i+1]
		for j := 0; j+1 < len(teams.Content); j += 2 {
			if teams.Content[j].Value != teamName {
				continue
			}
			team = teams.Content[j+1]
			for k := 0; k+1 < len(team.Content); k += 2 {
				if team.Content[k].Value == "lifecycle" {
					return team, team.Content[k], team.Content[k+1]
				}
			}
			return team, nil, nil
		}
	}
	return nil, nil, nil
}

func isDefaultTeam(teamName string) bool {
	teams, err := LoadTeamsConfigStrict()
	if err == nil {
		processDefault := strings.TrimSpace(teams.ActiveTeam)
		if configured := strings.TrimSpace(config.Get().MultiAgent.Team); configured != "" {
			processDefault = configured
		}
		if teamName == processDefault {
			return true
		}
	}
	for _, tenant := range config.Get().API.Tenants {
		if strings.TrimSpace(tenant.DefaultMultiAgentTeam) == teamName {
			return true
		}
	}
	return false
}

func atomicWriteTeamsFile(path string, data []byte) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".teams-*.yaml")
	if err != nil {
		return fmt.Errorf("create Team configuration temporary file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(info.Mode().Perm()); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace Team configuration: %w", err)
	}
	return nil
}
