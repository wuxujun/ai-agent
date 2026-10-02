package api

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wuxujun/ai-agent/internal/config"
)

const consoleCookieName = "ai_agent_console"
const consoleSessionLifetime = 8 * time.Hour

type consoleSession struct {
	Token     string    `json:"token"`
	TenantID  string    `json:"tenant_id"`
	CSRFToken string    `json:"csrf_token"`
	ExpiresAt time.Time `json:"expires_at"`
}

func consoleAEAD() (cipher.AEAD, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(os.Getenv("AI_AGENT_CONSOLE_SESSION_KEY")))
	if err != nil || len(key) != 32 {
		return nil, errors.New("console session key is unavailable")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func encryptConsoleSession(session consoleSession) (string, error) {
	aead, err := consoleAEAD()
	if err != nil {
		return "", err
	}
	plain, err := json.Marshal(session)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := aead.Seal(nonce, nonce, plain, nil)
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

func readConsoleSession(c *gin.Context) (consoleSession, error) {
	var session consoleSession
	cookie, err := c.Cookie(consoleCookieName)
	if err != nil || len(cookie) > 4096 {
		return session, errors.New("console session is missing")
	}
	aead, err := consoleAEAD()
	if err != nil {
		return session, err
	}
	sealed, err := base64.RawURLEncoding.DecodeString(cookie)
	if err != nil || len(sealed) <= aead.NonceSize() {
		return session, errors.New("console session is invalid")
	}
	plain, err := aead.Open(nil, sealed[:aead.NonceSize()], sealed[aead.NonceSize():], nil)
	if err != nil || json.Unmarshal(plain, &session) != nil || session.Token == "" || session.TenantID == "" || session.CSRFToken == "" || !session.ExpiresAt.After(time.Now()) {
		return consoleSession{}, errors.New("console session is invalid")
	}
	return session, nil
}

func sameOriginRequest(c *gin.Context) bool {
	origin := c.GetHeader("Origin")
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	return err == nil && parsed.Host == c.Request.Host && (parsed.Scheme == "https" || parsed.Scheme == "http") && parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == ""
}

func randomConsoleToken() (string, error) {
	bytes := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, bytes); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}

func setConsoleCookie(c *gin.Context, value string, maxAge int) {
	secure := true
	if c.Request.TLS == nil && consoleLoopbackAddress(c.Request.Host) && consoleLoopbackAddress(c.Request.RemoteAddr) {
		secure = false // Local development over HTTP only.
	}
	http.SetCookie(c.Writer, &http.Cookie{
		Name: consoleCookieName, Value: value, Path: "/", MaxAge: maxAge,
		Secure: secure, HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
}

func consoleLoopbackAddress(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	return strings.EqualFold(host, "localhost") || (net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback())
}

func createConsoleSession(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	if !sameOriginRequest(c) {
		c.JSON(http.StatusForbidden, gin.H{"error": "invalid request origin"})
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	if c.ContentType() != "application/json" || c.ShouldBindJSON(&body) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "JSON token is required"})
		return
	}
	token := strings.TrimSpace(body.Token)
	if token == "" || len(token) > 2500 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid token length"})
		return
	}
	cfg := config.Get()
	authMode := strings.ToLower(strings.TrimSpace(cfg.API.Auth.Mode))
	if authMode != "jwt" && authMode != "introspection" && authMode != "hybrid" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "console requires Bearer authentication"})
		return
	}
	tenantID, requireKnownTenant, err := verifyBearerCredential(c.Request.Context(), cfg, authMode, token)
	if err != nil || (requireKnownTenant && !knownConsoleTenant(cfg, tenantID)) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credential"})
		return
	}
	csrfToken, err := randomConsoleToken()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "session creation failed"})
		return
	}
	session := consoleSession{Token: token, TenantID: tenantID, CSRFToken: csrfToken, ExpiresAt: time.Now().Add(consoleSessionLifetime)}
	cookie, err := encryptConsoleSession(session)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "console session is not configured"})
		return
	}
	if len(cookie) > 3900 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "credential is too large for a browser session"})
		return
	}
	setConsoleCookie(c, cookie, int(consoleSessionLifetime.Seconds()))
	c.JSON(http.StatusOK, gin.H{"tenant_id": tenantID, "csrf_token": csrfToken, "expires_at": session.ExpiresAt})
}

func knownConsoleTenant(cfg *config.Config, tenantID string) bool {
	_, ok := cfg.API.Tenants[tenantID]
	return ok
}

func getConsoleSession(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	session, err := readConsoleSession(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "console session is unavailable"})
		return
	}
	cfg := config.Get()
	mode := strings.ToLower(strings.TrimSpace(cfg.API.Auth.Mode))
	tenantID, requireKnownTenant, err := verifyBearerCredential(c.Request.Context(), cfg, mode, session.Token)
	if err != nil || tenantID != session.TenantID || (requireKnownTenant && !knownConsoleTenant(cfg, tenantID)) {
		setConsoleCookie(c, "", -1)
		c.JSON(http.StatusUnauthorized, gin.H{"error": "console session has expired"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"tenant_id": tenantID, "csrf_token": session.CSRFToken, "expires_at": session.ExpiresAt})
}

func deleteConsoleSession(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	if !sameOriginRequest(c) {
		c.JSON(http.StatusForbidden, gin.H{"error": "invalid request origin"})
		return
	}
	session, err := readConsoleSession(c)
	if err == nil && c.GetHeader("X-Console-CSRF") != session.CSRFToken {
		c.JSON(http.StatusForbidden, gin.H{"error": "invalid CSRF token"})
		return
	}
	setConsoleCookie(c, "", -1)
	c.Status(http.StatusNoContent)
}

// ConsoleSessionMiddleware lets the existing API authentication validate the
// bearer on every request. Cookie auth also requires a CSRF token for writes.
func ConsoleSessionMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.GetHeader("Authorization") != "" || c.GetHeader("X-API-Key") != "" {
			c.Next()
			return
		}
		session, err := readConsoleSession(c)
		if err != nil {
			c.Next()
			return
		}
		if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead && c.Request.Method != http.MethodOptions {
			if !sameOriginRequest(c) || c.GetHeader("X-Console-CSRF") != session.CSRFToken {
				c.JSON(http.StatusForbidden, gin.H{"error": "invalid console CSRF token"})
				c.Abort()
				return
			}
		}
		c.Request.Header.Set("Authorization", "Bearer "+session.Token)
		c.Header("Cache-Control", "no-store")
		c.Next()
	}
}
