package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mdp/qrterminal/v3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// Connection states shown on the add-on page and returned by get_status.
const (
	StateStarting       = "starting"
	StateWaitingForQR   = "waiting_for_qr"
	StatePairingTimeout = "pairing_timeout"
	StateConnecting     = "connecting"
	StateConnected      = "connected"
	StateDisconnected   = "disconnected"
	StateLoggedOut      = "logged_out"
)

// Status is a snapshot of the WhatsApp connection.
type Status struct {
	State     string `json:"state"`
	Paired    bool   `json:"paired"`
	Connected bool   `json:"connected"`
	Account   string `json:"account,omitempty"` // masked phone number
}

// WhatsApp is what the MCP tools need from the WhatsApp connection (faked in tests).
type WhatsApp interface {
	Status() Status
	SendText(ctx context.Context, to types.JID, text string) (*MessageRow, error)
	SendFile(ctx context.Context, to types.JID, f *OutboxFile, caption string, voice bool) (*MessageRow, error)
	Download(ctx context.Context, m *MessageRow) ([]byte, error)
	ContactName(ctx context.Context, jid string) string
	AllContacts(ctx context.Context) map[string]string
}

// Manager owns the whatsmeow client: pairing, re-pairing after logout, and storing messages.
type Manager struct {
	cfg       *Config
	store     *Store
	log       *slog.Logger
	container *sqlstore.Container
	qrOut     io.Writer

	mu     sync.RWMutex
	client *whatsmeow.Client
	state  string
	qr     string

	pairReq   chan struct{}
	loggedOut chan struct{}

	groupMu    sync.Mutex
	groupTried map[string]time.Time // group name lookups, at most one per group per hour
}

func NewManager(ctx context.Context, cfg *Config, store *Store, log *slog.Logger) (*Manager, error) {
	waMin := "WARN"
	if cfg.LogLevel <= slog.LevelDebug {
		waMin = "INFO" // never DEBUG: whatsmeow debug logs can contain message payloads
	}
	dbLog := newWALogger(log.With("module", "whatsmeow-db"), waMin)
	path := filepath.Join(cfg.DataDir, "whatsmeow.db")
	container, err := sqlstore.New(ctx, "sqlite3", "file:"+path+"?_foreign_keys=on&_journal_mode=WAL&_synchronous=NORMAL&_busy_timeout=5000", dbLog)
	if err != nil {
		return nil, fmt.Errorf("open session store: %w", err)
	}
	_ = os.Chmod(path, 0o600)
	return &Manager{
		cfg: cfg, store: store, log: log, container: container, qrOut: os.Stdout,
		state: StateStarting, pairReq: make(chan struct{}, 1), loggedOut: make(chan struct{}, 1),
		groupTried: map[string]time.Time{},
	}, nil
}

func (m *Manager) setState(s string) {
	m.mu.Lock()
	m.state = s
	if s != StateWaitingForQR {
		m.qr = ""
	}
	m.mu.Unlock()
}

func (m *Manager) getClient() *whatsmeow.Client {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.client
}

// Status implements WhatsApp.
func (m *Manager) Status() Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	st := Status{State: m.state}
	if m.client != nil {
		st.Connected = m.client.IsConnected() && m.client.IsLoggedIn()
		if id := m.client.Store.ID; id != nil {
			st.Paired = true
			st.Account = maskPhone(id.User)
		}
	}
	return st
}

// QRCode returns the current pairing code (only for the ingress page).
func (m *Manager) QRCode() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.qr
}

// RequestPairing starts a new QR pairing round after a timeout (from the add-on page).
func (m *Manager) RequestPairing() {
	select {
	case m.pairReq <- struct{}{}:
	default:
	}
}

// Logout unlinks this device and starts a new pairing round.
func (m *Manager) Logout(ctx context.Context) error {
	c := m.getClient()
	if c == nil || c.Store.ID == nil {
		return errors.New("not paired")
	}
	if err := c.Logout(ctx); err != nil {
		return err
	}
	m.signalLoggedOut()
	return nil
}

func (m *Manager) signalLoggedOut() {
	select {
	case m.loggedOut <- struct{}{}:
	default:
	}
}

// Run keeps the client connected, pairing whenever there is no session, until ctx ends.
func (m *Manager) Run(ctx context.Context) {
	waMin := "WARN"
	if m.cfg.LogLevel <= slog.LevelDebug {
		waMin = "INFO"
	}
	for ctx.Err() == nil {
		device, err := m.container.GetFirstDevice(ctx)
		if err != nil {
			m.log.Error("cannot load the WhatsApp session", "error", err)
			sleepCtx(ctx, 30*time.Second)
			continue
		}
		client := whatsmeow.NewClient(device, newWALogger(m.log.With("module", "whatsmeow"), waMin))
		client.AddEventHandler(func(evt any) { m.handleEvent(client, evt) })
		// A logout signal from a previous client must not end this one.
		drain(m.loggedOut)
		m.mu.Lock()
		m.client = client
		m.mu.Unlock()

		if device.ID == nil {
			if !m.pair(ctx, client) {
				client.Disconnect()
				select {
				case <-ctx.Done():
					return
				case <-m.pairReq:
					continue
				}
			}
		} else {
			m.setState(StateConnecting)
			if err := client.Connect(); err != nil {
				m.log.Warn("cannot connect to WhatsApp yet, retrying", "error", err)
				client.Disconnect()
				sleepCtx(ctx, 30*time.Second)
				continue
			}
		}
		select {
		case <-ctx.Done():
			client.Disconnect()
			return
		case <-m.loggedOut:
			client.Disconnect()
			m.setState(StateLoggedOut)
			m.log.Warn("this device was unlinked from WhatsApp; showing a new pairing QR code")
		}
	}
}

// pair runs one QR pairing round. It returns true when the phone linked this device.
func (m *Manager) pair(ctx context.Context, client *whatsmeow.Client) bool {
	// A request made before this round (e.g. a stale click) must not skip the next wait.
	drain(m.pairReq)
	qrCh, err := client.GetQRChannel(ctx)
	if err != nil {
		m.log.Error("cannot start pairing", "error", err)
		m.setState(StatePairingTimeout)
		return false
	}
	if err := client.Connect(); err != nil {
		m.log.Error("cannot connect to WhatsApp for pairing", "error", err)
		m.setState(StatePairingTimeout)
		return false
	}
	for item := range qrCh {
		switch item.Event {
		case whatsmeow.QRChannelEventCode:
			m.mu.Lock()
			m.state, m.qr = StateWaitingForQR, item.Code
			m.mu.Unlock()
			m.log.Info("not linked: scan this QR code in WhatsApp > Settings > Linked devices > Link a device (also shown on the add-on page)",
				"valid_for", item.Timeout.String())
			qrterminal.GenerateWithConfig(item.Code, qrterminal.Config{
				Level: qrterminal.L, Writer: m.qrOut, HalfBlocks: true,
				BlackChar: qrterminal.BLACK_BLACK, WhiteChar: qrterminal.WHITE_WHITE,
				BlackWhiteChar: qrterminal.BLACK_WHITE, WhiteBlackChar: qrterminal.WHITE_BLACK, QuietZone: 2,
			})
		case "success":
			if !(client.IsConnected() && client.IsLoggedIn()) {
				m.setState(StateConnecting)
			} else {
				m.setState(StateConnected)
			}
			m.log.Info("linked to WhatsApp")
			return true
		case whatsmeow.QRChannelEventPasskeyRequest:
			m.log.Warn("the phone asked for passkey pairing, which this add-on does not support; scan the QR code instead")
		case "timeout":
			m.setState(StatePairingTimeout)
			m.log.Warn("the QR codes expired; open the add-on page and press 'Show a new QR code', or restart the add-on")
			return false
		default:
			m.setState(StatePairingTimeout)
			if item.Error != nil {
				m.log.Error("pairing failed", "event", item.Event, "error", item.Error)
			} else {
				m.log.Error("pairing failed", "event", item.Event)
			}
			return false
		}
	}
	m.setState(StatePairingTimeout)
	return false
}

func (m *Manager) handleEvent(client *whatsmeow.Client, evt any) {
	if client != m.getClient() {
		return // event from a replaced client
	}
	timeout := 2 * time.Minute
	if _, ok := evt.(*events.HistorySync); ok {
		timeout = 30 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	switch e := evt.(type) {
	case *events.Message:
		m.storeMessage(ctx, e)
	case *events.HistorySync:
		m.storeHistory(ctx, e)
	case *events.Connected:
		m.setState(StateConnected)
		m.log.Info("connected to WhatsApp")
	case *events.Disconnected:
		m.setState(StateDisconnected)
		m.log.Info("disconnected from WhatsApp, reconnecting")
	case *events.LoggedOut:
		m.log.Warn("WhatsApp logged this device out", "reason", e.Reason.String())
		m.signalLoggedOut()
	case *events.StreamReplaced:
		m.log.Error("another client connected with this session; stop the other instance")
	case *events.ClientOutdated:
		m.log.Error("WhatsApp rejected this client version; update the add-on")
	}
}

func (m *Manager) storeHistory(ctx context.Context, e *events.HistorySync) {
	c := m.getClient()
	if c == nil || e.Data == nil {
		return
	}
	stored := 0
	for _, conv := range e.Data.GetConversations() {
		chatJID, err := types.ParseJID(conv.GetID())
		if err != nil {
			continue
		}
		name := conv.GetDisplayName()
		if name == "" {
			name = conv.GetName()
		}
		canon := m.canonical(ctx, chatJID, types.EmptyJID)
		if name != "" {
			_ = m.store.UpsertChat(ctx, canon.String(), name, time.Time{})
		}
		for _, hm := range conv.GetMessages() {
			if hm.GetMessage() == nil {
				continue
			}
			evt, err := c.ParseWebMessage(chatJID, hm.GetMessage())
			if err != nil {
				continue
			}
			if m.storeMessage(ctx, evt) {
				stored++
			}
		}
	}
	m.log.Debug("history sync stored", "messages", stored)
}

// canonical prefers the phone-number JID over a LID when the mapping is known.
func (m *Manager) canonical(ctx context.Context, jid, alt types.JID) types.JID {
	jid = jid.ToNonAD()
	if jid.Server != types.HiddenUserServer {
		return jid
	}
	if alt.Server == types.DefaultUserServer {
		return alt.ToNonAD()
	}
	if c := m.getClient(); c != nil {
		if pn, err := c.Store.LIDs.GetPNForLID(ctx, jid); err == nil && !pn.IsEmpty() {
			return pn.ToNonAD()
		}
	}
	return jid
}

// storeMessage saves a message (live or history). Returns true if stored.
func (m *Manager) storeMessage(ctx context.Context, e *events.Message) bool {
	if e.Info.Chat.Server == types.BroadcastServer || e.Info.Chat.Server == types.NewsletterServer || e.IsEdit {
		return false
	}
	chatAlt := e.Info.SenderAlt
	if e.Info.IsFromMe {
		chatAlt = e.Info.RecipientAlt
	}
	chat := m.canonical(ctx, e.Info.Chat, chatAlt)
	sender := m.canonical(ctx, e.Info.Sender, e.Info.SenderAlt)
	row := &MessageRow{
		ID: e.Info.ID, ChatJID: chat.String(), Sender: sender.String(),
		Timestamp: e.Info.Timestamp, IsFromMe: e.Info.IsFromMe,
	}
	extractContent(e.Message, row)
	if row.Content == "" && row.MediaType == "" {
		return false
	}
	name := ""
	if !e.Info.IsGroup {
		name = m.ContactName(ctx, chat.String())
		if name == "" && !e.Info.IsFromMe && e.Info.PushName != "" {
			name = e.Info.PushName
		}
	} else if m.store.ChatName(ctx, chat.String()) == "" && m.shouldLookUpGroup(chat.String()) {
		if c := m.getClient(); c != nil && e.SourceWebMsg == nil {
			gctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			if gi, err := c.GetGroupInfo(gctx, e.Info.Chat); err == nil {
				name = gi.Name
			}
			cancel()
		}
	}
	if err := m.store.UpsertChat(ctx, row.ChatJID, name, row.Timestamp); err != nil {
		m.log.Warn("cannot store chat", "error", err)
		return false
	}
	if err := m.store.StoreMessage(ctx, row); err != nil {
		m.log.Warn("cannot store message", "error", err)
		return false
	}
	return true
}

func (m *Manager) shouldLookUpGroup(jid string) bool {
	m.groupMu.Lock()
	defer m.groupMu.Unlock()
	if t, ok := m.groupTried[jid]; ok && time.Since(t) < time.Hour {
		return false
	}
	if len(m.groupTried) > 5000 {
		m.groupTried = map[string]time.Time{}
	}
	m.groupTried[jid] = time.Now()
	return true
}

func drain(ch chan struct{}) {
	select {
	case <-ch:
	default:
	}
}

type mediaFields interface {
	GetURL() string
	GetDirectPath() string
	GetMediaKey() []byte
	GetFileSHA256() []byte
	GetFileEncSHA256() []byte
	GetFileLength() uint64
	GetMimetype() string
}

func setMedia(row *MessageRow, kind string, f mediaFields) {
	row.MediaType = kind
	row.URL, row.DirectPath = f.GetURL(), f.GetDirectPath()
	row.MediaKey, row.FileSHA256, row.FileEncSHA256 = f.GetMediaKey(), f.GetFileSHA256(), f.GetFileEncSHA256()
	row.FileLength, row.Mimetype = f.GetFileLength(), f.GetMimetype()
}

// extractContent fills text and media fields from a (unwrapped) message.
func extractContent(msg *waE2E.Message, row *MessageRow) {
	if msg == nil {
		return
	}
	switch {
	case msg.GetConversation() != "":
		row.Content = msg.GetConversation()
	case msg.GetExtendedTextMessage() != nil:
		row.Content = msg.GetExtendedTextMessage().GetText()
	case msg.GetImageMessage() != nil:
		im := msg.GetImageMessage()
		row.Content = im.GetCaption()
		setMedia(row, "image", im)
		row.Filename = "image" + extFor(im.GetMimetype())
	case msg.GetVideoMessage() != nil:
		vm := msg.GetVideoMessage()
		row.Content = vm.GetCaption()
		setMedia(row, "video", vm)
		row.Filename = "video" + extFor(vm.GetMimetype())
	case msg.GetAudioMessage() != nil:
		am := msg.GetAudioMessage()
		setMedia(row, "audio", am)
		row.Filename = "audio" + extFor(am.GetMimetype())
	case msg.GetDocumentMessage() != nil:
		dm := msg.GetDocumentMessage()
		row.Content = dm.GetCaption()
		setMedia(row, "document", dm)
		row.Filename = safeFileName(dm.GetFileName())
	case msg.GetStickerMessage() != nil:
		setMedia(row, "sticker", msg.GetStickerMessage())
		row.Filename = "sticker.webp"
	case msg.GetLocationMessage() != nil:
		lm := msg.GetLocationMessage()
		row.Content = strings.TrimSpace(fmt.Sprintf("[location %.6f,%.6f] %s %s", lm.GetDegreesLatitude(), lm.GetDegreesLongitude(), lm.GetName(), lm.GetAddress()))
	case msg.GetContactMessage() != nil:
		row.Content = "[contact] " + msg.GetContactMessage().GetDisplayName()
	}
}

func extFor(mime string) string {
	mime = strings.TrimSpace(strings.SplitN(mime, ";", 2)[0])
	switch mime {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	case "video/mp4":
		return ".mp4"
	case "audio/ogg":
		return ".ogg"
	case "audio/mpeg":
		return ".mp3"
	case "audio/mp4":
		return ".m4a"
	case "application/pdf":
		return ".pdf"
	}
	return ".bin"
}

// ContactName implements WhatsApp: the best known name for a user JID.
func (m *Manager) ContactName(ctx context.Context, jid string) string {
	c := m.getClient()
	if c == nil {
		return ""
	}
	j, err := types.ParseJID(jid)
	if err != nil || j.Server == types.GroupServer {
		return ""
	}
	info, err := c.Store.Contacts.GetContact(ctx, j)
	if err != nil || !info.Found {
		return ""
	}
	return firstNonEmpty(info.FullName, info.FirstName, info.BusinessName, info.PushName)
}

// AllContacts implements WhatsApp: JID -> name from the phone's address book sync.
func (m *Manager) AllContacts(ctx context.Context) map[string]string {
	out := map[string]string{}
	c := m.getClient()
	if c == nil {
		return out
	}
	all, err := c.Store.Contacts.GetAllContacts(ctx)
	if err != nil {
		return out
	}
	for jid, info := range all {
		if name := firstNonEmpty(info.FullName, info.FirstName, info.BusinessName, info.PushName); name != "" {
			out[m.canonical(ctx, jid, types.EmptyJID).String()] = name
		}
	}
	return out
}

func (m *Manager) connectedClient() (*whatsmeow.Client, error) {
	c := m.getClient()
	if c == nil || c.Store.ID == nil {
		return nil, errors.New("WhatsApp is not linked yet; scan the QR code on the add-on page")
	}
	if !c.IsConnected() || !c.IsLoggedIn() {
		return nil, errors.New("not connected to WhatsApp right now; try again shortly")
	}
	return c, nil
}

func (m *Manager) recordSent(ctx context.Context, c *whatsmeow.Client, to types.JID, id types.MessageID, ts time.Time, row *MessageRow) *MessageRow {
	row.ID, row.ChatJID, row.Sender, row.Timestamp, row.IsFromMe = id, to.String(), c.Store.ID.ToNonAD().String(), ts, true
	if err := m.store.UpsertChat(ctx, row.ChatJID, m.ContactName(ctx, row.ChatJID), ts); err == nil {
		if err := m.store.StoreMessage(ctx, row); err != nil {
			m.log.Warn("cannot store sent message", "error", err)
		}
	}
	return row
}

// SendText implements WhatsApp.
func (m *Manager) SendText(ctx context.Context, to types.JID, text string) (*MessageRow, error) {
	c, err := m.connectedClient()
	if err != nil {
		return nil, err
	}
	resp, err := c.SendMessage(ctx, to, &waE2E.Message{Conversation: proto.String(text)})
	if err != nil {
		return nil, fmt.Errorf("send failed: %w", err)
	}
	return m.recordSent(ctx, c, to, resp.ID, resp.Timestamp, &MessageRow{Content: text}), nil
}

// SendFile implements WhatsApp.
func (m *Manager) SendFile(ctx context.Context, to types.JID, f *OutboxFile, caption string, voice bool) (*MessageRow, error) {
	c, err := m.connectedClient()
	if err != nil {
		return nil, err
	}
	var mt whatsmeow.MediaType
	switch f.Kind {
	case KindImage:
		mt = whatsmeow.MediaImage
	case KindVideo:
		mt = whatsmeow.MediaVideo
	case KindAudio:
		mt = whatsmeow.MediaAudio
	default:
		mt = whatsmeow.MediaDocument
	}
	up, err := c.Upload(ctx, f.Data, mt)
	if err != nil {
		return nil, fmt.Errorf("upload failed: %w", err)
	}
	length := up.FileLength
	msg := &waE2E.Message{}
	switch f.Kind {
	case KindImage:
		msg.ImageMessage = &waE2E.ImageMessage{Caption: optString(caption), Mimetype: proto.String(f.Mime), URL: &up.URL,
			DirectPath: &up.DirectPath, MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: &length}
	case KindVideo:
		msg.VideoMessage = &waE2E.VideoMessage{Caption: optString(caption), Mimetype: proto.String(f.Mime), URL: &up.URL,
			DirectPath: &up.DirectPath, MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: &length}
	case KindAudio:
		secs, wave, err := analyzeOggOpus(f.Data)
		if err != nil {
			return nil, err
		}
		am := &waE2E.AudioMessage{Mimetype: proto.String(f.Mime), URL: &up.URL, DirectPath: &up.DirectPath, MediaKey: up.MediaKey,
			FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: &length, Seconds: proto.Uint32(secs)}
		if voice {
			am.PTT, am.Waveform = proto.Bool(true), wave
		}
		msg.AudioMessage = am
	default:
		msg.DocumentMessage = &waE2E.DocumentMessage{Caption: optString(caption), Title: proto.String(f.Name), FileName: proto.String(f.Name),
			Mimetype: proto.String(f.Mime), URL: &up.URL, DirectPath: &up.DirectPath, MediaKey: up.MediaKey,
			FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: &length}
	}
	resp, err := c.SendMessage(ctx, to, msg)
	if err != nil {
		return nil, fmt.Errorf("send failed: %w", err)
	}
	return m.recordSent(ctx, c, to, resp.ID, resp.Timestamp, &MessageRow{Content: caption, MediaType: string(f.Kind), Filename: f.Name}), nil
}

// Download implements WhatsApp: fetches and decrypts a stored media message.
func (m *Manager) Download(ctx context.Context, row *MessageRow) ([]byte, error) {
	c, err := m.connectedClient()
	if err != nil {
		return nil, err
	}
	if row.DirectPath == "" && row.URL == "" {
		return nil, errors.New("this message has no downloadable media")
	}
	length := row.FileLength
	var dl whatsmeow.DownloadableMessage
	switch row.MediaType {
	case "image":
		dl = &waE2E.ImageMessage{URL: &row.URL, DirectPath: &row.DirectPath, MediaKey: row.MediaKey, FileSHA256: row.FileSHA256, FileEncSHA256: row.FileEncSHA256, FileLength: &length}
	case "video":
		dl = &waE2E.VideoMessage{URL: &row.URL, DirectPath: &row.DirectPath, MediaKey: row.MediaKey, FileSHA256: row.FileSHA256, FileEncSHA256: row.FileEncSHA256, FileLength: &length}
	case "audio":
		dl = &waE2E.AudioMessage{URL: &row.URL, DirectPath: &row.DirectPath, MediaKey: row.MediaKey, FileSHA256: row.FileSHA256, FileEncSHA256: row.FileEncSHA256, FileLength: &length}
	case "sticker":
		dl = &waE2E.StickerMessage{URL: &row.URL, DirectPath: &row.DirectPath, MediaKey: row.MediaKey, FileSHA256: row.FileSHA256, FileEncSHA256: row.FileEncSHA256, FileLength: &length}
	default:
		dl = &waE2E.DocumentMessage{URL: &row.URL, DirectPath: &row.DirectPath, MediaKey: row.MediaKey, FileSHA256: row.FileSHA256, FileEncSHA256: row.FileEncSHA256, FileLength: &length}
	}
	return c.Download(ctx, dl)
}

func optString(s string) *string {
	if s == "" {
		return nil
	}
	return proto.String(s)
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

func maskPhone(user string) string {
	if len(user) <= 5 {
		return "+" + strings.Repeat("•", len(user))
	}
	return "+" + user[:3] + strings.Repeat("•", len(user)-5) + user[len(user)-2:]
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// waLogger adapts whatsmeow logging to slog with a minimum level. Debug is always dropped.
type waLogger struct {
	l   *slog.Logger
	min int
}

var waLevels = map[string]int{"DEBUG": 0, "INFO": 1, "WARN": 2, "ERROR": 3}

func newWALogger(l *slog.Logger, min string) waLog.Logger {
	return &waLogger{l: l, min: waLevels[min]}
}

func (w *waLogger) Debugf(string, ...any) {}
func (w *waLogger) Infof(msg string, args ...any) {
	if w.min <= 1 {
		w.l.Info(fmt.Sprintf(msg, args...))
	}
}
func (w *waLogger) Warnf(msg string, args ...any) {
	if w.min <= 2 {
		w.l.Warn(fmt.Sprintf(msg, args...))
	}
}
func (w *waLogger) Errorf(msg string, args ...any) { w.l.Error(fmt.Sprintf(msg, args...)) }
func (w *waLogger) Sub(module string) waLog.Logger {
	return &waLogger{l: w.l.With("sub", module), min: w.min}
}
