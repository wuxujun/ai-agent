package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/multiagent"
)

func (h *Handler) listTeams(c *gin.Context) {
	principal := principalFromGin(c)
	tenant := config.Get().API.Tenants[principal.TenantID]
	configured := multiagent.ListTeamSummaries()
	available := make([]multiagent.TeamSummary, 0, len(configured))
	defaultTeam := strings.TrimSpace(tenant.DefaultMultiAgentTeam)
	defaultSource := "tenant_default"
	if defaultTeam == "" {
		defaultSource = "global_default"
		for _, team := range configured {
			if team.Default {
				defaultTeam = team.Name
				break
			}
		}
	}
	for _, team := range configured {
		if !principal.Admin && !tenantAllowsMultiAgentTeam(tenant, team.Name) {
			continue
		}
		team.Default = team.Name == defaultTeam
		available = append(available, team)
	}
	if !principal.Admin && !tenantAllowsMultiAgentTeam(tenant, defaultTeam) {
		defaultTeam = ""
		defaultSource = ""
	}
	revision, err := multiagent.TeamsConfigRevision()
	if err != nil {
		c.Error(err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"default_team":    defaultTeam,
		"default_source":  defaultSource,
		"config_revision": revision,
		"teams":           available,
	})
}

type updateTeamLifecycleRequest struct {
	Lifecycle        multiagent.TeamLifecycle `json:"lifecycle" binding:"required"`
	ExpectedRevision string                   `json:"expected_revision" binding:"required"`
}

func (h *Handler) updateTeamLifecycle(c *gin.Context) {
	var req updateTeamLifecycleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.Error(err)
		return
	}
	change, err := multiagent.UpdateTeamLifecycle(c.Param("name"), req.Lifecycle, req.ExpectedRevision)
	if err != nil {
		status := http.StatusBadRequest
		switch {
		case errors.Is(err, multiagent.ErrTeamLifecycleConflict):
			status = http.StatusConflict
			if h.metrics != nil {
				h.metrics.ObserveMultiAgentTeamConfigEvent(c.Request.Context(), "lifecycle_conflict")
			}
		case errors.Is(err, multiagent.ErrTeamLifecycleDefault):
			status = http.StatusUnprocessableEntity
			if h.metrics != nil {
				h.metrics.ObserveMultiAgentTeamConfigEvent(c.Request.Context(), "default_protected")
			}
		case strings.Contains(err.Error(), "is not configured"):
			status = http.StatusNotFound
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	principal := principalFromGin(c)
	if h.metrics != nil && change.Changed {
		h.metrics.ObserveMultiAgentTeamConfigEvent(c.Request.Context(), "lifecycle_changed")
	}
	log.Info("Multi-agent Team lifecycle updated",
		"tenant_id", principal.TenantID,
		"team", change.Team,
		"previous_lifecycle", change.Previous,
		"lifecycle", change.Current,
		"previous_revision", change.PreviousRevision,
		"revision", change.Revision,
		"changed", change.Changed,
	)
	audit, auditErr := multiagent.AppendTeamLifecycleAudit(principal.TenantID, change)
	if auditErr != nil {
		log.Error("Failed to persist multi-agent Team lifecycle audit", "tenant_id", principal.TenantID, "team", change.Team, "error", auditErr)
		status := http.StatusInternalServerError
		if errors.Is(auditErr, multiagent.ErrTeamLifecycleAuditFull) {
			status = http.StatusInsufficientStorage
			if h.metrics != nil {
				h.metrics.ObserveMultiAgentTeamConfigEvent(c.Request.Context(), "audit_capacity_rejected")
			}
		}
		c.JSON(status, gin.H{
			"error":            "Team lifecycle changed but audit persistence failed",
			"lifecycle_change": change,
			"audit_persisted":  false,
		})
		return
	}
	c.JSON(http.StatusOK, struct {
		multiagent.TeamLifecycleChange
		Audit multiagent.TeamLifecycleAuditRecord `json:"audit"`
	}{TeamLifecycleChange: change, Audit: audit})
}

func (h *Handler) listTeamLifecycleAudits(c *gin.Context) {
	limit, offset := pageParams(c, 50, 200)
	filter := multiagent.TeamLifecycleAuditFilter{Team: strings.TrimSpace(c.Query("team"))}
	if rawChanged := strings.TrimSpace(c.Query("changed")); rawChanged != "" {
		changed, err := strconv.ParseBool(rawChanged)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "changed must be true or false"})
			return
		}
		filter.Changed = &changed
	}
	records, hasMore, err := multiagent.ListTeamLifecycleAuditsFiltered(filter, limit, offset)
	if err != nil {
		c.Error(err)
		return
	}
	filters := gin.H{}
	if filter.Team != "" {
		filters["team"] = filter.Team
	}
	if filter.Changed != nil {
		filters["changed"] = *filter.Changed
	}
	c.JSON(http.StatusOK, gin.H{"audits": records, "limit": limit, "offset": offset, "has_more": hasMore, "filters": filters})
}

func (h *Handler) getTeamLifecycleAuditIntegrity(c *gin.Context) {
	integrity := multiagent.CheckTeamLifecycleAuditIntegrity()
	status := http.StatusOK
	if !integrity.Healthy {
		status = http.StatusConflict
		if h.metrics != nil {
			h.metrics.ObserveMultiAgentTeamConfigEvent(c.Request.Context(), "audit_integrity_failure")
		}
	}
	c.JSON(status, integrity)
}

type archiveTeamLifecycleAuditsRequest struct {
	ExpectedFileDigest string `json:"expected_file_digest" binding:"required"`
}

func (h *Handler) archiveTeamLifecycleAudits(c *gin.Context) {
	var req archiveTeamLifecycleAuditsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.Error(err)
		return
	}
	archive, err := multiagent.ArchiveTeamLifecycleAudit(req.ExpectedFileDigest)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, multiagent.ErrTeamLifecycleAuditArchiveConflict) {
			status = http.StatusConflict
			if h.metrics != nil {
				h.metrics.ObserveMultiAgentTeamConfigEvent(c.Request.Context(), "audit_archive_conflict")
			}
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	principal := principalFromGin(c)
	if h.metrics != nil {
		h.metrics.ObserveMultiAgentTeamConfigEvent(c.Request.Context(), "audit_archived")
	}
	log.Info("Multi-agent Team lifecycle audit archived",
		"tenant_id", principal.TenantID,
		"archive", archive.Name,
		"file_digest", archive.FileDigest,
		"record_count", archive.RecordCount,
		"file_size_bytes", archive.FileSizeBytes,
	)
	c.JSON(http.StatusCreated, archive)
}

func (h *Handler) listTeamLifecycleAuditArchives(c *gin.Context) {
	archives, err := multiagent.ListTeamLifecycleAuditArchives()
	if err != nil {
		c.Error(err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"archives": archives, "count": len(archives)})
}
