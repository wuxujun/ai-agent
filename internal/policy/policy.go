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
	"git":     true,
}

func ValidateWorkspace(root string) error {
	cleanRaw := filepath.Clean(root)
	if cleanRaw == "." || cleanRaw == "/" {
		return errors.New("workspace too broad")
	}
	if strings.Contains(cleanRaw, "..") {
		return errors.New("invalid workspace path")
	}

	abs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	eval, err := filepath.EvalSymlinks(abs)
	if err != nil {
		eval = filepath.Clean(abs)
	}

	cleanAbs := filepath.Clean(eval)
	if cleanAbs == "/" {
		return errors.New("workspace too broad")
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
	if wEval, err := filepath.EvalSymlinks(w); err == nil {
		w = wEval
	}
	w = filepath.Clean(w)

	t, err := filepath.Abs(filepath.Clean(target))
	if err != nil {
		return err
	}
	if tEval, err := evalExistingPath(t); err == nil {
		t = tEval
	}
	t = filepath.Clean(t)

	if t != w && !strings.HasPrefix(t, w+string(filepath.Separator)) {
		return errors.New("target outside workspace")
	}
	return nil
}

func ValidateWritePath(workspace, target string) error {
	return ValidateReadPath(workspace, target)
}

func evalExistingPath(path string) (string, error) {
	curr := path
	var suffix string
	for {
		eval, err := filepath.EvalSymlinks(curr)
		if err == nil {
			if suffix == "" {
				return eval, nil
			}
			return filepath.Join(eval, suffix), nil
		}
		parent := filepath.Dir(curr)
		if parent == curr {
			return path, nil // reached root, fallback to original path
		}
		base := filepath.Base(curr)
		if suffix == "" {
			suffix = base
		} else {
			suffix = filepath.Join(base, suffix)
		}
		curr = parent
	}
}

