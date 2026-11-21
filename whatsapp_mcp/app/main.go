// Command whatsapp-mcp links a personal WhatsApp account as a device (whatsmeow, like WhatsApp Web)
// and serves it as an MCP server over streamable HTTP.
//
// The message store and tool set are derived from lharries/whatsapp-mcp (MIT).
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := LoadConfig("/data/options.json", os.Getenv)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(log)

	// Session keys, messages and the generated token: owner-only files.
	syscall.Umask(0o077)
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(cfg.DataDir, 0o700); err != nil {
		log.Warn("cannot restrict the data folder permissions", "error", err)
	}
	if err := os.MkdirAll(cfg.OutboxDir(), 0o750); err != nil {
		log.Warn("cannot create the outbox folder; send_file will not work", "error", err)
	}
	if err := cfg.EnsureToken(); err != nil {
		return err
	}
	if cfg.TokenGenerated {
		if cfg.AddonMode {
			log.Info("no auth_token configured: using a generated token. Open the add-on page (Open Web UI) to see it, or set auth_token in the configuration")
		} else {
			log.Info("no WA_MCP_TOKEN set: using a generated token stored in the data folder (auth_token file)")
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	store, err := OpenStore(filepath.Join(cfg.DataDir, "messages.db"))
	if err != nil {
		return err
	}
	defer store.Close()
	_ = os.Chmod(filepath.Join(cfg.DataDir, "messages.db"), 0o600)

	mgr, err := NewManager(ctx, cfg, store, log)
	if err != nil {
		return err
	}
	tools := &Tools{cfg: cfg, store: store, wa: mgr, sends: NewSendLimiter(cfg.SendPerMinute, cfg.SendPerDay)}
	auth := NewAuthenticator(cfg.AuthToken, log)
	servers := []*http.Server{newHTTPServer(cfg.Listen, NewMCPHandler(NewMCPServer(tools), auth))}
	if cfg.PageListen != "" {
		servers = append(servers, newHTTPServer(cfg.PageListen, NewPageHandler(cfg, store, mgr)))
	}
	errCh := make(chan error, len(servers))
	for _, s := range servers {
		go func(s *http.Server) {
			if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}(s)
	}
	log.Info("WhatsApp MCP started", "version", Version, "mcp", cfg.Listen+MCPPath,
		"send_tools", cfg.EnableSend, "media_download", cfg.AllowMediaDownload)

	done := make(chan struct{})
	if os.Getenv("WA_MCP_NO_CONNECT") == "1" {
		// Test hook (CI smoke test): serve HTTP without ever contacting WhatsApp.
		log.Warn("WA_MCP_NO_CONNECT=1: not connecting to WhatsApp")
		close(done)
	} else {
		go func() { mgr.Run(ctx); close(done) }()
	}

	select {
	case <-ctx.Done():
	case err = <-errCh:
		stop()
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(shutdownCtx)
	}
	select {
	case <-done:
	case <-shutdownCtx.Done():
	}
	return err
}
