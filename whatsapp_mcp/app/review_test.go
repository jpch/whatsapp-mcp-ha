package main

import (
	"bytes"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func mcpPost(t *testing.T, url, token, body string, hdr map[string]string) int {
	req, _ := http.NewRequest("POST", url+MCPPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2025-06-18")
	req.Header.Set("Authorization", "Bearer "+token)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Error(err)
		return 0
	}
	resp.Body.Close()
	return resp.StatusCode
}

// Run with -race: rotating both tokens on the page while send_message and get_status run must not race.
func TestRotateWhileSendingNoRace(t *testing.T) {
	e, auth := newEnvAuth(t, func(c *Config) { c.SendToken = ""; c.TokenGenerated = true })
	if !e.cfg.SendTokenGenerated || !e.cfg.SendActive() {
		t.Fatal("expected a generated, active send token")
	}
	e.cfg.PageAllowedIPs = []net.IP{net.ParseIP("172.30.32.2")}
	page := NewPageHandler(e.cfg, e.store, e.wa, auth)
	csrf := map[string]string{"X-Requested-With": "whatsapp-mcp"}
	sendTok, readTok := e.cfg.SendToken, e.cfg.AuthToken
	send := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"send_message","arguments":{"recipient":"351900000000","message":"x"}}}`
	status := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_status","arguments":{}}}`
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				pageReq(page, "POST", "/api/send_token/rotate", "172.30.32.2:1", csrf)
				pageReq(page, "POST", "/api/token/rotate", "172.30.32.2:1", csrf)
				pageReq(page, "GET", "/api/status", "172.30.32.2:1", nil)
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				mcpPost(t, e.srv.URL, sendTok, send, nil)
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				mcpPost(t, e.srv.URL, readTok, status, nil)
				_ = e.cfg.SendActive()
			}
		}()
	}
	wg.Wait()
	if auth.Check("Bearer "+sendTok) || auth.Check("Bearer "+readTok) {
		t.Fatal("old tokens still accepted after rotation")
	}
	if !e.cfg.SendActive() {
		t.Fatal("SendActive changed at runtime")
	}
}

func TestInactiveSendTokenIsNotAFailedLogin(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.EnableSend = false })
	list := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	cf := map[string]string{"CF-Connecting-IP": "203.0.113.7"}
	// More than the failed-login allowance (10) from one caller key.
	for i := 0; i < 15; i++ {
		if c := mcpPost(t, e.srv.URL, e.cfg.SendToken, list, cf); c != http.StatusForbidden {
			t.Fatalf("send token while disabled: %d, want 403", c)
		}
	}
	// Another client behind the same tunnel (same caller key) is not locked out.
	if c := mcpPost(t, e.srv.URL, e.cfg.AuthToken, list, cf); c != http.StatusOK {
		t.Fatalf("read token after inactive send token attempts: %d", c)
	}
}

func TestInactiveGeneratedSendTokenRemembered(t *testing.T) {
	dir := t.TempDir()
	on := &Config{DataDir: dir, EnableSend: true}
	if err := on.EnsureToken(); err != nil {
		t.Fatal(err)
	}
	if err := on.EnsureSendToken(); err != nil || !on.SendActive() {
		t.Fatal(err)
	}
	off := &Config{DataDir: dir, AuthToken: on.AuthToken}
	if err := off.EnsureSendToken(); err != nil || off.SendActive() || off.InactiveSendToken != on.SendToken {
		t.Fatalf("inactive generated send token: %v %+v", err, off)
	}
}

func TestConfigErrorDoesNotRepeatRecipient(t *testing.T) {
	opts := filepath.Join(t.TempDir(), "options.json")
	os.WriteFile(opts, []byte(`{"allowed_recipients":["351900000000","+351 900 000 001"]}`), 0o600)
	_, err := LoadConfig(opts, envMap(nil))
	if err == nil || strings.Contains(err.Error(), "900") || !strings.Contains(err.Error(), "entry 2") {
		t.Fatalf("error: %v", err)
	}
}

func TestLogRedactionGroupedNumbers(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(newRedactingHandler(slog.NewTextHandler(&buf, nil)))
	log.Info("contact +351 900 000 001 and 351-900-000-002, (351) 900 000 003, 900 000 004",
		"note", "call +44 20 7946 0000", "date", "2026-11-21", "time", "18:20:29", "addr", "198.51.100.200:443")
	out := buf.String()
	for _, leak := range []string{"900 000 001", "900-000-002", "900 000 003", "900 000 004", "7946"} {
		if strings.Contains(out, leak) {
			t.Errorf("leaks %q:\n%s", leak, out)
		}
	}
	for _, keep := range []string{"2026-11-21", "18:20:29", "198.51.100.200:443"} {
		if !strings.Contains(out, keep) {
			t.Errorf("over-redacted %q:\n%s", keep, out)
		}
	}
}
