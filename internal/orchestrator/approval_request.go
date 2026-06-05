package orchestrator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/wuxujun/ai-agent/internal/policy"
	"github.com/wuxujun/ai-agent/internal/tools"
	"github.com/wuxujun/ai-agent/internal/types"
)

const approvalPreviewLimit = 1600

func (e *Engine) BuildApprovalRequest(task *types.Task, action string, params map[string]any) *types.ApprovalRequest {
	risk := types.RiskLevelHigh
	if tool, ok := tools.Get(action); ok {
		risk = tool.RiskLevel()
	}

	req := &types.ApprovalRequest{
		TaskID:           task.ID,
		Action:           action,
		RiskLevel:        risk,
		Workspace:        task.Workspace,
		Parameters:       safeApprovalParameters(params),
		ParameterSummary: approvalParameterSummary(params),
		Preview:          buildApprovalPreview(task.Workspace, action, params),
	}
	return req
}

func safeApprovalParameters(params map[string]any) map[string]any {
	if len(params) == 0 {
		return nil
	}

	safe := make(map[string]any, len(params))
	for key, value := range params {
		safe[key] = safeApprovalValue(key, value)
	}
	return safe
}

func approvalParameterSummary(params map[string]any) []string {
	if len(params) == 0 {
		return nil
	}

	keys := make([]string, 0, len(params))
	for key := range params {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	summary := make([]string, 0, len(keys))
	for _, key := range keys {
		summary = append(summary, fmt.Sprintf("%s=%v", key, safeApprovalValue(key, params[key])))
	}
	return summary
}

func safeApprovalValue(key string, value any) any {
	lowerKey := strings.ToLower(key)
	if strings.Contains(lowerKey, "secret") ||
		strings.Contains(lowerKey, "token") ||
		strings.Contains(lowerKey, "password") ||
		strings.Contains(lowerKey, "api_key") ||
		lowerKey == "key" {
		return "[redacted]"
	}

	s, ok := value.(string)
	if !ok {
		return value
	}
	if key == "content" {
		return fmt.Sprintf("<%d chars>", len(s))
	}
	return truncateForApproval(s, 240)
}

func buildApprovalPreview(workspace, action string, params map[string]any) string {
	switch action {
	case "execute_code":
		command, _ := params["command"].(string)
		args, _ := params["args"].(string)
		return truncateForApproval(fmt.Sprintf("Command preview:\n%s\n\nWorking directory:\n%s", strings.TrimSpace(command+" "+args), workspace), approvalPreviewLimit)
	case "write_file":
		return buildWriteFileApprovalPreview(workspace, params)
	default:
		b, err := json.MarshalIndent(safeApprovalParameters(params), "", "  ")
		if err != nil {
			return ""
		}
		return truncateForApproval("Parameter preview:\n"+string(b), approvalPreviewLimit)
	}
}

func buildWriteFileApprovalPreview(workspace string, params map[string]any) string {
	path, _ := params["path"].(string)
	content, _ := params["content"].(string)
	fullPath := filepath.Join(workspace, path)

	header := fmt.Sprintf("Write file preview:\npath: %s\ncontent_size: %d chars\n\n", path, len(content))
	if err := policy.ValidateWritePath(workspace, fullPath); err != nil {
		return truncateForApproval(header+fmt.Sprintf("Policy warning: %v\n\nProposed content:\n%s", err, prefixLines(content, "+ ")), approvalPreviewLimit)
	}

	existing, err := os.ReadFile(fullPath)
	if err != nil {
		return truncateForApproval(header+"File does not currently exist or cannot be read. Proposed content:\n"+prefixLines(content, "+ "), approvalPreviewLimit)
	}

	return truncateForApproval(header+simpleDiffPreview(string(existing), content), approvalPreviewLimit)
}

func simpleDiffPreview(before, after string) string {
	if before == after {
		return "No content change detected."
	}

	beforeLines := firstLines(before, 12)
	afterLines := firstLines(after, 12)

	var b strings.Builder
	b.WriteString("--- current\n")
	b.WriteString("+++ proposed\n")
	for _, line := range beforeLines {
		b.WriteString("- ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	for _, line := range afterLines {
		b.WriteString("+ ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

func firstLines(s string, limit int) []string {
	lines := strings.Split(s, "\n")
	if len(lines) > limit {
		lines = lines[:limit]
	}
	return lines
}

func prefixLines(s, prefix string) string {
	lines := firstLines(s, 16)
	var b strings.Builder
	for _, line := range lines {
		b.WriteString(prefix)
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

func truncateForApproval(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + "\n...[truncated]"
}
