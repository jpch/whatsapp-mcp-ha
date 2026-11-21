package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func pngBytes() []byte {
	return append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 64)...)
}

func TestReadOutboxFile(t *testing.T) {
	base := t.TempDir()
	outbox := filepath.Join(base, "outbox")
	os.MkdirAll(filepath.Join(outbox, "sub"), 0o755)
	os.WriteFile(filepath.Join(outbox, "pic.png"), pngBytes(), 0o644)
	os.WriteFile(filepath.Join(outbox, "sub", "doc.pdf"), []byte("%PDF-1.4 test"), 0o644)
	os.WriteFile(filepath.Join(base, "secret.txt"), []byte("secret"), 0o644)
	os.WriteFile(filepath.Join(outbox, "big.bin"), bytes.Repeat([]byte{1}, 2<<20), 0o644)
	os.Symlink(filepath.Join(base, "secret.txt"), filepath.Join(outbox, "link.txt"))
	os.Symlink(base, filepath.Join(outbox, "dirlink"))

	f, err := ReadOutboxFile(outbox, "pic.png", 1<<20)
	if err != nil || f.Kind != KindImage || f.Mime != "image/png" {
		t.Fatalf("pic.png: %v %+v", err, f)
	}
	f, err = ReadOutboxFile(outbox, filepath.Join(outbox, "sub", "doc.pdf"), 1<<20)
	if err != nil || f.Kind != KindDocument || f.Name != "doc.pdf" {
		t.Fatalf("absolute path in outbox: %v", err)
	}
	for _, bad := range []string{
		"../secret.txt", filepath.Join(base, "secret.txt"), "/etc/passwd", "link.txt", "dirlink/secret.txt",
		"sub/../../secret.txt", "", "sub", "big.bin", "missing.png", "a\x00b",
	} {
		if _, err := ReadOutboxFile(outbox, bad, 1<<20); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
}

func oggPage(granule uint64, seq uint32, payload []byte) []byte {
	h := make([]byte, 27)
	copy(h, "OggS")
	binary.LittleEndian.PutUint64(h[6:], granule)
	binary.LittleEndian.PutUint32(h[18:], seq)
	h[26] = 1
	return append(append(h, byte(len(payload))), payload...)
}

func TestAnalyzeOggOpus(t *testing.T) {
	head := make([]byte, 19)
	copy(head, "OpusHead")
	head[8] = 1
	head[9] = 1
	binary.LittleEndian.PutUint16(head[10:], 312)
	binary.LittleEndian.PutUint32(head[12:], 48000)
	data := append(oggPage(0, 0, head), oggPage(0, 1, bytes.Repeat([]byte{'x'}, 40))...)
	data = append(data, oggPage(48000*5+312, 2, []byte{1, 2, 3})...)
	secs, wave, err := analyzeOggOpus(data)
	if err != nil || secs != 5 || len(wave) != 64 {
		t.Fatalf("got %d %d %v", secs, len(wave), err)
	}
	// Truncated input must not panic.
	for i := 0; i < len(data); i++ {
		analyzeOggOpus(data[:i])
	}
	if _, kind := detectMedia("v.ogg", data); kind != KindAudio {
		t.Fatalf("ogg opus detected as %s", kind)
	}
}

func TestSafeFileName(t *testing.T) {
	for in, want := range map[string]string{"../../etc/passwd": "passwd", "a b/c.pdf": "c.pdf", "..hidden": "hidden", `x\..\y.txt`: "y.txt"} {
		if got := safeFileName(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}
