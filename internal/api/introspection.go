package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/policy"
)

const (
	maxIntrospectionResponseBytes = 64 << 10
	maxIntrospectionCacheEntries  = 1024
)

var (
	errInvalidCredential         = errors.New("invalid credential")
	errAuthenticationUnavailable = errors.New("authentication service unavailable")
)

type cachedIntrospection struct {
	tenantID string
	expires  time.Time
}

type introspectionVerifier struct {
	config config.APIIntrospectionConfig
	client *http.Client

	mu    sync.Mutex
	cache map[[sha256.Size]byte]cachedIntrospection
}

var introspectionVerifierState struct {
	sync.Mutex
	config   config.APIIntrospectionConfig
	verifier *introspectionVerifier
}

func introspectorFor(introspectionConfig config.APIIntrospectionConfig) *introspectionVerifier {
	introspectionVerifierState.Lock()
	defer introspectionVerifierState.Unlock()
	if introspectionVerifierState.verifier != nil && reflect.DeepEqual(introspectionVerifierState.config, introspectionConfig) {
		return introspectionVerifierState.verifier
	}
	client := policy.ConfiguredHTTPClient(
		time.Duration(introspectionConfig.TimeoutSeconds)*time.Second,
		introspectionConfig.AllowPrivateNetwork,
	)
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	verifier := &introspectionVerifier{
		config: introspectionConfig,
		client: client,
		cache:  make(map[[sha256.Size]byte]cachedIntrospection),
	}
	introspectionVerifierState.config = introspectionConfig
	introspectionVerifierState.verifier = verifier
	return verifier
}

func (v *introspectionVerifier) verify(ctx context.Context, rawToken string) (string, error) {
	tokenHash := sha256.Sum256([]byte(rawToken))
	if tenantID, ok := v.cachedTenant(tokenHash); ok {
		return tenantID, nil
	}

	requestCtx, cancel := context.WithTimeout(ctx, time.Duration(v.config.TimeoutSeconds)*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, v.config.URL, nil)
	if err != nil {
		return "", fmt.Errorf("%w: build introspection request", errAuthenticationUnavailable)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+rawToken)

	response, err := v.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: token introspection request failed", errAuthenticationUnavailable)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return "", fmt.Errorf("%w: token introspection returned HTTP %d", errAuthenticationUnavailable, response.StatusCode)
	}

	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxIntrospectionResponseBytes+1))
	if err != nil || len(responseBody) > maxIntrospectionResponseBytes {
		return "", fmt.Errorf("%w: invalid introspection response size", errAuthenticationUnavailable)
	}
	decoder := json.NewDecoder(bytes.NewReader(responseBody))
	decoder.UseNumber()
	claims := make(map[string]any)
	if err := decoder.Decode(&claims); err != nil {
		return "", fmt.Errorf("%w: invalid introspection response", errAuthenticationUnavailable)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return "", fmt.Errorf("%w: invalid introspection response", errAuthenticationUnavailable)
	}
	if active, ok := introspectionClaim(claims, v.config.ActiveClaim).(bool); !ok || !active {
		return "", errInvalidCredential
	}
	tenantID, ok := introspectionClaim(claims, v.config.TenantClaim).(string)
	tenantID = strings.TrimSpace(tenantID)
	if !ok || !tenantIDPattern.MatchString(tenantID) {
		return "", errInvalidCredential
	}
	if v.config.Issuer != "" {
		issuer, _ := claims["iss"].(string)
		if issuer != v.config.Issuer {
			return "", errInvalidCredential
		}
	}
	if v.config.Audience != "" && !introspectionAudienceContains(claims["aud"], v.config.Audience) {
		return "", errInvalidCredential
	}
	expiresAt, hasExpiration, validExpiration := introspectionExpiration(claims["exp"])
	if !validExpiration || (v.config.RequireExpiration && !hasExpiration) || (hasExpiration && !expiresAt.After(time.Now())) {
		return "", errInvalidCredential
	}
	v.cacheTenant(tokenHash, tenantID, expiresAt, hasExpiration)
	return tenantID, nil
}

// introspectionClaim resolves a dot-separated JSON object path such as
// "data.userid". A direct top-level key match takes precedence so existing
// providers with dots in their literal claim names remain compatible.
func introspectionClaim(claims map[string]any, path string) any {
	if value, ok := claims[path]; ok {
		return value
	}
	segments := strings.Split(path, ".")
	var current any = claims
	for _, segment := range segments {
		object, ok := current.(map[string]any)
		if !ok || segment == "" {
			return nil
		}
		current, ok = object[segment]
		if !ok {
			return nil
		}
	}
	return current
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func introspectionAudienceContains(raw any, expected string) bool {
	switch audience := raw.(type) {
	case string:
		return audience == expected
	case []any:
		for _, item := range audience {
			if value, ok := item.(string); ok && value == expected {
				return true
			}
		}
	}
	return false
}

func introspectionExpiration(raw any) (time.Time, bool, bool) {
	if raw == nil {
		return time.Time{}, false, true
	}
	number, ok := raw.(json.Number)
	if !ok {
		return time.Time{}, true, false
	}
	seconds, err := number.Int64()
	if err != nil || seconds <= 0 {
		return time.Time{}, true, false
	}
	return time.Unix(seconds, 0), true, true
}

func (v *introspectionVerifier) cachedTenant(tokenHash [sha256.Size]byte) (string, bool) {
	if v.config.CacheTTLSeconds <= 0 {
		return "", false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	entry, ok := v.cache[tokenHash]
	if !ok {
		return "", false
	}
	if !time.Now().Before(entry.expires) {
		delete(v.cache, tokenHash)
		return "", false
	}
	return entry.tenantID, true
}

func (v *introspectionVerifier) cacheTenant(tokenHash [sha256.Size]byte, tenantID string, tokenExpiration time.Time, hasExpiration bool) {
	if v.config.CacheTTLSeconds <= 0 {
		return
	}
	expires := time.Now().Add(time.Duration(v.config.CacheTTLSeconds) * time.Second)
	if hasExpiration && tokenExpiration.Before(expires) {
		expires = tokenExpiration
	}
	if !expires.After(time.Now()) {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(v.cache) >= maxIntrospectionCacheEntries {
		v.evictCacheEntryLocked()
	}
	v.cache[tokenHash] = cachedIntrospection{tenantID: tenantID, expires: expires}
}

func (v *introspectionVerifier) evictCacheEntryLocked() {
	now := time.Now()
	var oldestHash [sha256.Size]byte
	var oldestExpiry time.Time
	hasOldest := false
	for tokenHash, entry := range v.cache {
		if !entry.expires.After(now) {
			delete(v.cache, tokenHash)
			return
		}
		if !hasOldest || entry.expires.Before(oldestExpiry) {
			oldestHash, oldestExpiry, hasOldest = tokenHash, entry.expires, true
		}
	}
	if hasOldest {
		delete(v.cache, oldestHash)
	}
}
