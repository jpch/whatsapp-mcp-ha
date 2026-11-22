package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testRSA(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func jwksOf(pub *rsa.PublicKey, kid string) string {
	n := base64.RawURLEncoding.EncodeToString(pub.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes())
	b, _ := json.Marshal(map[string]any{"keys": []map[string]string{{
		"kty": "RSA", "kid": kid, "alg": "RS256", "use": "sig", "n": n, "e": e,
	}}})
	return string(b)
}

func signAccessJWT(t *testing.T, key *rsa.PrivateKey, kid, alg string, claims map[string]any) string {
	t.Helper()
	if alg == "" {
		alg = "RS256"
	}
	hb, err := json.Marshal(map[string]string{"alg": alg, "kid": kid, "typ": "JWT"})
	if err != nil {
		t.Fatal(err)
	}
	pb, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	body := enc(hb) + "." + enc(pb)
	if alg == "none" {
		return body + "."
	}
	sum := sha256.Sum256([]byte(body))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return body + "." + enc(sig)
}

func accessVerifier(t *testing.T, jwks string) (*CFAccessVerifier, string) {
	t.Helper()
	const aud = "aud-0123456789abcdef"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != cfCertsPath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(jwks))
	}))
	t.Cleanup(srv.Close)
	v := NewCFAccessVerifier("team.cloudflareaccess.com", aud)
	v.certsURL = srv.URL + cfCertsPath
	v.client = srv.Client()
	return v, aud
}

func goodClaims(aud string) map[string]any {
	now := time.Now().Unix()
	return map[string]any{
		"iss":   "https://team.cloudflareaccess.com",
		"aud":   []string{aud},
		"exp":   now + 3600,
		"nbf":   now - 10,
		"sub":   "user-id",
		"email": "person@example.com",
	}
}

func TestCloudflareJWTAcceptsHeaderAndCookie(t *testing.T) {
	key := testRSA(t)
	v, aud := accessVerifier(t, jwksOf(&key.PublicKey, "kid-1"))
	tok := signAccessJWT(t, key, "kid-1", "", goodClaims(aud))
	if err := v.Verify(tok); err != nil {
		t.Fatal(err)
	}
	h := make(http.Header)
	h.Set("Cf-Access-Jwt-Assertion", tok)
	if accessJWT(h) != tok {
		t.Fatal("header not read")
	}
	h.Set("Cookie", "CF_Authorization="+tok)
	h.Del("Cf-Access-Jwt-Assertion")
	if accessJWT(h) != tok {
		t.Fatal("cookie not read")
	}
	// The header wins over the cookie, and a query string is not a source.
	h.Set("Cf-Access-Jwt-Assertion", "header-token")
	if accessJWT(h) != "header-token" {
		t.Fatal("header should win")
	}
}

func TestCloudflareJWTRejectsBadTokens(t *testing.T) {
	key := testRSA(t)
	other := testRSA(t)
	v, aud := accessVerifier(t, jwksOf(&key.PublicKey, "kid-1"))
	now := time.Now().Unix()
	cases := []struct {
		name   string
		token  string
		reason string
	}{
		{"none alg", signAccessJWT(t, key, "kid-1", "none", goodClaims(aud)), "unsupported"},
		{"other key", signAccessJWT(t, other, "kid-1", "", goodClaims(aud)), "signature"},
		{"unknown kid", signAccessJWT(t, key, "kid-missing", "", goodClaims(aud)), "unknown"},
		{"wrong iss", signAccessJWT(t, key, "kid-1", "", map[string]any{
			"iss": "https://evil.cloudflareaccess.com", "aud": []string{aud}, "exp": now + 60,
		}), "issuer"},
		{"wrong aud", signAccessJWT(t, key, "kid-1", "", map[string]any{
			"iss": "https://team.cloudflareaccess.com", "aud": "someone-else", "exp": now + 60,
		}), "audience"},
		{"expired", signAccessJWT(t, key, "kid-1", "", map[string]any{
			"iss": "https://team.cloudflareaccess.com", "aud": []string{aud}, "exp": now - 3600,
		}), "expired"},
	}
	for _, tc := range cases {
		err := v.Verify(tc.token)
		if err == nil || !strings.Contains(err.Error(), tc.reason) {
			t.Errorf("%s: %v", tc.name, err)
		}
		if err != nil && strings.Contains(err.Error(), "person@example.com") {
			t.Errorf("%s: error leaked an identity claim", tc.name)
		}
	}
	if err := v.Verify("not-a-jwt"); err == nil {
		t.Fatal("malformed token accepted")
	}
}

func TestCloudflareJWTRefreshesUnknownKid(t *testing.T) {
	key := testRSA(t)
	jwks := jwksOf(&key.PublicKey, "kid-1")
	v, aud := accessVerifier(t, jwks)
	if err := v.Verify(signAccessJWT(t, key, "kid-1", "", goodClaims(aud))); err != nil {
		t.Fatal(err)
	}
	// A new kid is published. The cache is still fresh, so the first unknown kid is refused
	// without a stampede; once the cache is old enough it is fetched again.
	v.mu.Lock()
	v.fetched = time.Now().Add(-cfKeyMinRefresh - time.Second)
	v.mu.Unlock()
	rotated := testRSA(t)
	// Point the existing server at the new key by replacing the handler URL contents through a new server.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(jwksOf(&rotated.PublicKey, "kid-2")))
	}))
	t.Cleanup(srv.Close)
	v.certsURL = srv.URL
	v.client = srv.Client()
	if err := v.Verify(signAccessJWT(t, rotated, "kid-2", "", goodClaims(aud))); err != nil {
		t.Fatal(err)
	}
}

func TestTeamDomainNormalisation(t *testing.T) {
	got, err := normalizeCFTeamDomain(" HTTPS://My-Team.cloudflareaccess.com/ ")
	if err != nil || got != "my-team.cloudflareaccess.com" {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := normalizeCFTeamDomain("my-team"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "https://evil.com", "team.cloudflareaccess.com.evil.com", "team.cloudflareaccess.com/cdn-cgi/access/certs", "team:443", "a/b", ".."} {
		if _, err := normalizeCFTeamDomain(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
