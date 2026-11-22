package main

import (
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
	"strings"
	"sync"
	"time"
)

// Cloudflare Access puts a signed JWT on requests that passed an Access application:
// the Cf-Access-Jwt-Assertion header, and for browsers also the CF_Authorization cookie.
// The signature is checked against the team JWKS at https://<team>.cloudflareaccess.com/cdn-cgi/access/certs.
// See https://developers.cloudflare.com/cloudflare-one/access-controls/applications/http-apps/authorization-cookie/validating-json/

const (
	cfCertsPath     = "/cdn-cgi/access/certs"
	cfJWTMaxBytes   = 12 << 10
	cfJWKSMaxBytes  = 1 << 20
	cfKeyTTL        = time.Hour
	cfKeyMinRefresh = 30 * time.Second
	cfClockLeeway   = time.Minute
)

// normalizeCFTeamDomain accepts a team name ("myteam") or the team hostname
// ("myteam.cloudflareaccess.com") and returns the hostname. Anything else is refused
// so the certificate URL cannot be pointed at an arbitrary host.
func normalizeCFTeamDomain(raw string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimRight(s, "/")
	if s == "" {
		return "", errors.New("cf_team_domain is required when auth_method is cloudflare_jwt (your team name, or <team>.cloudflareaccess.com)")
	}
	if strings.ContainsAny(s, "/:?#@\\ \t") || strings.Contains(s, "..") {
		return "", errors.New("cf_team_domain must be a team name or <team>.cloudflareaccess.com, without a path, port or URL")
	}
	const suffix = ".cloudflareaccess.com"
	if !strings.HasSuffix(s, suffix) {
		if !isDNSLabel(s) {
			return "", errors.New("cf_team_domain must be a team name or <team>.cloudflareaccess.com")
		}
		s += suffix
	}
	label := strings.TrimSuffix(s, suffix)
	if !isDNSLabel(label) {
		return "", errors.New("cf_team_domain must be a single team label plus .cloudflareaccess.com")
	}
	return s, nil
}

func isDNSLabel(s string) bool {
	if len(s) < 1 || len(s) > 63 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' && i > 0 && i < len(s)-1:
		default:
			return false
		}
	}
	return true
}

// CFAccessVerifier checks Cloudflare Access application JWTs (RS256 only).
type CFAccessVerifier struct {
	issuer   string
	audience string
	certsURL string
	client   *http.Client
	now      func() time.Time

	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey
	fetched time.Time
}

// NewCFAccessVerifier builds a verifier for one Access application.
// teamDomain is a normalised hostname (myteam.cloudflareaccess.com). aud is the application AUD tag.
func NewCFAccessVerifier(teamDomain, aud string) *CFAccessVerifier {
	return &CFAccessVerifier{
		issuer:   "https://" + teamDomain,
		audience: aud,
		certsURL: "https://" + teamDomain + cfCertsPath,
		client: &http.Client{
			Timeout: 10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("redirects are not followed")
			},
		},
		now:  time.Now,
		keys: map[string]*rsa.PublicKey{},
	}
}

type cfHeader struct {
	Alg  string   `json:"alg"`
	Kid  string   `json:"kid"`
	Typ  string   `json:"typ"`
	Crit []string `json:"crit"`
}

type cfAudience []string

func (a *cfAudience) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		return nil
	}
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*a = []string{s}
		return nil
	}
	var ss []string
	if err := json.Unmarshal(b, &ss); err != nil {
		return err
	}
	*a = ss
	return nil
}

type cfClaims struct {
	Iss string     `json:"iss"`
	Aud cfAudience `json:"aud"`
	Exp float64    `json:"exp"`
	Nbf *float64   `json:"nbf"`
}

// Verify reports whether token is a current Access JWT for this team and application.
// The error text names the reason and never includes the token or identity claims such as email.
func (v *CFAccessVerifier) Verify(token string) error {
	if v == nil {
		return errors.New("cloudflare access jwt: verifier is not configured")
	}
	token = strings.TrimSpace(token)
	if token == "" || len(token) > cfJWTMaxBytes || strings.Count(token, ".") != 2 {
		return errors.New("cloudflare access jwt: malformed")
	}
	parts := strings.Split(token, ".")
	hb, err := decodeJWTPart(parts[0])
	if err != nil {
		return errors.New("cloudflare access jwt: malformed header")
	}
	var hdr cfHeader
	if err := json.Unmarshal(hb, &hdr); err != nil {
		return errors.New("cloudflare access jwt: malformed header")
	}
	// The algorithm is fixed. The header is not allowed to select HMAC or "none".
	if hdr.Alg != "RS256" || hdr.Kid == "" || len(hdr.Crit) != 0 || (hdr.Typ != "" && hdr.Typ != "JWT") {
		return errors.New("cloudflare access jwt: unsupported header")
	}
	sig, err := decodeJWTPart(parts[2])
	if err != nil {
		return errors.New("cloudflare access jwt: malformed signature")
	}
	key, err := v.publicKey(hdr.Kid)
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], sig); err != nil {
		return errors.New("cloudflare access jwt: bad signature")
	}
	pb, err := decodeJWTPart(parts[1])
	if err != nil {
		return errors.New("cloudflare access jwt: malformed payload")
	}
	var claims cfClaims
	if err := json.Unmarshal(pb, &claims); err != nil {
		return errors.New("cloudflare access jwt: malformed payload")
	}
	if claims.Iss != v.issuer {
		return errors.New("cloudflare access jwt: issuer mismatch")
	}
	okAud := false
	for _, aud := range claims.Aud {
		if aud == v.audience {
			okAud = true
			break
		}
	}
	if !okAud {
		return errors.New("cloudflare access jwt: audience mismatch")
	}
	now := float64(v.now().Unix())
	leeway := cfClockLeeway.Seconds()
	if claims.Exp == 0 || claims.Exp+leeway < now {
		return errors.New("cloudflare access jwt: expired")
	}
	if claims.Nbf != nil && *claims.Nbf-leeway > now {
		return errors.New("cloudflare access jwt: not yet valid")
	}
	return nil
}

func decodeJWTPart(s string) ([]byte, error) {
	if s == "" {
		return nil, errors.New("empty")
	}
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.URLEncoding.DecodeString(s)
}

func (v *CFAccessVerifier) publicKey(kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := v.now()
	key := v.keys[kid]
	fresh := !v.fetched.IsZero() && now.Sub(v.fetched) < cfKeyTTL
	if key != nil && fresh {
		return key, nil
	}
	// An unknown kid is refreshed, but not more than once per cfKeyMinRefresh (a bad kid must not
	// become a request flood against Cloudflare).
	if key == nil && !v.fetched.IsZero() && now.Sub(v.fetched) < cfKeyMinRefresh {
		return nil, errors.New("cloudflare access jwt: unknown signing key")
	}
	if err := v.fetchLocked(); err != nil {
		if key != nil {
			return key, nil // keep serving with the previous key if Cloudflare is briefly unreachable
		}
		return nil, err
	}
	key = v.keys[kid]
	if key == nil {
		return nil, errors.New("cloudflare access jwt: unknown signing key")
	}
	return key, nil
}

type cfJWK struct {
	Kid string `json:"kid"`
	Kty string `json:"kty"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	N   string `json:"n"`
	E   string `json:"e"`
}

func (v *CFAccessVerifier) fetchLocked() error {
	req, err := http.NewRequest(http.MethodGet, v.certsURL, nil)
	if err != nil {
		return fmt.Errorf("cloudflare access certs: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := v.client.Do(req)
	if err != nil {
		return errors.New("cloudflare access certs: request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New("cloudflare access certs: unexpected status")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, cfJWKSMaxBytes+1))
	if err != nil {
		return errors.New("cloudflare access certs: read failed")
	}
	if len(body) > cfJWKSMaxBytes {
		return errors.New("cloudflare access certs: response too large")
	}
	var set struct {
		Keys []cfJWK `json:"keys"`
	}
	if err := json.Unmarshal(body, &set); err != nil {
		return errors.New("cloudflare access certs: not a JWKS")
	}
	keys := make(map[string]*rsa.PublicKey, len(set.Keys))
	for _, k := range set.Keys {
		if k.Kid == "" || k.Kty != "RSA" {
			continue
		}
		if k.Alg != "" && k.Alg != "RS256" {
			continue
		}
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		pub, err := rsaPublicKey(k.N, k.E)
		if err != nil {
			continue
		}
		keys[k.Kid] = pub
	}
	if len(keys) == 0 {
		return errors.New("cloudflare access certs: no RSA signing keys")
	}
	v.keys = keys
	v.fetched = v.now()
	return nil
}

func rsaPublicKey(nB64, eB64 string) (*rsa.PublicKey, error) {
	nb, err := decodeJWTPart(nB64)
	if err != nil {
		return nil, err
	}
	eb, err := decodeJWTPart(eB64)
	if err != nil {
		return nil, err
	}
	if len(nb) == 0 || len(eb) == 0 || len(eb) > 8 {
		return nil, errors.New("bad rsa key")
	}
	e := 0
	for _, b := range eb {
		e = e<<8 | int(b)
	}
	if e < 2 {
		return nil, errors.New("bad rsa exponent")
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: e}, nil
}

// accessJWT returns the Access JWT from the header Cloudflare adds, or the browser cookie.
// The header wins when both are present. Query parameters are never accepted.
func accessJWT(h http.Header) string {
	if h == nil {
		return ""
	}
	if v := strings.TrimSpace(h.Get("Cf-Access-Jwt-Assertion")); v != "" {
		return v
	}
	c, err := (&http.Request{Header: h}).Cookie("CF_Authorization")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(c.Value)
}
