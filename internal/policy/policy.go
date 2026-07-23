package policy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
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

var lookupIP = net.LookupIP
var dialResolvedIP = func(ctx context.Context, network, address string, timeout time.Duration) (net.Conn, error) {
	dialer := &net.Dialer{
		Timeout:   timeout,
		KeepAlive: 30 * time.Second,
	}
	return dialer.DialContext(ctx, network, address)
}

// blockedSystemPaths lists path prefixes that are unconditionally forbidden as
// workspace roots, even if they happen to exist and pass other checks. These
// directories contain OS internals or sensitive host data that the agent must
// never be allowed to browse.
var blockedSystemPaths = []string{
	"/etc",
	"/proc",
	"/sys",
	"/dev",
	"/run",
	"/boot",
	"/root",
	"/private/etc", // macOS shadow of /etc
	"/private/var",
}

// ValidateWorkspace ensures that root is a safe, non-escaping workspace path.
//
// Security guarantees:
//  1. Rejects "." and "/" (too broad).
//  2. Rejects raw paths containing ".." before cleaning.
//  3. Converts root to an absolute path, then resolves ALL symlink components
//     using evalExistingPath (which walks upward to the deepest existing
//     ancestor) — this prevents "create a non-existent path to skip EvalSymlinks"
//     attacks.
//  4. If the resolved real path differs from the cleaned absolute path, the
//     workspace itself is (or traverses) a symlink; this is flagged explicitly.
//  5. Rejects resolved paths that are, or are prefixes of, known sensitive
//     system directories (/etc, /proc, /sys, …).
//  6. Rejects paths that escape the application's working directory (cwd).
//     Both cwd and the workspace root are symlink-resolved before comparison,
//     so a symlink-based cwd cannot be used to widen the allowed zone.
func ValidateWorkspace(root string) error {
	cleanRaw := filepath.Clean(root)
	if cleanRaw == "." {
		return errors.New("workspace too broad: must not be the current directory")
	}
	if strings.Contains(cleanRaw, "..") {
		return errors.New("invalid workspace path: contains '..'")
	}

	abs, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("workspace path error: %w", err)
	}

	// Resolve every symlink component, even for paths that do not fully exist yet.
	// evalExistingPath walks from the deepest existing ancestor upward, so a
	// symlink at any level in the path is always resolved.
	eval, err := evalExistingPath(abs)
	if err != nil {
		return fmt.Errorf("workspace symlink resolution failed: %w", err)
	}
	eval = filepath.Clean(eval)

	// Guard: resolved root must not be the filesystem root.
	if eval == "/" {
		return errors.New("workspace too broad: resolves to filesystem root")
	}

	// Guard: explicit symlink detection.
	// If the real path differs from the cleaned absolute path, the workspace
	// itself is a symlink (or traverses one). Reject it outright — symlinked
	// workspaces make the sandbox boundary hard to reason about.
	absClean := filepath.Clean(abs)
	if eval != absClean {
		return fmt.Errorf("workspace is or traverses a symlink (real path: %s)", eval)
	}

	// Guard: block known sensitive system directories.
	for _, blocked := range blockedSystemPaths {
		blocked = filepath.Clean(blocked)
		if eval == blocked || strings.HasPrefix(eval, blocked+string(filepath.Separator)) {
			return fmt.Errorf("workspace resolves to a restricted system path: %s", eval)
		}
	}

	// Guard: workspace must reside inside the application's working directory.
	// Both sides are symlink-resolved so a symlinked cwd cannot widen the zone.
	cwd, err := filepath.Abs(".")
	if err != nil {
		return fmt.Errorf("cannot determine working directory: %w", err)
	}
	cwdEval, evalErr := evalExistingPath(cwd)
	if evalErr == nil {
		cwd = filepath.Clean(cwdEval)
	} else {
		cwd = filepath.Clean(cwd)
	}

	if eval != cwd && !strings.HasPrefix(eval, cwd+string(filepath.Separator)) {
		return errors.New("workspace escapes application root directory")
	}

	return nil
}

// ValidateURL guards outbound HTTP requests against SSRF. It permits only
// http/https schemes and rejects hosts that resolve to loopback, private,
// link-local, or otherwise non-public addresses (e.g. the cloud metadata
// endpoint 169.254.169.254). Because the URL originates from LLM output, this
// gate prevents the agent from being steered into the internal network.
func ValidateURL(raw string) error {
	if err := validateHTTPURL(raw); err != nil {
		return err
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return errors.New("invalid url")
	}
	host := u.Hostname()

	var ips []net.IP
	if ip := net.ParseIP(host); ip != nil {
		ips = []net.IP{ip}
	} else {
		resolved, err := lookupIP(host)
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

// ValidateConfiguredURL validates an operator-provided HTTP endpoint. Private
// addresses remain blocked by default. They are accepted only when the
// operator explicitly opts in, which is useful for locally hosted MCP servers
// while keeping model-provided URLs behind the strict ValidateURL gate.
func ValidateConfiguredURL(raw string, allowPrivate bool) error {
	if !allowPrivate {
		return ValidateURL(raw)
	}
	return validateHTTPURL(raw)
}

func validateHTTPURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return errors.New("invalid url")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("only http/https urls are allowed")
	}
	if u.Hostname() == "" {
		return errors.New("url has no host")
	}
	if u.User != nil {
		return errors.New("url userinfo is not allowed")
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

// ValidateReadPath ensures that target resides inside workspace after full
// symlink resolution of both paths.
//
// Security guarantees:
//  1. workspace is converted to an absolute path and every symlink component
//     is resolved via evalExistingPath (handles partially-existing paths).
//  2. target is resolved the same way.
//  3. The final containment check uses the resolved real paths, so a symlink
//     placed inside the workspace and pointing outside cannot bypass the gate.
//  4. Both workspace and target are verified to be non-empty after resolution.
func ValidateReadPath(workspace, target string) error {
	// Resolve workspace to its real, canonical, absolute path.
	wAbs, err := filepath.Abs(filepath.Clean(workspace))
	if err != nil {
		return fmt.Errorf("workspace path error: %w", err)
	}
	wReal, err := evalExistingPath(wAbs)
	if err != nil {
		return fmt.Errorf("workspace symlink resolution failed: %w", err)
	}
	w := filepath.Clean(wReal)

	// Resolve target to its real, canonical, absolute path.
	tAbs, err := filepath.Abs(filepath.Clean(target))
	if err != nil {
		return fmt.Errorf("target path error: %w", err)
	}
	tReal, err := evalExistingPath(tAbs)
	if err != nil {
		return fmt.Errorf("target symlink resolution failed: %w", err)
	}
	t := filepath.Clean(tReal)

	// Containment check: t must equal w or be a child of w.
	if t != w && !strings.HasPrefix(t, w+string(filepath.Separator)) {
		return fmt.Errorf("target outside workspace (real path %q not under %q)", t, w)
	}
	return nil
}

// ValidateWritePath delegates to ValidateReadPath — the containment rule is
// identical for reads and writes.
func ValidateWritePath(workspace, target string) error {
	return ValidateReadPath(workspace, target)
}

// evalExistingPath resolves as many symlink components of path as possible,
// even when the path (or a suffix of it) does not exist on disk.
//
// Algorithm:
//  1. Try filepath.EvalSymlinks on the full path. If it succeeds, return.
//  2. Otherwise, peel the last component into a suffix, move to the parent, and
//     repeat from step 1.
//  3. If we reach the filesystem root without a successful eval, return the
//     original path (best-effort; the OS will reject it on access anyway).
//
// This approach prevents the "path not found → skip EvalSymlinks → bypass
// symlink checks" attack: even if the final file does not exist, every existing
// directory component in the path is still resolved through its real location.
//
// Example:
//
//	workspace/evil_link/nonexistent.txt
//	  → evil_link exists and is a symlink → resolves to /outside/nonexistent.txt
//	  → containment check fails ✓
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
		// Check whether the error is due to a non-existent component (vs. a
		// real I/O error such as permission denied).
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("evalExistingPath: %w", err)
		}
		parent := filepath.Dir(curr)
		if parent == curr {
			// Reached the filesystem root without a successful resolution;
			// return the original path as a safe fallback.
			return path, nil
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

// SafeHTTPClient returns an http.Client designed to protect against SSRF and DNS Rebinding.
// It intercepts and validates IP addresses during connection Dialing and redirection.
func SafeHTTPClient(timeout time.Duration) *http.Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, fmt.Errorf("SSRF guard: invalid address: %w", err)
			}

			var ips []net.IP
			if ip := net.ParseIP(host); ip != nil {
				ips = []net.IP{ip}
			} else {
				ips, err = lookupIP(host)
			}
			if err != nil {
				return nil, fmt.Errorf("SSRF guard: DNS lookup failed: %w", err)
			}
			if len(ips) == 0 {
				return nil, fmt.Errorf("SSRF guard: DNS lookup returned no addresses")
			}

			for _, ip := range ips {
				if isBlockedIP(ip) {
					return nil, fmt.Errorf("SSRF guard: connection to private/restricted IP blocked: %s", ip)
				}
			}

			// Dial the exact address that was just validated. Dialing addr again
			// would resolve the hostname a second time and reopen a DNS-rebinding
			// window between validation and connection establishment.
			target := net.JoinHostPort(ips[0].String(), port)
			return dialResolvedIP(ctx, network, target, timeout)
		},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	return &http.Client{
		Transport: otelhttp.NewTransport(transport),
		Timeout:   timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("stopped after 10 redirects")
			}
			// Verify each redirect URL's domain/IP
			urlStr := req.URL.String()
			if err := ValidateURL(urlStr); err != nil {
				return fmt.Errorf("SSRF guard redirect violation: %w", err)
			}
			return nil
		},
	}
}

// ConfiguredHTTPClient applies the outbound network policy selected for a
// trusted, operator-configured endpoint. The default path is the SSRF-safe
// client. An explicit private-network opt-in still enforces HTTP(S)-only
// redirects and bounded connection behavior.
func ConfiguredHTTPClient(timeout time.Duration, allowPrivate bool) *http.Client {
	if !allowPrivate {
		return SafeHTTPClient(timeout)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	return &http.Client{
		Transport: otelhttp.NewTransport(transport),
		Timeout:   timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("stopped after 10 redirects")
			}
			if err := ValidateConfiguredURL(req.URL.String(), true); err != nil {
				return fmt.Errorf("configured endpoint redirect violation: %w", err)
			}
			return nil
		},
	}
}
