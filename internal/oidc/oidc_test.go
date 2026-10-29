package oidc

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// fakeIssuer serves discovery + JWKS for one RSA key and signs tokens with it.
type fakeIssuer struct {
	srv *httptest.Server
	key *rsa.PrivateKey
}

func newIssuer(t *testing.T) *fakeIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIssuer{key: key}
	mux := http.NewServeMux()
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"jwks_uri": f.srv.URL + "/jwks"})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": "k1",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	return f
}

func (f *fakeIssuer) sign(t *testing.T, claims map[string]any) string {
	t.Helper()
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	head := enc(map[string]string{"alg": "RS256", "kid": "k1", "typ": "JWT"})
	body := enc(claims)
	sum := sha256.Sum256([]byte(head + "." + body))
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return head + "." + body + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func TestVerify(t *testing.T) {
	iss := newIssuer(t)
	v := &Verifier{Issuers: []string{iss.srv.URL}, Audience: "https://deploy.example.com"}
	now := time.Now().Unix()
	good := map[string]any{"iss": iss.srv.URL, "aud": "https://deploy.example.com", "exp": now + 300,
		"repository": "me/app", "ref": "refs/heads/main"}

	c, err := v.Verify(context.Background(), iss.sign(t, good))
	if err != nil {
		t.Fatalf("good token: %v", err)
	}
	if c.String("repository") != "me/app" {
		t.Fatalf("claims: %v", c)
	}

	cases := map[string]struct {
		mut  func(map[string]any)
		want error
	}{
		"other audience": {func(m map[string]any) { m["aud"] = "https://evil.example.com" }, ErrAudience},
		"expired":        {func(m map[string]any) { m["exp"] = now - 3600 }, ErrExpired},
		"other issuer":   {func(m map[string]any) { m["iss"] = "https://evil.example.com" }, ErrIssuer},
		"no exp":         {func(m map[string]any) { delete(m, "exp") }, ErrExpired},
	}
	for name, tc := range cases {
		claims := map[string]any{}
		for k, val := range good {
			claims[k] = val
		}
		tc.mut(claims)
		if _, err := v.Verify(context.Background(), iss.sign(t, claims)); !errors.Is(err, tc.want) {
			t.Errorf("%s: got %v, want %v", name, err, tc.want)
		}
	}

	// payload swapped after signing
	tok := iss.sign(t, good)
	forged := map[string]any{}
	for k, val := range good {
		forged[k] = val
	}
	forged["repository"] = "evil/app"
	other := iss.sign(t, forged)
	parts, otherParts := splitN(tok), splitN(other)
	if _, err := v.Verify(context.Background(), parts[0]+"."+otherParts[1]+"."+parts[2]); !errors.Is(err, ErrSignature) {
		t.Errorf("forged payload: got %v", err)
	}

	// alg=none and garbage
	if _, err := v.Verify(context.Background(), "eyJhbGciOiJub25lIn0.e30."); err == nil {
		t.Error("alg none accepted")
	}
	if _, err := v.Verify(context.Background(), "nope"); !errors.Is(err, ErrMalformed) {
		t.Errorf("garbage: %v", err)
	}
}

func TestAudienceArray(t *testing.T) {
	if !audienceMatches([]any{"x", "https://d.example.com/"}, "https://d.example.com") {
		t.Fatal("array audience with trailing slash should match")
	}
}

func splitN(s string) []string {
	out := make([]string, 0, 3)
	start := 0
	for i := range len(s) {
		if s[i] == '.' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}
