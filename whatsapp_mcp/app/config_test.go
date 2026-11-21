package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func envMap(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestLoadConfigFromOptions(t *testing.T) {
	dir := t.TempDir()
	opts := filepath.Join(dir, "options.json")
	os.WriteFile(opts, []byte(`{"auth_token":"","enable_send":false,"send_rate_per_minute":3,"send_daily_limit":20,"allow_media_download":true,"max_media_mb":8,"log_level":"warning"}`), 0o600)
	cfg, err := LoadConfig(opts, envMap(nil))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.AddonMode || cfg.PageListen != ":8121" || cfg.EnableSend || cfg.SendPerMinute != 3 || !cfg.AllowMediaDownload || cfg.MaxMediaMB != 8 {
		t.Fatalf("unexpected config %+v", cfg)
	}
}

func TestLoadConfigFromEnvDefaults(t *testing.T) {
	cfg, err := LoadConfig(filepath.Join(t.TempDir(), "missing.json"), envMap(map[string]string{"WA_MCP_DATA_DIR": "/x"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AddonMode || cfg.PageListen != "" || !cfg.EnableSend || cfg.AllowMediaDownload || cfg.DataDir != "/x" || cfg.Listen != ":8120" {
		t.Fatalf("unexpected defaults %+v", cfg)
	}
}

func TestShortTokenRejected(t *testing.T) {
	_, err := LoadConfig(filepath.Join(t.TempDir(), "missing.json"), envMap(map[string]string{"WA_MCP_TOKEN": "short"}))
	if err == nil || !strings.Contains(err.Error(), "at least") {
		t.Fatalf("want short token error, got %v", err)
	}
	_, err = LoadConfig(filepath.Join(t.TempDir(), "missing.json"), envMap(map[string]string{"WA_MCP_TOKEN": strings.Repeat("a", 31) + " b"}))
	if err == nil {
		t.Fatal("token with a space accepted")
	}
}

func TestLimitsValidated(t *testing.T) {
	for _, env := range []map[string]string{
		{"WA_MCP_SEND_RATE_PER_MINUTE": "0"}, {"WA_MCP_SEND_DAILY_LIMIT": "0"}, {"WA_MCP_MAX_MEDIA_MB": "500"}, {"WA_MCP_ENABLE_SEND": "maybe"},
	} {
		if _, err := LoadConfig(filepath.Join(t.TempDir(), "missing.json"), envMap(env)); err == nil {
			t.Fatalf("accepted %v", env)
		}
	}
}

func TestGeneratedTokenPersisted(t *testing.T) {
	cfg := &Config{DataDir: t.TempDir()}
	if err := cfg.EnsureToken(); err != nil {
		t.Fatal(err)
	}
	if !cfg.TokenGenerated || len(cfg.AuthToken) < MinTokenLength {
		t.Fatalf("bad generated token %q", cfg.AuthToken)
	}
	st, _ := os.Stat(filepath.Join(cfg.DataDir, "auth_token"))
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode %v", st.Mode())
	}
	again := &Config{DataDir: cfg.DataDir}
	again.EnsureToken()
	if again.AuthToken != cfg.AuthToken {
		t.Fatal("generated token not reused")
	}
	own := &Config{DataDir: t.TempDir(), AuthToken: strings.Repeat("x", 40)}
	own.EnsureToken()
	if own.TokenGenerated || own.AuthToken != strings.Repeat("x", 40) {
		t.Fatal("configured token replaced")
	}
}

func TestWeakTokensRejected(t *testing.T) {
	for _, tok := range []string{strings.Repeat("a", 40), strings.Repeat("abc", 15), strings.Repeat("01234567", 5), "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaabcd"} {
		if _, err := LoadConfig(filepath.Join(t.TempDir(), "missing.json"), envMap(map[string]string{"WA_MCP_TOKEN": tok})); err == nil {
			t.Errorf("weak token %q accepted", tok)
		}
	}
	if _, err := LoadConfig(filepath.Join(t.TempDir(), "missing.json"), envMap(map[string]string{"WA_MCP_TOKEN": "Zq3vR8kP1xW7mN4tY6bC9dF2gH5jL0sA"})); err != nil {
		t.Fatal(err)
	}
}

func TestAllowedRecipientsAndRetention(t *testing.T) {
	dir := t.TempDir()
	opts := filepath.Join(dir, "options.json")
	os.WriteFile(opts, []byte(`{"allowed_recipients":["+351900000000","123-456@g.us",""],"retention_days":30}`), 0o600)
	cfg, err := LoadConfig(opts, envMap(nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.AllowedRecipients) != 2 || !cfg.AllowedRecipients["351900000000@s.whatsapp.net"] || cfg.RetentionDays != 30 || cfg.SendPerDay != 50 {
		t.Fatalf("%+v", cfg)
	}
	os.WriteFile(opts, []byte(`{"allowed_recipients":["status@broadcast"]}`), 0o600)
	if _, err := LoadConfig(opts, envMap(nil)); err == nil {
		t.Fatal("invalid allowed recipient accepted")
	}
	if _, err := LoadConfig(filepath.Join(dir, "missing.json"), envMap(map[string]string{"WA_MCP_RETENTION_DAYS": "-1"})); err == nil {
		t.Fatal("negative retention accepted")
	}
	cfg, _ = LoadConfig(filepath.Join(dir, "missing.json"), envMap(map[string]string{"WA_MCP_ALLOWED_RECIPIENTS": "351911111111,1-2@g.us"}))
	if len(cfg.AllowedRecipients) != 2 {
		t.Fatalf("env allowlist: %+v", cfg.AllowedRecipients)
	}
}
