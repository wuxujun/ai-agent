package planner

import (
	"errors"
	"fmt"
	"strings"
)

func ValidateDecision(d *PlanDecision) error {
	valid := map[string]bool{
		"find_files":  true,
		"search_text": true,
		"read_file":   true,
		"none":        true,
	}

	if !valid[d.Action] {
		return fmt.Errorf("invalid action: %s", d.Action)
	}

	if d.Stop && d.Action != "none" {
		return errors.New("stop=true requires action=none")
	}
	if !d.Stop && d.Action == "none" {
		return errors.New("action=none requires stop=true")
	}

	switch d.Action {
	case "find_files":
		return validateFindFiles(d.Parameters)
	case "search_text":
		return validateSearchText(d.Parameters)
	case "read_file":
		return validateReadFile(d.Parameters)
	case "none":
		if strings.TrimSpace(d.FinalAnswer) == "" {
			return errors.New("stop decision requires final_answer")
		}
		return nil
	default:
		return nil
	}
}

func validateFindFiles(params map[string]any) error {
	pattern, _ := params["pattern"].(string)
	if strings.TrimSpace(pattern) == "" {
		return errors.New("find_files requires non-empty pattern")
	}
	return nil
}

func validateSearchText(params map[string]any) error {
	query, _ := params["query"].(string)
	if strings.TrimSpace(query) == "" {
		return errors.New("search_text requires non-empty query")
	}
	if glob, ok := params["glob"].(string); ok {
		if len(glob) > 100 {
			return errors.New("glob too long")
		}
	}
	return nil
}

func validateReadFile(params map[string]any) error {
	path, _ := params["path"].(string)
	path = strings.TrimSpace(path)
	if path == "" {
		return errors.New("read_file requires non-empty path")
	}
	if strings.HasPrefix(path, "/") || strings.Contains(path, "..") {
		return errors.New("invalid read_file path")
	}
	return nil
}
