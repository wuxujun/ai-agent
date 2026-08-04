package api

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/telemetry"
)

const (
	maxJWKSResponseBytes   = 1 << 20
	minJWKSRefreshInterval = 5 * time.Second
)

var tenantIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

type cachedJWTKey struct {
	key       *rsa.PublicKey
	algorithm string
}

type jwtVerifier struct {
	config config.APIJWTConfig
	client *http.Client

	mu          sync.Mutex
	keys        map[string]cachedJWTKey
	expiresAt   time.Time
	refreshedAt time.Time
}

var jwtVerifierState struct {
	sync.Mutex
	config   config.APIJWTConfig
	verifier *jwtVerifier
}

func verifierFor(jwtConfig config.APIJWTConfig) *jwtVerifier {
	jwtVerifierState.Lock()
	defer jwtVerifierState.Unlock()
	if jwtVerifierState.verifier != nil && reflect.DeepEqual(jwtVerifierState.config, jwtConfig) {
		return jwtVerifierState.verifier
	}
	verifier := &jwtVerifier{
		config: jwtConfig,
		client: telemetry.NewHTTPClient(time.Duration(jwtConfig.JWKSRequestTimeoutSeconds) * time.Second),
	}
	// The JWKS location is operator-configured. Do not let a compromised issuer
	// redirect this server to an internal metadata or control-plane endpoint.
	verifier.client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	jwtVerifierState.config = jwtConfig
	jwtVerifierState.verifier = verifier
	return verifier
}

func (v *jwtVerifier) verify(ctx context.Context, rawToken string) (string, error) {
	allowedAlgorithms := make([]string, 0, len(v.config.AllowedAlgorithms))
	for _, algorithm := range v.config.AllowedAlgorithms {
		allowedAlgorithms = append(allowedAlgorithms, strings.ToUpper(strings.TrimSpace(algorithm)))
	}
	claims := jwt.MapClaims{}
	options := []jwt.ParserOption{
		jwt.WithValidMethods(allowedAlgorithms),
		jwt.WithIssuer(strings.TrimSpace(v.config.Issuer)),
		jwt.WithAudience(strings.TrimSpace(v.config.Audience)),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(time.Duration(v.config.ClockSkewSeconds) * time.Second),
	}
	token, err := jwt.ParseWithClaims(rawToken, claims, func(token *jwt.Token) (any, error) {
		if _, ok := token.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, fmt.Errorf("unexpected JWT signing method %q", token.Method.Alg())
		}
		return v.keyFor(ctx, token)
	}, options...)
	if err != nil || token == nil || !token.Valid {
		return "", errors.New("JWT validation failed")
	}
	tenantID, ok := claims[strings.TrimSpace(v.config.TenantClaim)].(string)
	tenantID = strings.TrimSpace(tenantID)
	if !ok || !tenantIDPattern.MatchString(tenantID) {
		return "", errors.New("JWT tenant claim is missing or invalid")
	}
	return tenantID, nil
}

func (v *jwtVerifier) keyFor(ctx context.Context, token *jwt.Token) (*rsa.PublicKey, error) {
	kid, _ := token.Header["kid"].(string)
	kid = strings.TrimSpace(kid)
	keys, err := v.cachedKeys(ctx, false)
	if err != nil {
		return nil, err
	}
	key, ok := selectJWTKey(keys, kid)
	if !ok {
		// A previously unseen key ID commonly means the issuer rotated its key.
		keys, err = v.cachedKeys(ctx, true)
		if err != nil {
			return nil, err
		}
		key, ok = selectJWTKey(keys, kid)
	}
	if !ok {
		return nil, errors.New("JWT signing key not found")
	}
	if key.algorithm != "" && key.algorithm != token.Method.Alg() {
		return nil, errors.New("JWT key algorithm does not match token algorithm")
	}
	return key.key, nil
}

func selectJWTKey(keys map[string]cachedJWTKey, kid string) (cachedJWTKey, bool) {
	if kid != "" {
		key, ok := keys[kid]
		return key, ok
	}
	if len(keys) != 1 {
		return cachedJWTKey{}, false
	}
	for _, key := range keys {
		return key, true
	}
	return cachedJWTKey{}, false
}

func (v *jwtVerifier) cachedKeys(ctx context.Context, forceRefresh bool) (map[string]cachedJWTKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !forceRefresh && len(v.keys) > 0 && time.Now().Before(v.expiresAt) {
		return v.keys, nil
	}
	// Unknown-kid requests are unauthenticated input. Bound forced refreshes so
	// an attacker cannot turn arbitrary JWT headers into a JWKS request flood.
	if forceRefresh && len(v.keys) > 0 && time.Since(v.refreshedAt) < minJWKSRefreshInterval {
		return v.keys, nil
	}
	keys, err := v.fetchKeys(ctx)
	if err != nil {
		return nil, err
	}
	v.keys = keys
	v.refreshedAt = time.Now()
	v.expiresAt = v.refreshedAt.Add(time.Duration(v.config.JWKSCacheTTLSeconds) * time.Second)
	return v.keys, nil
}

func (v *jwtVerifier) fetchKeys(ctx context.Context) (map[string]cachedJWTKey, error) {
	requestCtx, cancel := context.WithTimeout(ctx, time.Duration(v.config.JWKSRequestTimeoutSeconds)*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, v.config.JWKSURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build JWKS request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	response, err := v.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch JWKS: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return nil, fmt.Errorf("fetch JWKS: HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxJWKSResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read JWKS: %w", err)
	}
	if len(body) > maxJWKSResponseBytes {
		return nil, errors.New("JWKS response is too large")
	}
	var document struct {
		Keys []struct {
			KeyID         string   `json:"kid"`
			KeyType       string   `json:"kty"`
			Use           string   `json:"use"`
			KeyOperations []string `json:"key_ops"`
			Algorithm     string   `json:"alg"`
			Modulus       string   `json:"n"`
			Exponent      string   `json:"e"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(body, &document); err != nil {
		return nil, fmt.Errorf("decode JWKS: %w", err)
	}
	keys := make(map[string]cachedJWTKey)
	for _, item := range document.Keys {
		if item.KeyType != "RSA" || (item.Use != "" && item.Use != "sig") || !allowsJWTVerification(item.KeyOperations) {
			continue
		}
		keyID := strings.TrimSpace(item.KeyID)
		if keyID == "" {
			continue
		}
		publicKey, err := rsaPublicKey(item.Modulus, item.Exponent)
		if err != nil {
			return nil, fmt.Errorf("decode JWKS key %q: %w", keyID, err)
		}
		if _, exists := keys[keyID]; exists {
			return nil, fmt.Errorf("JWKS contains duplicate key ID %q", keyID)
		}
		keys[keyID] = cachedJWTKey{key: publicKey, algorithm: strings.TrimSpace(item.Algorithm)}
	}
	if len(keys) == 0 {
		return nil, errors.New("JWKS contains no usable RSA signing keys")
	}
	return keys, nil
}

func allowsJWTVerification(operations []string) bool {
	if len(operations) == 0 {
		return true
	}
	for _, operation := range operations {
		if operation == "verify" {
			return true
		}
	}
	return false
}

func rsaPublicKey(encodedModulus, encodedExponent string) (*rsa.PublicKey, error) {
	modulusBytes, err := base64.RawURLEncoding.DecodeString(encodedModulus)
	if err != nil || len(modulusBytes) == 0 {
		return nil, errors.New("invalid RSA modulus")
	}
	exponentBytes, err := base64.RawURLEncoding.DecodeString(encodedExponent)
	if err != nil || len(exponentBytes) == 0 || len(exponentBytes) > 4 {
		return nil, errors.New("invalid RSA exponent")
	}
	exponent := 0
	for _, value := range exponentBytes {
		exponent = exponent<<8 | int(value)
	}
	if exponent < 3 || exponent%2 == 0 {
		return nil, errors.New("invalid RSA exponent")
	}
	modulus := new(big.Int).SetBytes(modulusBytes)
	if modulus.BitLen() < 2048 {
		return nil, errors.New("RSA signing key must be at least 2048 bits")
	}
	return &rsa.PublicKey{N: modulus, E: exponent}, nil
}
