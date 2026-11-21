package main

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
)

type fakeWA struct {
	mu       sync.Mutex
	status   Status
	sent     []string
	files    []*OutboxFile
	contacts map[string]string
	media    []byte
	qr       string
	pairReqs int
	logouts  int
}

func (f *fakeWA) Status() Status { return f.status }
func (f *fakeWA) SendText(ctx context.Context, to types.JID, text string) (*MessageRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, to.String()+"|"+text)
	return &MessageRow{ID: "SENT1", ChatJID: to.String(), Timestamp: time.Unix(1700000000, 0).UTC()}, nil
}
func (f *fakeWA) SendFile(ctx context.Context, to types.JID, file *OutboxFile, caption string, voice bool) (*MessageRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files = append(f.files, file)
	return &MessageRow{ID: "SENT2", ChatJID: to.String(), Timestamp: time.Unix(1700000000, 0).UTC()}, nil
}
func (f *fakeWA) Download(ctx context.Context, m *MessageRow) ([]byte, error) { return f.media, nil }
func (f *fakeWA) ContactName(ctx context.Context, jid string) string          { return f.contacts[jid] }
func (f *fakeWA) AllContacts(ctx context.Context) map[string]string           { return f.contacts }
func (f *fakeWA) QRCode() string                                              { return f.qr }
func (f *fakeWA) RequestPairing()                                             { f.pairReqs++ }
func (f *fakeWA) Logout(ctx context.Context) error                            { f.logouts++; return nil }

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testConfig(t *testing.T) *Config {
	t.Helper()
	dir := t.TempDir()
	return &Config{
		AuthToken: "test-token-0123456789abcdefghijklmnop", SendToken: "send-token-ZYXWVUTSRQPONMLKJIHGFEDCBA98",
		EnableSend: true, SendPerMinute: 6, SendPerDay: 100,
		AllowedRecipients: map[string]bool{"351900000000@s.whatsapp.net": true, "123-456@g.us": true},
		MaxMediaMB:        1, DataDir: filepath.Join(dir, "data"), MediaDir: filepath.Join(dir, "media"), Listen: ":8120",
	}
}

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
