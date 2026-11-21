package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/time/rate"
)

// MCPPath is the MCP endpoint path.
const MCPPath = "/mcp"

// Authenticator checks bearer tokens in constant time and slows down repeated failures.
type Authenticator struct {
	want atomic.Pointer[[32]byte]
	log  *slog.Logger

	mu         sync.Mutex
	failures   map[string]*failEntry
	now        func() time.Time
	logLimit   *rate.Limiter // failed-auth log lines, all clients together
	suppressed int
}

type failEntry struct {
	lim  *rate.Limiter
	seen time.Time
}

func NewAuthenticator(token string, log *slog.Logger) *Authenticator {
	a := &Authenticator{log: log, failures: map[string]*failEntry{}, now: time.Now,
		logLimit: rate.NewLimiter(rate.Every(10*time.Second), 5)}
	a.SetToken(token)
	return a
}

// SetToken replaces the accepted token (token rotation from the add-on page).
func (a *Authenticator) SetToken(token string) {
	h := sha256.Sum256([]byte(token))
	a.want.Store(&h)
}

// Check returns true if the Authorization header carries the token ("Bearer <token>").
func (a *Authenticator) Check(header string) bool {
	scheme, tok, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		tok = ""
	}
	got := sha256.Sum256([]byte(strings.TrimSpace(tok)))
	want := a.want.Load()
	match := subtle.ConstantTimeCompare(got[:], want[:]) == 1
	return match && tok != ""
}

// clientKey identifies the caller for failure limiting. Behind Cloudflare every request comes from
// the tunnel connector, so the CF-Connecting-IP header is part of the key. It can be forged, which
// only lets a caller evade its own limit; the token length is what prevents guessing.
func clientKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if cf := r.Header.Get("CF-Connecting-IP"); cf != "" && len(cf) <= 64 {
		return host + "|" + cf
	}
	return host
}

const maxFailEntries = 10000

func (a *Authenticator) limited(key string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	e := a.failures[key]
	return e != nil && e.lim.TokensAt(a.now()) < 1
}

func (a *Authenticator) recordFailure(key string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	e := a.failures[key]
	if e == nil {
		if len(a.failures) >= maxFailEntries {
			// Evict the least recently seen entry (never a full reset, which would clear every limit).
			oldestKey, oldest := "", now
			for k, v := range a.failures {
				if v.seen.Before(oldest) || oldestKey == "" {
					oldestKey, oldest = k, v.seen
				}
			}
			delete(a.failures, oldestKey)
		}
		// 10 failed attempts, then one more every 6 seconds.
		e = &failEntry{lim: rate.NewLimiter(rate.Every(6*time.Second), 10)}
		a.failures[key] = e
	}
	e.seen = now
	e.lim.AllowN(now, 1)
}

// logFailure logs failed attempts with a global rate limit, so a flood cannot fill the log.
func (a *Authenticator) logFailure(key string) {
	a.mu.Lock()
	allow := a.logLimit.AllowN(a.now(), 1)
	suppressed := a.suppressed
	if allow {
		a.suppressed = 0
	} else {
		a.suppressed++
	}
	a.mu.Unlock()
	if allow {
		a.log.Warn("rejected MCP request without a valid token", "client", key, "suppressed_since_last", suppressed)
	}
}

// Middleware rejects requests without the bearer token. Nothing about the token is logged.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := clientKey(r)
		if a.limited(key) {
			w.Header().Set("Retry-After", "60")
			http.Error(w, "too many failed authentication attempts", http.StatusTooManyRequests)
			return
		}
		if !a.Check(r.Header.Get("Authorization")) {
			a.recordFailure(key)
			a.logFailure(key)
			w.Header().Set("WWW-Authenticate", `Bearer realm="whatsapp-mcp"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// NewMCPHandler builds the exposed handler: GET /healthz (no details) and the authenticated /mcp.
// Every other path returns 404.
func NewMCPHandler(server *mcp.Server, auth *Authenticator) http.Handler {
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
		Stateless:           true,
		JSONResponse:        true,
		MaxRequestBodyBytes: 1 << 20,
	})
	protected := auth.Middleware(http.NewCrossOriginProtection().Handler(mcpHandler))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.Handle(MCPPath, protected)
	mux.HandleFunc("/", http.NotFound)
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Cache-Control", "no-store")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func newHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    16 << 10,
	}
}
