package policy

import (
	"errors"
	"net"
	"net/url"
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

// ValidateURL guards outbound HTTP requests against SSRF. It permits only
// http/https schemes and rejects hosts that resolve to loopback, private,
// link-local, or otherwise non-public addresses (e.g. the cloud metadata
// endpoint 169.254.169.254). Because the URL originates from LLM output, this
// gate prevents the agent from being steered into the internal network.
func ValidateURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return errors.New("invalid url")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("only http/https urls are allowed")
	}
	host := u.Hostname()
	if host == "" {
		return errors.New("url has no host")
	}

	var ips []net.IP
	if ip := net.ParseIP(host); ip != nil {
		ips = []net.IP{ip}
	} else {
		resolved, err := net.LookupIP(host)
		if err != nil {
			return errors.New("cannot resolve url host")
		}
		ips = resolved
	}

	for _, ip := range ips {
		if isBlockedIP(ip) {
			return errors.New("url resolves to a disallowed (private/loopback/link-local) address")
		}
	}
	return nil
}

func isBlockedIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
		return true
	}
	// Block IPv4 carrier-grade NAT range 100.64.0.0/10 as a precaution.
	if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
		return true
	}
	return false
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

