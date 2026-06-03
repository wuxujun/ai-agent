package planner

import (
	"errors"
	"fmt"
	"strings"
)

func ValidateDecision(d *PlanDecision) error {
	valid := map[string]bool{
		"find_files":   true,
		"search_text":  true,
		"read_file":    true,
		"write_file":   true,
		"execute_code": true,
		"none":         true,
	}

	if len(d.Actions) == 0 {
		return errors.New("decision must contain at least one action")
	}

	for _, ac := range d.Actions {
		if !valid[ac.Action] {
			return fmt.Errorf("invalid action: %s", ac.Action)
		}

		if d.Stop && ac.Action != "none" {
			return errors.New("stop=true requires action=none")
		}
		if !d.Stop && ac.Action == "none" {
			return errors.New("action=none requires stop=true")
		}

		var err error
		switch ac.Action {
		case "find_files":
			err = validateFindFiles(ac.Parameters)
		case "search_text":
			err = validateSearchText(ac.Parameters)
		case "read_file":
			err = validateReadFile(ac.Parameters)
		case "write_file":
			err = validateWriteFile(ac.Parameters)
		case "execute_code":
			err = validateExecuteCode(ac.Parameters)
		case "none":
			if strings.TrimSpace(d.FinalAnswer) == "" {
				err = errors.New("stop decision requires final_answer")
			}
		}

		if err != nil {
			return fmt.Errorf("validation failed for action %s: %w", ac.Action, err)
		}
	}
	return nil
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

func validateWriteFile(params map[string]any) error {
	path, _ := params["path"].(string)
	path = strings.TrimSpace(path)
	if path == "" {
		return errors.New("write_file requires non-empty path")
	}
	if strings.HasPrefix(path, "/") || strings.Contains(path, "..") {
		return errors.New("invalid write_file path")
	}
	// content is required to be present as a string type
	if _, ok := params["content"].(string); !ok {
		return errors.New("write_file requires content string parameter")
	}
	return nil
}

func validateExecuteCode(params map[string]any) error {
	command, _ := params["command"].(string)
	command = strings.TrimSpace(command)
	if command == "" {
		return errors.New("execute_code requires non-empty command")
	}
	// args is required to be present as a string type
	if _, ok := params["args"].(string); !ok {
		return errors.New("execute_code requires args string parameter")
	}
	return nil
}
