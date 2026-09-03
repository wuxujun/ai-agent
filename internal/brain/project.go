// Package brain contains the project-scoped, read-only Brain Wiki contracts.
package brain

import (
	"crypto/sha256"
	"fmt"
	"strings"

	"github.com/wuxujun/ai-agent/internal/config"
)

// ProjectRef is the fully authorized Brain scope. StorageKey is a stable hash
// of the tenant identity so storage layouts do not disclose tenant names.
type ProjectRef struct {
	TenantID   string
	ProjectID  string
	WikiSpace  string
	StorageKey string
}

// ResolveProject authorizes one explicit Brain project for one tenant. It
// rejects disabled Brain, malformed identifiers, unknown tenants, and projects
// not present in that tenant's configured allowlist.
func ResolveProject(cfg *config.Config, tenantID, projectID string) (ProjectRef, error) {
	if cfg == nil {
		return ProjectRef{}, fmt.Errorf("brain configuration is required")
	}
	if !cfg.Brain.Enabled {
		return ProjectRef{}, fmt.Errorf("brain is disabled")
	}
	if strings.TrimSpace(tenantID) == "" || strings.TrimSpace(tenantID) != tenantID {
		return ProjectRef{}, fmt.Errorf("brain tenant id must be non-empty and trimmed")
	}
	if !isProjectSlug(projectID) {
		return ProjectRef{}, fmt.Errorf("brain project id %q must be a strict slug", projectID)
	}
	tenant, exists := cfg.API.Tenants[tenantID]
	if !exists {
		return ProjectRef{}, fmt.Errorf("brain tenant %q is not configured", tenantID)
	}
	project, exists := tenant.BrainProjects[projectID]
	if !exists {
		return ProjectRef{}, fmt.Errorf("brain project %q is not authorized for tenant %q", projectID, tenantID)
	}
	wikiSpace := strings.TrimSpace(project.WikiSpace)
	if wikiSpace == "" || wikiSpace != project.WikiSpace {
		return ProjectRef{}, fmt.Errorf("brain project %q has an invalid wiki space", projectID)
	}
	storageHash := sha256.Sum256([]byte(tenantID))
	return ProjectRef{
		TenantID:   tenantID,
		ProjectID:  projectID,
		WikiSpace:  wikiSpace,
		StorageKey: fmt.Sprintf("sha256:%x", storageHash),
	}, nil
}

// ProjectConfigDigest returns a stable digest for the project scope that a
// task pins. NUL delimiters make the encoded fields unambiguous.
func ProjectConfigDigest(ref ProjectRef) string {
	encoded := strings.Join([]string{ref.TenantID, ref.ProjectID, ref.WikiSpace, ref.StorageKey}, "\x00")
	digest := sha256.Sum256([]byte(encoded))
	return fmt.Sprintf("sha256:%x", digest)
}

func isProjectSlug(value string) bool {
	if value == "" || strings.TrimSpace(value) != value || value[0] == '-' || value[len(value)-1] == '-' {
		return false
	}
	previousHyphen := false
	for _, r := range value {
		if r == '-' {
			if previousHyphen {
				return false
			}
			previousHyphen = true
			continue
		}
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
		previousHyphen = false
	}
	return true
}
