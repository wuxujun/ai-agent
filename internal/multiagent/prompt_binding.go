package multiagent

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/wuxujun/ai-agent/internal/promptmanager"
	"github.com/wuxujun/ai-agent/internal/types"
)

const PromptVersionBindingTraceAction = "multiagent_prompt_version"

type persistedPromptVersionBinding struct {
	SchemaVersion int                      `json:"schema_version"`
	TeamDigest    string                   `json:"team_digest"`
	Pin           promptmanager.VersionPin `json:"pin"`
}

func persistedPromptVersionPins(task *types.Task, teamDigest string) []promptmanager.VersionPin {
	if task == nil || strings.TrimSpace(teamDigest) == "" {
		return nil
	}
	byName := make(map[string]promptmanager.VersionPin)
	order := make([]string, 0)
	for _, trace := range task.Trace {
		if trace.Action != PromptVersionBindingTraceAction {
			continue
		}
		var binding persistedPromptVersionBinding
		if json.Unmarshal([]byte(trace.Observation), &binding) != nil || binding.SchemaVersion != 1 || binding.TeamDigest != teamDigest || binding.Pin.Version <= 0 || strings.TrimSpace(binding.Pin.Name) == "" {
			continue
		}
		if _, exists := byName[binding.Pin.Name]; !exists {
			order = append(order, binding.Pin.Name)
		}
		byName[binding.Pin.Name] = binding.Pin
	}
	result := make([]promptmanager.VersionPin, 0, len(order))
	for _, name := range order {
		result = append(result, byName[name])
	}
	return result
}

func appendPromptVersionBinding(task *types.Task, teamDigest string, pin promptmanager.VersionPin) {
	if task == nil || strings.TrimSpace(teamDigest) == "" || strings.TrimSpace(pin.Name) == "" || pin.Version <= 0 {
		return
	}
	for _, existing := range persistedPromptVersionPins(task, teamDigest) {
		if existing.Name == pin.Name && existing.Version == pin.Version {
			return
		}
	}
	pin.Labels = boundedPromptLabels(pin.Labels)
	binding := persistedPromptVersionBinding{SchemaVersion: 1, TeamDigest: teamDigest, Pin: pin}
	observation, err := json.Marshal(binding)
	if err != nil {
		return
	}
	task.Trace = append(task.Trace, types.StepTrace{
		Step:        task.StepCount,
		Goal:        task.Goal,
		Action:      PromptVersionBindingTraceAction,
		Query:       pin.Name,
		Observation: string(observation),
		Evidence: []types.Evidence{{
			Path:  "prompt_version",
			Query: pin.Name,
			Lines: []string{fmt.Sprintf("version:%d", pin.Version), "selector:" + pin.Selector.String(), "team_digest:" + teamDigest},
		}},
	})
	task.StepCount++
}

func boundedPromptLabels(labels []string) []string {
	if len(labels) > 10 {
		labels = labels[:10]
	}
	result := make([]string, 0, len(labels))
	for _, label := range labels {
		label = strings.TrimSpace(label)
		runes := []rune(label)
		if len(runes) > 100 {
			label = string(runes[:100])
		}
		if label != "" {
			result = append(result, label)
		}
	}
	return result
}
