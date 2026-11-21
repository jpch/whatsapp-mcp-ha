package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// Store is the local message database (chats and messages seen by this linked device).
// Derived from the schema of lharries/whatsapp-mcp (MIT).
type Store struct {
	db *sql.DB
}

// MessageRow is a stored message, including the media keys needed to download it later.
type MessageRow struct {
	ID        string    `json:"id"`
	ChatJID   string    `json:"chat_jid"`
	ChatName  string    `json:"chat_name,omitempty"`
	Sender    string    `json:"sender"`
	Content   string    `json:"content"`
	Timestamp time.Time `json:"timestamp"`
	IsFromMe  bool      `json:"is_from_me"`
	MediaType string    `json:"media_type,omitempty"`
	Filename  string    `json:"filename,omitempty"`

	URL           string `json:"-"`
	DirectPath    string `json:"-"`
	MediaKey      []byte `json:"-"`
	FileSHA256    []byte `json:"-"`
	FileEncSHA256 []byte `json:"-"`
	FileLength    uint64 `json:"-"`
	Mimetype      string `json:"-"`
}

// ChatRow is a chat with optional last message.
type ChatRow struct {
	JID             string     `json:"jid"`
	Name            string     `json:"name,omitempty"`
	IsGroup         bool       `json:"is_group"`
	LastMessageTime *time.Time `json:"last_message_time,omitempty"`
	LastMessage     string     `json:"last_message,omitempty"`
	LastSender      string     `json:"last_sender,omitempty"`
	LastIsFromMe    *bool      `json:"last_is_from_me,omitempty"`
}

const schema = `
CREATE TABLE IF NOT EXISTS chats (
	jid TEXT PRIMARY KEY,
	name TEXT NOT NULL DEFAULT '',
	last_message_time TIMESTAMP
);
CREATE TABLE IF NOT EXISTS messages (
	id TEXT NOT NULL,
	chat_jid TEXT NOT NULL,
	sender TEXT NOT NULL DEFAULT '',
	content TEXT NOT NULL DEFAULT '',
	timestamp TIMESTAMP NOT NULL,
	is_from_me BOOLEAN NOT NULL DEFAULT 0,
	media_type TEXT NOT NULL DEFAULT '',
	filename TEXT NOT NULL DEFAULT '',
	url TEXT NOT NULL DEFAULT '',
	direct_path TEXT NOT NULL DEFAULT '',
	media_key BLOB,
	file_sha256 BLOB,
	file_enc_sha256 BLOB,
	file_length INTEGER NOT NULL DEFAULT 0,
	mimetype TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (id, chat_jid),
	FOREIGN KEY (chat_jid) REFERENCES chats(jid)
);
CREATE INDEX IF NOT EXISTS idx_messages_chat_time ON messages(chat_jid, timestamp);
CREATE INDEX IF NOT EXISTS idx_messages_time ON messages(timestamp);
CREATE INDEX IF NOT EXISTS idx_messages_sender ON messages(sender);
`

// OpenStore opens (and creates) the message database at path.
func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite3", "file:"+path+"?_foreign_keys=on&_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// UpsertChat creates the chat or updates its name (when non-empty) and last message time (when newer).
func (s *Store) UpsertChat(ctx context.Context, jid, name string, last time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO chats (jid, name, last_message_time) VALUES (?, ?, ?)
		ON CONFLICT(jid) DO UPDATE SET
			name = CASE WHEN excluded.name != '' THEN excluded.name ELSE chats.name END,
			last_message_time = CASE
				WHEN chats.last_message_time IS NULL OR excluded.last_message_time > chats.last_message_time
				THEN excluded.last_message_time ELSE chats.last_message_time END`,
		jid, name, nullTime(last))
	return err
}

// ChatName returns the stored name of a chat, or "" if unknown.
func (s *Store) ChatName(ctx context.Context, jid string) string {
	var name string
	_ = s.db.QueryRowContext(ctx, `SELECT name FROM chats WHERE jid = ?`, jid).Scan(&name)
	return name
}

// StoreMessage inserts or replaces a message. Messages with neither text nor media are skipped.
func (s *Store) StoreMessage(ctx context.Context, m *MessageRow) error {
	if m.Content == "" && m.MediaType == "" {
		return nil
	}
	if err := s.UpsertChat(ctx, m.ChatJID, "", m.Timestamp); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT OR REPLACE INTO messages
		(id, chat_jid, sender, content, timestamp, is_from_me, media_type, filename, url, direct_path,
		 media_key, file_sha256, file_enc_sha256, file_length, mimetype)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.ChatJID, m.Sender, m.Content, m.Timestamp.UTC(), m.IsFromMe, m.MediaType, m.Filename,
		m.URL, m.DirectPath, m.MediaKey, m.FileSHA256, m.FileEncSHA256, m.FileLength, m.Mimetype)
	return err
}

// ListMessagesParams filters list_messages.
type ListMessagesParams struct {
	After, Before *time.Time
	Sender        string // JID or phone number
	ChatJID       string
	Query         string
	Limit, Page   int
}

const msgCols = `m.id, m.chat_jid, COALESCE(c.name, ''), m.sender, m.content, m.timestamp, m.is_from_me, m.media_type, m.filename`

func scanMessages(rows *sql.Rows) ([]MessageRow, error) {
	defer rows.Close()
	out := []MessageRow{}
	for rows.Next() {
		var m MessageRow
		if err := rows.Scan(&m.ID, &m.ChatJID, &m.ChatName, &m.Sender, &m.Content, &m.Timestamp, &m.IsFromMe, &m.MediaType, &m.Filename); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// escapeLike escapes LIKE wildcards in user input (used with ESCAPE '\').
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// ListMessages returns matching messages, newest first.
func (s *Store) ListMessages(ctx context.Context, p ListMessagesParams) ([]MessageRow, error) {
	var where []string
	var args []any
	if p.After != nil {
		where = append(where, "m.timestamp > ?")
		args = append(args, p.After.UTC())
	}
	if p.Before != nil {
		where = append(where, "m.timestamp < ?")
		args = append(args, p.Before.UTC())
	}
	if p.Sender != "" {
		where = append(where, `(m.sender = ? OR m.sender LIKE ? ESCAPE '\')`)
		args = append(args, p.Sender, escapeLike(userPart(p.Sender))+"@%")
	}
	if p.ChatJID != "" {
		where = append(where, "m.chat_jid = ?")
		args = append(args, p.ChatJID)
	}
	if p.Query != "" {
		where = append(where, `LOWER(m.content) LIKE LOWER(?) ESCAPE '\'`)
		args = append(args, "%"+escapeLike(p.Query)+"%")
	}
	q := `SELECT ` + msgCols + ` FROM messages m LEFT JOIN chats c ON m.chat_jid = c.jid`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY m.timestamp DESC LIMIT ? OFFSET ?"
	args = append(args, p.Limit, p.Limit*p.Page)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return scanMessages(rows)
}

// MessageContext is a message with surrounding messages in the same chat.
type MessageContext struct {
	Message MessageRow   `json:"message"`
	Before  []MessageRow `json:"before"`
	After   []MessageRow `json:"after"`
}

// GetMessageContext returns before/after messages around messageID (optionally restricted to a chat).
func (s *Store) GetMessageContext(ctx context.Context, messageID, chatJID string, before, after int) (*MessageContext, error) {
	q := `SELECT ` + msgCols + ` FROM messages m LEFT JOIN chats c ON m.chat_jid = c.jid WHERE m.id = ?`
	args := []any{messageID}
	if chatJID != "" {
		q += " AND m.chat_jid = ?"
		args = append(args, chatJID)
	}
	rows, err := s.db.QueryContext(ctx, q+" LIMIT 1", args...)
	if err != nil {
		return nil, err
	}
	found, err := scanMessages(rows)
	if err != nil {
		return nil, err
	}
	if len(found) == 0 {
		return nil, errors.New("message not found")
	}
	target := found[0]
	res := &MessageContext{Message: target}
	rows, err = s.db.QueryContext(ctx, `SELECT `+msgCols+` FROM messages m LEFT JOIN chats c ON m.chat_jid = c.jid
		WHERE m.chat_jid = ? AND m.timestamp < ? ORDER BY m.timestamp DESC LIMIT ?`, target.ChatJID, target.Timestamp.UTC(), before)
	if err != nil {
		return nil, err
	}
	if res.Before, err = scanMessages(rows); err != nil {
		return nil, err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT `+msgCols+` FROM messages m LEFT JOIN chats c ON m.chat_jid = c.jid
		WHERE m.chat_jid = ? AND m.timestamp > ? ORDER BY m.timestamp ASC LIMIT ?`, target.ChatJID, target.Timestamp.UTC(), after)
	if err != nil {
		return nil, err
	}
	if res.After, err = scanMessages(rows); err != nil {
		return nil, err
	}
	return res, nil
}

const chatCols = `c.jid, c.name, c.last_message_time, lm.content, lm.sender, lm.is_from_me`
const chatJoin = ` FROM chats c LEFT JOIN messages lm ON lm.rowid = (
	SELECT rowid FROM messages WHERE chat_jid = c.jid ORDER BY timestamp DESC LIMIT 1)`

func scanChats(rows *sql.Rows, includeLast bool) ([]ChatRow, error) {
	defer rows.Close()
	out := []ChatRow{}
	for rows.Next() {
		var c ChatRow
		var last sql.NullTime
		var content, sender sql.NullString
		var fromMe sql.NullBool
		if err := rows.Scan(&c.JID, &c.Name, &last, &content, &sender, &fromMe); err != nil {
			return nil, err
		}
		if last.Valid {
			t := last.Time
			c.LastMessageTime = &t
		}
		if includeLast {
			c.LastMessage, c.LastSender = content.String, sender.String
			if fromMe.Valid {
				b := fromMe.Bool
				c.LastIsFromMe = &b
			}
		}
		c.IsGroup = strings.HasSuffix(c.JID, "@g.us")
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListChats lists chats filtered by name/JID, sorted by last activity or name.
func (s *Store) ListChats(ctx context.Context, query string, limit, page int, includeLast bool, sortBy string) ([]ChatRow, error) {
	q := `SELECT ` + chatCols + chatJoin
	var args []any
	if query != "" {
		q += ` WHERE (LOWER(c.name) LIKE LOWER(?) ESCAPE '\' OR c.jid LIKE ? ESCAPE '\')`
		pat := "%" + escapeLike(query) + "%"
		args = append(args, pat, pat)
	}
	if sortBy == "name" {
		q += " ORDER BY c.name COLLATE NOCASE ASC"
	} else {
		q += " ORDER BY c.last_message_time DESC"
	}
	q += " LIMIT ? OFFSET ?"
	args = append(args, limit, limit*page)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return scanChats(rows, includeLast)
}

// GetChat returns one chat by JID.
func (s *Store) GetChat(ctx context.Context, jid string, includeLast bool) (*ChatRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+chatCols+chatJoin+` WHERE c.jid = ?`, jid)
	if err != nil {
		return nil, err
	}
	chats, err := scanChats(rows, includeLast)
	if err != nil {
		return nil, err
	}
	if len(chats) == 0 {
		return nil, errors.New("chat not found")
	}
	return &chats[0], nil
}

// GetDirectChatByPhone finds the 1:1 chat with a phone number.
func (s *Store) GetDirectChatByPhone(ctx context.Context, phone string) (*ChatRow, error) {
	return s.GetChat(ctx, phone+"@s.whatsapp.net", true)
}

// GetContactChats lists chats where the contact sent a message or that are the direct chat.
func (s *Store) GetContactChats(ctx context.Context, jid string, limit, page int) ([]ChatRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+chatCols+chatJoin+`
		WHERE c.jid = ? OR c.jid IN (SELECT DISTINCT chat_jid FROM messages WHERE sender = ?)
		ORDER BY c.last_message_time DESC LIMIT ? OFFSET ?`, jid, jid, limit, limit*page)
	if err != nil {
		return nil, err
	}
	return scanChats(rows, true)
}

// LastInteraction returns the newest message sent by the contact or in their direct chat.
func (s *Store) LastInteraction(ctx context.Context, jid string) (*MessageRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+msgCols+` FROM messages m LEFT JOIN chats c ON m.chat_jid = c.jid
		WHERE m.sender = ? OR m.chat_jid = ? ORDER BY m.timestamp DESC LIMIT 1`, jid, jid)
	if err != nil {
		return nil, err
	}
	msgs, err := scanMessages(rows)
	if err != nil {
		return nil, err
	}
	if len(msgs) == 0 {
		return nil, errors.New("no messages found for this contact")
	}
	return &msgs[0], nil
}

// DirectChatContacts searches 1:1 chats by name or JID (contacts known from chats).
func (s *Store) DirectChatContacts(ctx context.Context, query string, limit int) ([]ChatRow, error) {
	pat := "%" + escapeLike(query) + "%"
	rows, err := s.db.QueryContext(ctx, `SELECT `+chatCols+chatJoin+`
		WHERE (LOWER(c.name) LIKE LOWER(?) ESCAPE '\' OR c.jid LIKE ? ESCAPE '\')
		AND c.jid NOT LIKE '%@g.us' ORDER BY c.name COLLATE NOCASE LIMIT ?`, pat, pat, limit)
	if err != nil {
		return nil, err
	}
	return scanChats(rows, false)
}

// GetMediaMessage returns the full row (with media keys) for a media message.
func (s *Store) GetMediaMessage(ctx context.Context, id, chatJID string) (*MessageRow, error) {
	var m MessageRow
	err := s.db.QueryRowContext(ctx, `SELECT id, chat_jid, media_type, filename, url, direct_path, media_key,
		file_sha256, file_enc_sha256, file_length, mimetype FROM messages WHERE id = ? AND chat_jid = ?`, id, chatJID).
		Scan(&m.ID, &m.ChatJID, &m.MediaType, &m.Filename, &m.URL, &m.DirectPath, &m.MediaKey,
			&m.FileSHA256, &m.FileEncSHA256, &m.FileLength, &m.Mimetype)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errors.New("message not found")
	}
	if err != nil {
		return nil, err
	}
	if m.MediaType == "" {
		return nil, errors.New("message has no media")
	}
	return &m, nil
}

// Counts returns the number of chats and messages (for the status page).
func (s *Store) Counts(ctx context.Context) (chats, messages int) {
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM chats`).Scan(&chats)
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages`).Scan(&messages)
	return
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}

func userPart(jidOrPhone string) string {
	if i := strings.IndexByte(jidOrPhone, '@'); i >= 0 {
		return jidOrPhone[:i]
	}
	return jidOrPhone
}
