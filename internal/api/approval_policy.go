package api

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

const crossTenantApprovalKey = "cross_tenant_approval"

// Authority comes from the server's current policy and a verified personal
// identity, never a raw userid, provider role, request header or body field.
func crossTenantApprovalAllowed(principal Principal, cfg *config.Config) bool {
	tenant := cfg.API.Tenants[principal.TenantID]
	if !tenant.ApprovalCrossTenant || tenant.ApprovalMode != "actors" || !tenant.ApprovalDisallowSelf || !(bearerIdentity{ActorID: principal.ActorID}).personal() {
		return false
	}
	for _, actor := range tenant.ApprovalActorIDs {
		if actor == principal.ActorID {
			return true
		}
	}
	return false
}

func approvalDecisionRoute(c *gin.Context) bool {
	return c.Request.Method == http.MethodPost && (c.FullPath() == "/api/tasks/:id/approve" || c.FullPath() == "/api/tasks/:id/reject")
}

// ApprovalAuthorizationMiddleware is deliberately before all resolver/CAS/bus
// effects. Authority comes from local tenant policy, never a provider role or
// a request's actor_id. Read routes retain their tenant-scoped authorization.
func ApprovalAuthorizationMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		principal := principalFromGin(c)
		tenant := config.Get().API.Tenants[principal.TenantID]
		allowed := tenant.ApprovalMode == "" || tenant.ApprovalMode == "tenant"
		if tenant.ApprovalMode == "actors" {
			for _, actor := range tenant.ApprovalActorIDs {
				if actor == principal.ActorID && (bearerIdentity{ActorID: actor}).personal() {
					allowed = true
					break
				}
			}
		}
		if !allowed {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "approval permission required"})
			return
		}
		c.Next()
	}
}

// The task owner's policy applies even to cross-tenant administrators. Read
// persisted identity before decoding decisions or invoking CAS/recovery/buses.
func (h *Handler) authorizeIndependentApproval(c *gin.Context, taskID string) bool {
	cfg := config.Get()
	// A globally scoped approver must remain independent even when the target
	// tenant retains its legacy policy. Missing historical creators fail closed.
	globalApprover := crossTenantApprovalAllowed(principalFromGin(c), cfg)
	if crossed, _ := c.Get(crossTenantApprovalKey); crossed == true && !globalApprover {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "task not found"})
		return false
	}
	enabled := globalApprover
	for _, tenant := range cfg.API.Tenants {
		if tenant.ApprovalDisallowSelf {
			enabled = true
			break
		}
	}
	if !enabled {
		return true
	}
	var task *types.Task
	var err error
	if reader, ok := h.store.(store.TaskSummaryStore); ok {
		task, err = reader.GetTaskWithoutTrace(c.Request.Context(), taskID)
	} else {
		task, err = h.store.GetTask(c.Request.Context(), taskID)
	}
	if errors.Is(err, sql.ErrNoRows) {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "task not found"})
		return false
	}
	if err != nil || task == nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to verify task creator"})
		return false
	}
	tenantID := task.TenantID
	if tenantID == "" {
		tenantID = "default"
	}
	if !globalApprover && !cfg.API.Tenants[tenantID].ApprovalDisallowSelf {
		return true
	}
	actor := principalFromGin(c).ActorID
	if !(bearerIdentity{ActorID: actor}).personal() || !(bearerIdentity{ActorID: task.CreatorActorID}).personal() {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "verified personal creator and approver identities required"})
		return false
	}
	if actor == task.CreatorActorID {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "task creator cannot resolve own approvals"})
		return false
	}
	return true
}
