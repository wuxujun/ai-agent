package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/wuxujun/ai-agent/internal/store"
	"github.com/wuxujun/ai-agent/internal/types"
)

type createSessionRequest struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

type updateSessionRequest struct {
	Title  *string              `json:"title"`
	Status *types.SessionStatus `json:"status"`
}

func (h *Handler) sessionStore(c *gin.Context) (store.SessionStore, bool) {
	sessions, ok := h.store.(store.SessionStore)
	if !ok {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "session storage is not supported"})
	}
	return sessions, ok
}

func (h *Handler) createSession(c *gin.Context) {
	sessions, ok := h.sessionStore(c)
	if !ok {
		return
	}
	var req createSessionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if strings.TrimSpace(req.ID) == "" {
		req.ID = uuid.NewString()
	} else if !validRequestID(req.ID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid session id"})
		return
	}
	if strings.TrimSpace(req.Title) == "" {
		req.Title = "New session"
	}
	if len([]rune(req.Title)) > 200 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "session title exceeds 200 characters"})
		return
	}
	now := time.Now().UTC()
	session := &types.Session{ID: req.ID, TenantID: principalFromGin(c).TenantID, Title: strings.TrimSpace(req.Title), Status: types.SessionStatusActive, CreatedAt: now, UpdatedAt: now}
	if err := sessions.CreateSession(c.Request.Context(), session); err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "session already exists"})
		return
	}
	c.JSON(http.StatusCreated, session)
}

func (h *Handler) getSession(c *gin.Context) {
	sessions, ok := h.sessionStore(c)
	if !ok {
		return
	}
	session, err := sessions.GetSession(c.Request.Context(), c.Param("id"), principalFromGin(c).TenantID)
	if errors.Is(err, store.ErrSessionNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}
	if err != nil {
		c.Error(err)
		return
	}
	c.JSON(http.StatusOK, session)
}

func (h *Handler) listSessions(c *gin.Context) {
	sessions, ok := h.sessionStore(c)
	if !ok {
		return
	}
	status := types.SessionStatus(c.Query("status"))
	if status != "" && status != types.SessionStatusActive && status != types.SessionStatusArchived {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid session status"})
		return
	}
	filter := store.ListSessionFilter{TenantID: principalFromGin(c).TenantID, Status: status}
	if value, err := strconv.Atoi(c.Query("limit")); err == nil && value > 0 {
		filter.Limit = value
	}
	if value, err := strconv.Atoi(c.Query("offset")); err == nil && value >= 0 {
		filter.Offset = value
	}
	items, err := sessions.ListSessions(c.Request.Context(), filter)
	if err != nil {
		c.Error(err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"sessions": items, "count": len(items), "limit": filter.Limit, "offset": filter.Offset})
}

func (h *Handler) updateSession(c *gin.Context) {
	sessions, ok := h.sessionStore(c)
	if !ok {
		return
	}
	session, err := sessions.GetSession(c.Request.Context(), c.Param("id"), principalFromGin(c).TenantID)
	if errors.Is(err, store.ErrSessionNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}
	if err != nil {
		c.Error(err)
		return
	}
	var req updateSessionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.Title != nil {
		title := strings.TrimSpace(*req.Title)
		if title == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "session title must not be empty"})
			return
		}
		if len([]rune(title)) > 200 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "session title exceeds 200 characters"})
			return
		}
		session.Title = title
	}
	if req.Status != nil {
		if *req.Status != types.SessionStatusActive && *req.Status != types.SessionStatusArchived {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid session status"})
			return
		}
		session.Status = *req.Status
	}
	if err := sessions.UpdateSession(c.Request.Context(), session); err != nil {
		c.Error(err)
		return
	}
	c.JSON(http.StatusOK, session)
}

func (h *Handler) archiveSession(c *gin.Context) {
	sessions, ok := h.sessionStore(c)
	if !ok {
		return
	}
	session, err := sessions.GetSession(c.Request.Context(), c.Param("id"), principalFromGin(c).TenantID)
	if errors.Is(err, store.ErrSessionNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}
	if err != nil {
		c.Error(err)
		return
	}
	session.Status = types.SessionStatusArchived
	if err := sessions.UpdateSession(c.Request.Context(), session); err != nil {
		c.Error(err)
		return
	}
	c.JSON(http.StatusOK, session)
}

func (h *Handler) listSessionTasks(c *gin.Context) {
	sessions, ok := h.sessionStore(c)
	if !ok {
		return
	}
	if _, err := sessions.GetSession(c.Request.Context(), c.Param("id"), principalFromGin(c).TenantID); errors.Is(err, store.ErrSessionNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	} else if err != nil {
		c.Error(err)
		return
	}
	filter := store.ListFilter{TenantID: principalFromGin(c).TenantID, SessionID: c.Param("id")}
	if status := c.Query("status"); status != "" {
		filter.Status = types.TaskStatus(status)
	}
	if value, err := strconv.Atoi(c.Query("limit")); err == nil && value > 0 {
		filter.Limit = value
	}
	if value, err := strconv.Atoi(c.Query("offset")); err == nil && value >= 0 {
		filter.Offset = value
	}
	items, err := h.store.ListTasks(c.Request.Context(), filter)
	if err != nil {
		c.Error(err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"tasks": items, "count": len(items), "limit": filter.Limit, "offset": filter.Offset})
}

func (h *Handler) listSessionMemories(c *gin.Context) {
	manager, ok := h.store.(store.MemoryManagementStore)
	if !ok {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "memory management is not supported"})
		return
	}
	if sessions, ok := h.sessionStore(c); !ok {
		return
	} else if _, err := sessions.GetSession(c.Request.Context(), c.Param("id"), principalFromGin(c).TenantID); errors.Is(err, store.ErrSessionNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	} else if err != nil {
		c.Error(err)
		return
	}
	filter := store.ListMemoryFilter{TenantID: principalFromGin(c).TenantID, SessionID: c.Param("id")}
	if value, err := strconv.Atoi(c.Query("limit")); err == nil && value > 0 {
		filter.Limit = value
	}
	if value, err := strconv.Atoi(c.Query("offset")); err == nil && value >= 0 {
		filter.Offset = value
	}
	items, err := manager.ListMemories(c.Request.Context(), filter)
	if err != nil {
		c.Error(err)
		return
	}
	response := make([]memoryListItem, 0, len(items))
	for _, mem := range items {
		response = append(response, memoryListItem{ID: mem.ID, TenantID: mem.TenantID, SessionID: mem.SessionID, TaskID: mem.TaskID, Goal: mem.Goal, FinalAnswer: mem.FinalAnswer, KeyFindings: mem.KeyFindings, Timestamp: mem.Timestamp, EmbeddingDimensions: len(mem.Embedding)})
	}
	c.JSON(http.StatusOK, gin.H{"memories": response, "count": len(response), "limit": filter.Limit, "offset": filter.Offset})
}
