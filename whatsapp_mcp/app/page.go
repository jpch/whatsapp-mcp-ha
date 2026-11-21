package main

import (
	"context"
	_ "embed"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	qrcode "github.com/skip2/go-qrcode"
)

//go:embed web/index.html
var pageHTML []byte

//go:embed web/app.js
var pageJS []byte

// PageController is what the pairing page needs from the WhatsApp manager.
type PageController interface {
	Status() Status
	QRCode() string
	RequestPairing()
	Logout(ctx context.Context) error
}

// NewPageHandler serves the pairing page for Home Assistant ingress. Only the ingress proxy
// address may connect (Home Assistant has already authenticated the user); state-changing
// requests also need a custom header, which browsers cannot send cross-site without CORS.
func NewPageHandler(cfg *Config, store *Store, wa PageController) http.Handler {
	hostname, _ := os.Hostname()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'unsafe-inline'; img-src 'self'; connect-src 'self'; frame-ancestors 'self'")
		_, _ = w.Write(pageHTML)
	})
	mux.HandleFunc("GET /app.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		_, _ = w.Write(pageJS)
	})
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		chats, msgs := store.Counts(r.Context())
		_, port, _ := net.SplitHostPort(cfg.Listen)
		writeJSON(w, http.StatusOK, map[string]any{
			"whatsapp":        wa.Status(),
			"has_qr":          wa.QRCode() != "",
			"chats":           chats,
			"messages":        msgs,
			"token_generated": cfg.TokenGenerated,
			"send_enabled":    cfg.EnableSend,
			"media_download":  cfg.AllowMediaDownload,
			"internal_url":    "http://" + hostname + ":" + port + MCPPath,
			"version":         Version,
		})
	})
	mux.HandleFunc("GET /api/qr.png", func(w http.ResponseWriter, r *http.Request) {
		code := wa.QRCode()
		if code == "" {
			http.NotFound(w, r)
			return
		}
		png, err := qrcode.Encode(code, qrcode.Medium, 320)
		if err != nil {
			http.Error(w, "cannot render QR code", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(png)
	})
	mux.HandleFunc("POST /api/token", func(w http.ResponseWriter, r *http.Request) {
		if !cfg.TokenGenerated {
			writeJSON(w, http.StatusOK, map[string]any{"token": nil})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"token": cfg.AuthToken})
	})
	mux.HandleFunc("POST /api/pair", func(w http.ResponseWriter, r *http.Request) {
		wa.RequestPairing()
		writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/logout", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		if err := wa.Logout(ctx); err != nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	return securityHeaders(ingressOnly(cfg.PageAllowedIPs, mux))
}

func ingressOnly(allowed []net.IP, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		ip := net.ParseIP(host)
		ok := false
		for _, a := range allowed {
			if err == nil && ip != nil && a.Equal(ip) {
				ok = true
				break
			}
		}
		if !ok {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if r.Header.Get("X-Requested-With") != "whatsapp-mcp" || strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site") {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
