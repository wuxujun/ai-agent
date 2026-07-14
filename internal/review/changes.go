package review

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/wuxujun/ai-agent/internal/policy"
	"github.com/wuxujun/ai-agent/internal/types"
)

const maxChangeBytes = 96 << 10

type ChangeSet struct {
	Paths []string
	Diff  string
}

func TaskMayHaveCodeChanges(task *types.Task) bool {
	for _, trace := range task.Trace {
		switch trace.Action {
		case "write_file", "apply_patch", "execute_code":
			if trace.Error == "" {
				return true
			}
		}
	}
	return false
}

func CollectChanges(ctx context.Context, workspace string) (ChangeSet, error) {
	if err := policy.ValidateWorkspace(workspace); err != nil {
		return ChangeSet{}, err
	}
	status, err := runGitLimited(ctx, workspace, maxChangeBytes, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return ChangeSet{}, fmt.Errorf("inspect git status: %w", err)
	}
	paths := parseStatusPaths(status)
	if len(paths) == 0 {
		return ChangeSet{}, nil
	}
	diff, err := runGitLimited(ctx, workspace, maxChangeBytes, "diff", "--no-ext-diff", "--unified=40", "HEAD", "--")
	if err != nil {
		// A repository without an initial commit has no HEAD. The staged and
		// working-tree diffs still provide useful review context.
		staged, stagedErr := runGitLimited(ctx, workspace, maxChangeBytes/2, "diff", "--no-ext-diff", "--cached", "--")
		working, workingErr := runGitLimited(ctx, workspace, maxChangeBytes/2, "diff", "--no-ext-diff", "--")
		if stagedErr != nil || workingErr != nil {
			return ChangeSet{}, fmt.Errorf("collect git diff: %w", err)
		}
		diff = staged + working
	}
	var content strings.Builder
	content.WriteString(diff)
	reviewed := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		if !isUntrackedStatus(status, path) {
			reviewed[path] = struct{}{}
			continue
		}
		if !reviewableUntracked(path) || content.Len() >= maxChangeBytes {
			continue
		}
		text, readErr := readUntracked(workspace, path, maxChangeBytes-content.Len())
		if readErr != nil || text == "" {
			continue
		}
		reviewed[path] = struct{}{}
		fmt.Fprintf(&content, "\n--- /dev/null\n+++ b/%s\n@@ new file @@\n%s\n", path, text)
	}
	paths = paths[:0]
	for path := range reviewed {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return ChangeSet{Paths: paths, Diff: redactSecrets(truncateBytes(content.String(), maxChangeBytes))}, nil
}

func parseStatusPaths(status string) []string {
	seen := map[string]struct{}{}
	parts := strings.Split(status, "\x00")
	for i := 0; i < len(parts); i++ {
		entry := parts[i]
		if len(entry) < 4 {
			continue
		}
		path := entry[3:]
		if validRelativePath(path) {
			seen[path] = struct{}{}
		}
		if entry[0] == 'R' || entry[0] == 'C' || entry[1] == 'R' || entry[1] == 'C' {
			if i+1 < len(parts) {
				i++
				path = parts[i]
			}
		}
		if validRelativePath(path) {
			seen[path] = struct{}{}
		}
	}
	paths := make([]string, 0, len(seen))
	for path := range seen {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

func isUntrackedStatus(status, path string) bool {
	return strings.Contains(status, "?? "+path+"\x00")
}

func readUntracked(workspace, relative string, limit int) (string, error) {
	if limit <= 0 || !validRelativePath(relative) {
		return "", nil
	}
	full := filepath.Join(workspace, relative)
	if err := policy.ValidateReadPath(workspace, full); err != nil {
		return "", err
	}
	file, err := os.OpenFile(full, os.O_RDONLY|policy.O_NOFOLLOW, 0)
	if err != nil {
		return "", err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil || bytes.IndexByte(raw, 0) >= 0 {
		return "", err
	}
	return truncateBytes(string(raw), limit), nil
}

func validRelativePath(path string) bool {
	clean := filepath.Clean(path)
	return path != "" && clean == path && !filepath.IsAbs(path) && path != ".." && !strings.HasPrefix(path, ".."+string(filepath.Separator)) && strings.IndexFunc(path, unicode.IsControl) < 0
}

func reviewableUntracked(path string) bool {
	base := strings.ToLower(filepath.Base(path))
	if base == "dockerfile" || base == "makefile" || base == "go.mod" || base == "go.sum" {
		return true
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go", ".py", ".js", ".jsx", ".ts", ".tsx", ".java", ".kt", ".kts", ".rs", ".c", ".h", ".cc", ".cpp", ".cs", ".rb", ".php", ".swift", ".scala", ".sh", ".bash", ".sql", ".proto", ".yaml", ".yml", ".json", ".toml", ".xml", ".md":
		return true
	default:
		return false
	}
}

var secretPatterns = []struct {
	pattern     *regexp.Regexp
	replacement string
}{
	{regexp.MustCompile(`(?is)-----BEGIN [^-\n]*PRIVATE KEY-----.*?-----END [^-\n]*PRIVATE KEY-----`), `[REDACTED PRIVATE KEY]`},
	{regexp.MustCompile(`(?is)-----BEGIN [^-\n]*PRIVATE KEY-----.*`), `[REDACTED PRIVATE KEY]`},
	{regexp.MustCompile(`(?i)(authorization.{0,20}bearer\s+)[A-Za-z0-9._~+/=-]+`), `${1}[REDACTED]`},
	{regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{12,}`), `[REDACTED API KEY]`},
	{regexp.MustCompile(`\bAKIA[A-Z0-9]{16}\b`), `[REDACTED AWS KEY]`},
	{regexp.MustCompile(`\bgh[psour]_[A-Za-z0-9]{20,}\b`), `[REDACTED GITHUB TOKEN]`},
	{regexp.MustCompile(`(?m)^([+ -]?\s*["']?[A-Z0-9_]*(?:API_KEY|SECRET|TOKEN|PASSWORD|MASTER_KEY)[A-Z0-9_]*["']?\s*[:=]\s*)\S+.*$`), `${1}[REDACTED]`},
	{regexp.MustCompile(`(?im)^([+ -]?\s*["']?(?:api[_-]?key|secret|token|password|authorization)["']?\s*[:=]\s*)\S+.*$`), `${1}[REDACTED]`},
}

func redactSecrets(value string) string {
	for _, rule := range secretPatterns {
		value = rule.pattern.ReplaceAllString(value, rule.replacement)
	}
	return value
}

type cappedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	original := len(p)
	remaining := b.limit - b.Len()
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = b.Buffer.Write(p)
	}
	return original, nil
}

func runGitLimited(ctx context.Context, workspace string, limit int, args ...string) (string, error) {
	if err := policy.ValidateCommand("git"); err != nil {
		return "", err
	}
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(callCtx, "git", args...)
	cmd.Dir = workspace
	stdout := &cappedBuffer{limit: limit}
	stderr := &cappedBuffer{limit: 4096}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		if callCtx.Err() != nil {
			return stdout.String(), callCtx.Err()
		}
		return stdout.String(), fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func truncateBytes(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
