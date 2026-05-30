package policy

import (
	"errors"
	"path/filepath"
	"strings"
)

var allowedCommands = map[string]bool{
	"rg":      true,
	"find":    true,
	"cat":     true,
	"python3": true,
	"python":  true,
	"go":      true,
	"node":    true,
	"bash":    true,
	"sh":      true,
}

func ValidateWorkspace(root string) error {
	clean := filepath.Clean(root)
	if clean == "." || clean == "/" {
		return errors.New("workspace too broad")
	}
	if strings.Contains(clean, "..") {
		return errors.New("invalid workspace path")
	}
	return nil
}

func ValidateCommand(name string) error {
	if !allowedCommands[name] {
		return errors.New("command not allowed")
	}
	return nil
}

func ValidateReadPath(workspace, target string) error {
	w, err := filepath.Abs(filepath.Clean(workspace))
	if err != nil {
		return err
	}
	t, err := filepath.Abs(filepath.Clean(target))
	if err != nil {
		return err
	}
	if t != w && !strings.HasPrefix(t, w+string(filepath.Separator)) {
		return errors.New("target outside workspace")
	}
	return nil
}

func ValidateWritePath(workspace, target string) error {
	return ValidateReadPath(workspace, target)
}

