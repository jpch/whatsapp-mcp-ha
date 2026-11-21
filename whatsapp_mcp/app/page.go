package main

import (
	"context"
	_ "embed"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
//
// Every Home Assistant ingress page is served from the same origin, so a page of another add-on that an
// administrator opens could call these endpoints too. A generated token is therefore revealed only once;
// after that it can only be replaced (rotated), which also reveals the new one once.
func NewPageHandler(cfg *Config, store *Store, wa PageController, auth *Authenticator) http.Handler {
	hostname, _ := os.Hostname()
	var tokMu sync.Mutex
	shownPath := filepath.Join(cfg.DataDir, tokenShownFile)
	sendShownPath := filepath.Join(cfg.DataDir, sendTokenShownFile)
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
			"whatsapp":             wa.Status(),
			"has_qr":               wa.QRCode() != "",
			"chats":                chats,
			"messages":             msgs,
			"token_generated":      cfg.TokenGenerated,
			"token_shown":          cfg.TokenGenerated && fileExists(shownPath),
			"send_enabled":         cfg.SendActive(),
			"send_token_generated": cfg.SendTokenGenerated && cfg.SendActive(),
			"send_token_shown":     cfg.SendTokenGenerated && cfg.SendActive() && fileExists(sendShownPath),
			"allowed_recipients":   len(cfg.AllowedRecipients),
			"media_download":       cfg.AllowMediaDownload,
			"internal_url":         "http://" + hostname + ":" + port + MCPPath,
			"version":              Version,
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
	// The read-only token (auth_token) and the send token (send_token) are handled the same way.
	type tokenSlot struct {
		route, option, shownFile, label string
		generated                       func() bool
		get                             func() string
		rotate                          func() (string, error)
	}
	slots := []tokenSlot{
		{route: "/api/token", option: "auth_token", shownFile: tokenShownFile, label: "generated token",
			generated: func() bool { return cfg.TokenGenerated },
			get:       func() string { return cfg.AuthToken },
			rotate: func() (string, error) {
				tok, err := GenerateToken(cfg.DataDir)
				if err == nil {
					cfg.AuthToken = tok
					auth.SetToken(tok)
				}
				return tok, err
			}},
		{route: "/api/send_token", option: "send_token", shownFile: sendTokenShownFile, label: "generated send token",
			generated: func() bool { return cfg.SendTokenGenerated && cfg.SendActive() },
			get:       func() string { return cfg.SendToken },
			rotate: func() (string, error) {
				tok, err := GenerateSendToken(cfg.DataDir)
				if err == nil {
					cfg.SendToken = tok
					auth.SetSendToken(tok)
				}
				return tok, err
			}},
	}
	for _, sl := range slots {
		shown := filepath.Join(cfg.DataDir, sl.shownFile)
		mux.HandleFunc("POST "+sl.route, func(w http.ResponseWriter, r *http.Request) {
			tokMu.Lock()
			defer tokMu.Unlock()
			if !sl.generated() {
				writeJSON(w, http.StatusOK, map[string]any{"token": nil, "reason": "configured"})
				return
			}
			if fileExists(shown) {
				writeJSON(w, http.StatusOK, map[string]any{"token": nil, "reason": "already_shown"})
				return
			}
			if err := os.WriteFile(shown, nil, 0o600); err != nil {
				http.Error(w, "cannot record that the token was shown", http.StatusInternalServerError)
				return
			}
			// Audit trail (never the token): lets the owner spot a reveal they did not make.
			slog.Warn(sl.label + " revealed on the add-on page")
			writeJSON(w, http.StatusOK, map[string]any{"token": sl.get()})
		})
		mux.HandleFunc("POST "+sl.route+"/rotate", func(w http.ResponseWriter, r *http.Request) {
			tokMu.Lock()
			defer tokMu.Unlock()
			if !sl.generated() {
				writeJSON(w, http.StatusConflict, map[string]string{"error": sl.option + " is set in the configuration; change it there"})
				return
			}
			tok, err := sl.rotate()
			if err != nil {
				http.Error(w, "cannot generate a token", http.StatusInternalServerError)
				return
			}
			if err := os.WriteFile(shown, nil, 0o600); err != nil {
				http.Error(w, "cannot record that the token was shown", http.StatusInternalServerError)
				return
			}
			slog.Warn(sl.label + " rotated on the add-on page; MCP clients need the new token")
			writeJSON(w, http.StatusOK, map[string]any{"token": tok})
		})
	}
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

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
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
