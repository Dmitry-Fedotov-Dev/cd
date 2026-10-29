// Package oidc verifies the short-lived identity tokens that CI platforms issue to a job
// (GitHub Actions, GitLab CI). Only the standard library: RS256, JWKS discovery, a key cache.
package oidc

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// Claims are the token's payload. Values are kept as JSON allows (string, number, bool).
type Claims map[string]any

// String returns a claim as text ("" when absent); numbers and booleans are formatted.
func (c Claims) String(name string) string {
	switch v := c[name].(type) {
	case string:
		return v
	case float64:
		return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%f", v), "0"), ".")
	case bool:
		if v {
			return "true"
		}
		return "false"
	}
	return ""
}

// Verifier checks tokens from a fixed set of issuers for one audience.
type Verifier struct {
	Issuers  []string // allowed "iss" values, exactly as the platform writes them
	Audience string   // the deploy server's public URL; tokens for other servers are rejected
	Client   *http.Client
	Leeway   time.Duration
	Now      func() time.Time

	mu    sync.Mutex
	keys  map[string]map[string]*rsa.PublicKey // issuer -> kid -> key
	fetch map[string]time.Time                 // issuer -> last JWKS fetch
}

var (
	ErrMalformed = errors.New("malformed token")
	ErrIssuer    = errors.New("issuer not allowed")
	ErrAudience  = errors.New("token is for another audience")
	ErrExpired   = errors.New("token expired or not yet valid")
	ErrSignature = errors.New("bad signature")
)

const keysTTL = time.Hour

// Verify checks signature, issuer, audience and lifetime, and returns the claims.
func (v *Verifier) Verify(ctx context.Context, raw string) (Claims, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, ErrMalformed
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodeSegment(parts[0], &header); err != nil {
		return nil, ErrMalformed
	}
	if header.Alg != "RS256" {
		return nil, fmt.Errorf("%w: alg %q", ErrMalformed, header.Alg)
	}
	var claims Claims
	if err := decodeSegment(parts[1], &claims); err != nil {
		return nil, ErrMalformed
	}
	iss := claims.String("iss")
	if !slices.Contains(v.Issuers, iss) {
		return nil, fmt.Errorf("%w: %q", ErrIssuer, iss)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, ErrMalformed
	}
	key, err := v.key(ctx, iss, header.Kid)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], sig) != nil {
		return nil, ErrSignature
	}
	if !audienceMatches(claims["aud"], v.Audience) {
		return nil, ErrAudience
	}
	if err := v.checkTime(claims); err != nil {
		return nil, err
	}
	return claims, nil
}

func audienceMatches(aud any, want string) bool {
	want = strings.TrimRight(want, "/")
	switch a := aud.(type) {
	case string:
		return strings.TrimRight(a, "/") == want
	case []any:
		for _, x := range a {
			if s, ok := x.(string); ok && strings.TrimRight(s, "/") == want {
				return true
			}
		}
	}
	return false
}

func (v *Verifier) checkTime(c Claims) error {
	now := time.Now
	if v.Now != nil {
		now = v.Now
	}
	leeway := v.Leeway
	if leeway == 0 {
		leeway = time.Minute
	}
	t := now()
	exp, ok := c["exp"].(float64)
	if !ok || t.After(time.Unix(int64(exp), 0).Add(leeway)) {
		return ErrExpired
	}
	if nbf, ok := c["nbf"].(float64); ok && t.Add(leeway).Before(time.Unix(int64(nbf), 0)) {
		return ErrExpired
	}
	return nil
}

func (v *Verifier) key(ctx context.Context, iss, kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.keys == nil {
		v.keys = map[string]map[string]*rsa.PublicKey{}
		v.fetch = map[string]time.Time{}
	}
	last := v.fetch[iss]
	k, ok := v.keys[iss][kid]
	stale := time.Since(last) > keysTTL
	// unknown kid: the platform may have rotated keys; refetch, but not more than once a minute
	if !ok && time.Since(last) > time.Minute || stale {
		keys, err := v.loadJWKS(ctx, iss)
		if err != nil {
			if ok {
				return k, nil // keep serving the cached key if the platform is briefly unreachable
			}
			return nil, err
		}
		v.keys[iss], v.fetch[iss] = keys, time.Now()
		k, ok = keys[kid]
	}
	if !ok {
		return nil, fmt.Errorf("%w: unknown key id %q", ErrSignature, kid)
	}
	return k, nil
}

func (v *Verifier) loadJWKS(ctx context.Context, iss string) (map[string]*rsa.PublicKey, error) {
	var disc struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := v.getJSON(ctx, strings.TrimRight(iss, "/")+"/.well-known/openid-configuration", &disc); err != nil {
		return nil, fmt.Errorf("oidc discovery for %s: %w", iss, err)
	}
	if disc.JWKSURI == "" {
		return nil, fmt.Errorf("oidc discovery for %s: no jwks_uri", iss)
	}
	var set struct {
		Keys []struct {
			Kty, Kid, N, E string
		} `json:"keys"`
	}
	if err := v.getJSON(ctx, disc.JWKSURI, &set); err != nil {
		return nil, fmt.Errorf("jwks for %s: %w", iss, err)
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil || len(e) > 4 {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("jwks for %s: no RSA keys", iss)
	}
	return keys, nil
}

func (v *Verifier) getJSON(ctx context.Context, url string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	client := v.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

func decodeSegment(s string, out any) error {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}
