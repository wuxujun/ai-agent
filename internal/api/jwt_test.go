package api

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/wuxujun/ai-agent/internal/config"
)

type jwtRoundTripFunc func(*http.Request) (*http.Response, error)

func (f jwtRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestJWTAuthMapsVerifiedCodeToTenant(t *testing.T) {
	gin.SetMode(gin.TestMode)
	privateKey := mustRSAKey(t)
	jwtConfig := testJWTConfig()
	var jwksRequests atomic.Int32
	installTestJWTVerifier(jwtConfig, privateKey.PublicKey, &jwksRequests)
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.API.APIKey = "admin-static-key"
		cfg.API.Auth = config.APIAuthConfig{Mode: "hybrid", JWT: jwtConfig}
		cfg.API.Tenants = map[string]config.APITenantConfig{
			"project_a": {Admin: false},
		}
	}))

	router := gin.New()
	router.Use(AuthMiddleware())
	router.GET("/", func(c *gin.Context) {
		principal := principalFromGin(c)
		c.JSON(http.StatusOK, gin.H{"tenant_id": principal.TenantID, "admin": principal.Admin})
	})

	token := signJWT(t, privateKey, jwt.MapClaims{
		"iss":  jwtConfig.Issuer,
		"aud":  jwtConfig.Audience,
		"exp":  time.Now().Add(time.Minute).Unix(),
		"nbf":  time.Now().Add(-time.Second).Unix(),
		"code": "project_a",
	})
	response := performJWTRequest(router, token)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"tenant_id":"project_a"`) || !strings.Contains(response.Body.String(), `"admin":false`) {
		t.Fatalf("verified JWT response = %d %s", response.Code, response.Body.String())
	}
	response = performJWTRequest(router, token)
	if response.Code != http.StatusOK || jwksRequests.Load() != 1 {
		t.Fatalf("JWKS cache response=%d requests=%d", response.Code, jwksRequests.Load())
	}

	staticRequest := httptest.NewRequest(http.MethodGet, "/", nil)
	staticRequest.Header.Set("X-API-Key", "admin-static-key")
	staticResponse := httptest.NewRecorder()
	router.ServeHTTP(staticResponse, staticRequest)
	if staticResponse.Code != http.StatusOK || !strings.Contains(staticResponse.Body.String(), `"tenant_id":"default"`) || !strings.Contains(staticResponse.Body.String(), `"admin":true`) {
		t.Fatalf("hybrid static key response = %d %s", staticResponse.Code, staticResponse.Body.String())
	}
}

func TestJWTAuthRejectsUntrustedClaims(t *testing.T) {
	gin.SetMode(gin.TestMode)
	privateKey := mustRSAKey(t)
	otherKey := mustRSAKey(t)
	jwtConfig := testJWTConfig()
	var jwksRequests atomic.Int32
	installTestJWTVerifier(jwtConfig, privateKey.PublicKey, &jwksRequests)
	t.Cleanup(config.OverrideForTesting(func(cfg *config.Config) {
		cfg.API.APIKey = ""
		cfg.API.Auth = config.APIAuthConfig{Mode: "jwt", JWT: jwtConfig}
		cfg.API.Tenants = map[string]config.APITenantConfig{"project_a": {}}
	}))

	router := gin.New()
	router.Use(AuthMiddleware())
	router.GET("/", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	validClaims := func() jwt.MapClaims {
		return jwt.MapClaims{
			"iss": jwtConfig.Issuer, "aud": jwtConfig.Audience,
			"exp": time.Now().Add(time.Minute).Unix(), "code": "project_a",
		}
	}
	tests := []struct {
		name   string
		key    *rsa.PrivateKey
		kid    string
		mutate func(jwt.MapClaims)
	}{
		{name: "wrong signature", key: otherKey},
		{name: "unknown key id", key: privateKey, kid: "attacker-controlled-kid"},
		{name: "expired", key: privateKey, mutate: func(claims jwt.MapClaims) { claims["exp"] = time.Now().Add(-time.Minute).Unix() }},
		{name: "wrong issuer", key: privateKey, mutate: func(claims jwt.MapClaims) { claims["iss"] = "https://attacker.example" }},
		{name: "wrong audience", key: privateKey, mutate: func(claims jwt.MapClaims) { claims["aud"] = "other-service" }},
		{name: "missing code", key: privateKey, mutate: func(claims jwt.MapClaims) { delete(claims, "code") }},
		{name: "unknown tenant", key: privateKey, mutate: func(claims jwt.MapClaims) { claims["code"] = "project_b" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			claims := validClaims()
			if tt.mutate != nil {
				tt.mutate(claims)
			}
			response := performJWTRequest(router, signJWTWithKeyID(t, tt.key, claims, tt.kid))
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("response = %d %s", response.Code, response.Body.String())
			}
		})
	}
	if jwksRequests.Load() != 1 {
		t.Fatalf("unknown key IDs triggered excessive JWKS refreshes: %d", jwksRequests.Load())
	}
}

func testJWTConfig() config.APIJWTConfig {
	return config.APIJWTConfig{
		Issuer:                    "https://issuer.example",
		Audience:                  "ai-agent",
		JWKSURL:                   "https://issuer.example/.well-known/jwks.json",
		TenantClaim:               "code",
		AllowedAlgorithms:         []string{"RS256"},
		RequireKnownTenant:        true,
		ClockSkewSeconds:          0,
		JWKSCacheTTLSeconds:       300,
		JWKSRequestTimeoutSeconds: 2,
	}
}

func installTestJWTVerifier(jwtConfig config.APIJWTConfig, publicKey rsa.PublicKey, requests *atomic.Int32) {
	modulus := base64.RawURLEncoding.EncodeToString(publicKey.N.Bytes())
	exponent := base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1})
	body, _ := json.Marshal(map[string]any{"keys": []map[string]string{{
		"kid": "test-key", "kty": "RSA", "use": "sig", "alg": "RS256", "n": modulus, "e": exponent,
	}}})
	client := &http.Client{Transport: jwtRoundTripFunc(func(*http.Request) (*http.Response, error) {
		if requests != nil {
			requests.Add(1)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(string(body))),
		}, nil
	})}
	jwtVerifierState.Lock()
	jwtVerifierState.config = jwtConfig
	jwtVerifierState.verifier = &jwtVerifier{config: jwtConfig, client: client}
	jwtVerifierState.Unlock()
}

func mustRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func signJWT(t *testing.T, key *rsa.PrivateKey, claims jwt.MapClaims) string {
	return signJWTWithKeyID(t, key, claims, "test-key")
}

func signJWTWithKeyID(t *testing.T, key *rsa.PrivateKey, claims jwt.MapClaims, keyID string) string {
	t.Helper()
	if keyID == "" {
		keyID = "test-key"
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = keyID
	signed, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func performJWTRequest(router http.Handler, token string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}
