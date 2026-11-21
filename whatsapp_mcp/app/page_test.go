package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func pageReq(h http.Handler, method, path, remote string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = remote
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestPageIngressOnly(t *testing.T) {
	cfg := testConfig(t)
	cfg.PageAllowedIPs = []net.IP{net.ParseIP("172.30.32.2")}
	cfg.TokenGenerated = true
	wa := &fakeWA{qr: "2@abc,def", status: Status{State: StateWaitingForQR}}
	os.MkdirAll(cfg.DataDir, 0o700)
	auth := NewAuthenticator(cfg.AuthToken, quietLog())
	h := NewPageHandler(cfg, testStore(t), wa, auth)
	ingress := "172.30.32.2:5555"
	csrf := map[string]string{"X-Requested-With": "whatsapp-mcp"}

	if w := pageReq(h, "GET", "/", "172.30.33.7:5555", nil); w.Code != 403 {
		t.Fatalf("other add-on reached the page: %d", w.Code)
	}
	if w := pageReq(h, "POST", "/api/token", "10.0.0.2:1", csrf); w.Code != 403 {
		t.Fatalf("token from LAN: %d", w.Code)
	}
	if w := pageReq(h, "GET", "/", ingress, nil); w.Code != 200 || !strings.Contains(w.Header().Get("Content-Security-Policy"), "default-src 'none'") {
		t.Fatalf("page: %d", w.Code)
	}
	if w := pageReq(h, "GET", "/app.js", ingress, nil); w.Code != 200 {
		t.Fatalf("app.js: %d", w.Code)
	}
	if w := pageReq(h, "GET", "/api/qr.png", ingress, nil); w.Code != 200 || w.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("qr: %d", w.Code)
	}
	if w := pageReq(h, "GET", "/api/status", ingress, nil); w.Code != 200 || strings.Contains(w.Body.String(), cfg.AuthToken) || strings.Contains(w.Body.String(), "2@abc") {
		t.Fatalf("status leaks secrets: %s", w.Body.String())
	}
	// State-changing requests need the custom header.
	if w := pageReq(h, "POST", "/api/token", ingress, nil); w.Code != 403 {
		t.Fatalf("token without header: %d", w.Code)
	}
	if w := pageReq(h, "POST", "/api/token", ingress, map[string]string{"X-Requested-With": "whatsapp-mcp", "Sec-Fetch-Site": "cross-site"}); w.Code != 403 {
		t.Fatalf("cross-site token: %d", w.Code)
	}
	if w := pageReq(h, "POST", "/api/token", ingress, csrf); w.Code != 200 || !strings.Contains(w.Body.String(), cfg.AuthToken) {
		t.Fatalf("token: %d", w.Code)
	}
	// Shown only once.
	if w := pageReq(h, "POST", "/api/token", ingress, csrf); strings.Contains(w.Body.String(), cfg.AuthToken) || !strings.Contains(w.Body.String(), "already_shown") {
		t.Fatalf("token shown twice: %s", w.Body.String())
	}
	if w := pageReq(h, "GET", "/api/status", ingress, nil); !strings.Contains(w.Body.String(), `"token_shown":true`) {
		t.Fatalf("status: %s", w.Body.String())
	}
	old := cfg.AuthToken
	w := pageReq(h, "POST", "/api/token/rotate", ingress, csrf)
	var rot struct{ Token string }
	json.Unmarshal(w.Body.Bytes(), &rot)
	if w.Code != 200 || rot.Token == "" || rot.Token == old || !auth.Check("Bearer "+rot.Token) || auth.Check("Bearer "+old) {
		t.Fatalf("rotate: %d %s", w.Code, w.Body.String())
	}
	if b, _ := os.ReadFile(filepath.Join(cfg.DataDir, "auth_token")); strings.TrimSpace(string(b)) != rot.Token {
		t.Fatal("rotated token not persisted")
	}
	if w := pageReq(h, "POST", "/api/token", ingress, csrf); strings.Contains(w.Body.String(), rot.Token) {
		t.Fatal("rotated token shown again")
	}
	if w := pageReq(h, "GET", "/api/token", ingress, nil); w.Code != 405 {
		t.Fatalf("GET token: %d", w.Code)
	}
	pageReq(h, "POST", "/api/pair", ingress, csrf)
	pageReq(h, "POST", "/api/logout", ingress, csrf)
	if wa.pairReqs != 1 || wa.logouts != 1 {
		t.Fatalf("pair %d logout %d", wa.pairReqs, wa.logouts)
	}
	cfg.TokenGenerated = false
	if w := pageReq(h, "POST", "/api/token", ingress, csrf); strings.Contains(w.Body.String(), cfg.AuthToken) {
		t.Fatal("configured token revealed")
	}
	if w := pageReq(h, "POST", "/api/token/rotate", ingress, csrf); w.Code != 409 {
		t.Fatalf("rotate with configured token: %d", w.Code)
	}
}
