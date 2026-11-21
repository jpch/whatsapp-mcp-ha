package main

import (
	"net"
	"net/http"
	"net/http/httptest"
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
	h := NewPageHandler(cfg, testStore(t), wa)
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
}
