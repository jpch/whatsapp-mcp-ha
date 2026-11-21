package main

import (
	"bytes"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSafeFileNameHostileNames(t *testing.T) {
	cases := map[string]string{
		"../../x":                "x",
		"../../etc/passwd":       "passwd",
		"/etc/passwd":            "passwd",
		"/":                      "_",
		`C:\Windows\system.ini`:  "system.ini",
		`..\..\x.bat`:            "x.bat",
		"..":                     "",
		".":                      "",
		"...":                    "",
		".bashrc":                "bashrc",
		"a..b.pdf":               "a_.b.pdf",
		"report\x00.pdf":         "report_.pdf",
		"line\nbreak\t.txt":      "line_break_.txt",
		"evil\u202Efdp.exe":      "evil_fdp.exe", // right-to-left override
		"a\uFF0F..\uFF0Fb.txt":   "a__._b.txt",   // fullwidth solidus
		"\u2025\u2025/x":         "x",            // two-dot leader before a real separator
		"\u2025\u2025":           "__",
		"photo.jpg":              "photo.jpg",
		strings.Repeat("a", 200): strings.Repeat("a", 100),
	}
	for in, want := range cases {
		got := safeFileName(in)
		if got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
		if got != "" && !validElement(got) {
			t.Errorf("%q -> %q is not a single safe element", in, got)
		}
		if strings.Contains(got, "..") || strings.HasPrefix(got, ".") || strings.ContainsAny(got, "/\\\x00") {
			t.Errorf("%q -> %q", in, got)
		}
	}
}

func TestCreateExclusiveNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "chat"), 0o700)
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	os.WriteFile(filepath.Join(dir, "chat", "a.pdf"), []byte("original"), 0o600)
	f, name, err := createExclusive(root, "chat", "a.pdf")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if name != "a-2.pdf" {
		t.Fatalf("name %q", name)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "chat", "a.pdf")); string(b) != "original" {
		t.Fatal("existing file overwritten")
	}
	st, _ := os.Stat(filepath.Join(dir, "chat", name))
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
	// A planted symlink with the target name is not followed or replaced.
	outside := filepath.Join(t.TempDir(), "victim")
	os.WriteFile(outside, []byte("victim"), 0o600)
	os.Symlink(outside, filepath.Join(dir, "chat", "b.pdf"))
	f, name, err = createExclusive(root, "chat", "b.pdf")
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("new"))
	f.Close()
	if name == "b.pdf" {
		t.Fatal("wrote through the symlink name")
	}
	if b, _ := os.ReadFile(outside); string(b) != "victim" {
		t.Fatal("file outside the folder changed")
	}
	for _, bad := range [][2]string{{"chat", "../x"}, {"chat", ".."}, {"chat", ""}, {"..", "x"}, {"chat", "/abs"}, {"chat", ".hidden"}} {
		if _, _, err := createExclusive(root, bad[0], bad[1]); err == nil {
			t.Errorf("accepted %q/%q", bad[0], bad[1])
		}
	}
}

func TestDownloadMediaHostileFileName(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.AllowMediaDownload = true })
	e.wa.media = []byte("doc")
	cs, err := e.session(t, e.cfg.AuthToken)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	for _, fn := range []string{"../../../../tmp/pwned.sh", `..\..\pwned.bat`, "/etc/cron.d/x", ".profile", "\u202Egpj.exe"} {
		_, err := e.store.db.Exec(`UPDATE messages SET filename = ? WHERE id = 'G2'`, fn)
		if err != nil {
			t.Fatal(err)
		}
		res, out := call(t, cs, "download_media", map[string]any{"message_id": "G2", "chat_jid": "123-456@g.us"})
		if res.IsError {
			t.Fatalf("%q: %s", fn, errText(res))
		}
		p := out["file_path"].(string)
		rel, err := filepath.Rel(e.cfg.DownloadDir(), p)
		if err != nil || strings.HasPrefix(rel, "..") || strings.Count(rel, string(filepath.Separator)) != 1 {
			t.Fatalf("%q written to %s", fn, p)
		}
		if base := filepath.Base(p); strings.HasPrefix(base, ".") || strings.Contains(base, "..") {
			t.Fatalf("%q -> %s", fn, base)
		}
		st, _ := os.Stat(p)
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("media mode %v", st.Mode())
		}
		dst, _ := os.Stat(filepath.Dir(p))
		if dst.Mode().Perm() != 0o700 {
			t.Fatalf("chat folder mode %v", dst.Mode())
		}
	}
	// Downloading the same message twice keeps both files.
	matches, _ := filepath.Glob(filepath.Join(e.cfg.DownloadDir(), "123-456", "*"))
	if len(matches) != 5 {
		t.Fatalf("files: %v", matches)
	}
}

func TestTightenPermissions(t *testing.T) {
	base := t.TempDir()
	data := filepath.Join(base, "data")
	os.MkdirAll(data, 0o755)
	for _, n := range []string{"whatsmeow.db", "whatsmeow.db-wal", "messages.db", "messages.db-shm", "auth_token", "send_token"} {
		os.WriteFile(filepath.Join(data, n), []byte("x"), 0o644)
	}
	os.WriteFile(filepath.Join(data, "options.json"), []byte("{}"), 0o644)
	outside := filepath.Join(base, "outside.txt")
	os.WriteFile(outside, []byte("x"), 0o644)
	os.Symlink(outside, filepath.Join(data, "messages.db-journal"))
	n, err := tightenDataDir(data)
	if err != nil || n != 7 {
		t.Fatalf("changed %d, %v", n, err)
	}
	mode := func(p string) fs.FileMode { st, _ := os.Lstat(p); return st.Mode().Perm() }
	if mode(data) != 0o700 || mode(filepath.Join(data, "whatsmeow.db")) != 0o600 || mode(filepath.Join(data, "send_token")) != 0o600 {
		t.Fatal("data folder not tightened")
	}
	if mode(filepath.Join(data, "options.json")) != 0o644 || mode(outside) != 0o644 {
		t.Fatal("unrelated file or symlink target changed")
	}
	if n, _ := tightenDataDir(data); n != 0 {
		t.Fatalf("second run changed %d", n)
	}

	dl := filepath.Join(base, "media", "downloads")
	os.MkdirAll(filepath.Join(dl, "chat"), 0o755)
	os.WriteFile(filepath.Join(dl, "chat", "a.jpg"), []byte("x"), 0o644)
	os.Symlink(outside, filepath.Join(dl, "chat", "link"))
	if n, err := tightenTree(dl); err != nil || n != 3 {
		t.Fatalf("downloads changed %d, %v", n, err)
	}
	if mode(filepath.Join(dl, "chat", "a.jpg")) != 0o600 || mode(filepath.Join(dl, "chat")) != 0o700 || mode(outside) != 0o644 {
		t.Fatal("downloads not tightened correctly")
	}
	if n, err := tightenTree(filepath.Join(base, "nope")); n != 0 || err != nil {
		t.Fatal("missing folder")
	}
	// A symlinked downloads folder is refused.
	os.Symlink(base, filepath.Join(base, "evil"))
	if _, err := tightenTree(filepath.Join(base, "evil")); err == nil {
		t.Fatal("symlinked folder accepted")
	}
}

func TestLogRedaction(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(newRedactingHandler(slog.NewTextHandler(&buf, nil)))
	log.Warn("failed to decrypt message from 351900000000:12@s.whatsapp.net in 120363000000000000@g.us",
		"error", errors.New("untrusted identity for 351900000001@s.whatsapp.net"),
		"lid", "123456789012345@lid", "phone", "+351900000002", "count", 42, "id", "3EB0ABCDEF")
	log.With("chat", "351900000003@s.whatsapp.net").WithGroup("g").Info("x", "group", "123-456789@g.us")
	out := buf.String()
	for _, leak := range []string{"351900000000", "120363000000000000", "351900000001", "123456789012345", "351900000002", "351900000003", "123-456789"} {
		if strings.Contains(out, leak) {
			t.Errorf("log leaks %s:\n%s", leak, out)
		}
	}
	if !strings.Contains(out, "count=42") || !strings.Contains(out, "3EB0ABCDEF") {
		t.Fatalf("redaction removed harmless data:\n%s", out)
	}
}
