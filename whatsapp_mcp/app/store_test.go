package main

import (
	"context"
	"testing"
	"time"
)

func seed(t *testing.T, s *Store) {
	ctx := context.Background()
	base := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	s.UpsertChat(ctx, "111111111@s.whatsapp.net", "Alice", time.Time{})
	s.UpsertChat(ctx, "123-456@g.us", "Family", time.Time{})
	msgs := []MessageRow{
		{ID: "A1", ChatJID: "111111111@s.whatsapp.net", Sender: "111111111@s.whatsapp.net", Content: "hello there", Timestamp: base},
		{ID: "A2", ChatJID: "111111111@s.whatsapp.net", Sender: "999999999@s.whatsapp.net", Content: "hi Alice", Timestamp: base.Add(time.Minute), IsFromMe: true},
		{ID: "G1", ChatJID: "123-456@g.us", Sender: "111111111@s.whatsapp.net", Content: "100% sure_thing", Timestamp: base.Add(2 * time.Minute)},
		{ID: "G2", ChatJID: "123-456@g.us", Sender: "222222222@s.whatsapp.net", Content: "", MediaType: "image", Filename: "image.jpg",
			Timestamp: base.Add(3 * time.Minute), DirectPath: "/x", MediaKey: []byte{1}, FileLength: 10},
		{ID: "E1", ChatJID: "123-456@g.us", Sender: "x@s.whatsapp.net", Timestamp: base},
	}
	for i := range msgs {
		if err := s.StoreMessage(ctx, &msgs[i]); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStoreQueries(t *testing.T) {
	s := testStore(t)
	seed(t, s)
	ctx := context.Background()
	all, _ := s.ListMessages(ctx, ListMessagesParams{Limit: 10})
	if len(all) != 4 || all[0].ID != "G2" {
		t.Fatalf("list all: %+v", all)
	}
	q, _ := s.ListMessages(ctx, ListMessagesParams{Query: "HELLO", Limit: 10})
	if len(q) != 1 || q[0].ChatName != "Alice" {
		t.Fatalf("query: %+v", q)
	}
	// LIKE wildcards are literal.
	if w, _ := s.ListMessages(ctx, ListMessagesParams{Query: "%", Limit: 10}); len(w) != 1 || w[0].ID != "G1" {
		t.Fatalf("wildcard escaping: %+v", w)
	}
	if w, _ := s.ListMessages(ctx, ListMessagesParams{Query: "_", Limit: 10}); len(w) != 1 {
		t.Fatalf("underscore escaping: %+v", w)
	}
	bySender, _ := s.ListMessages(ctx, ListMessagesParams{Sender: "111111111", Limit: 10})
	if len(bySender) != 2 {
		t.Fatalf("sender: %+v", bySender)
	}
	after := time.Date(2026, 11, 1, 10, 1, 30, 0, time.UTC)
	if r, _ := s.ListMessages(ctx, ListMessagesParams{After: &after, Limit: 10}); len(r) != 2 {
		t.Fatalf("after: %+v", r)
	}
	if p, _ := s.ListMessages(ctx, ListMessagesParams{Limit: 2, Page: 1}); len(p) != 2 || p[0].ID != "A2" {
		t.Fatalf("page: %+v", p)
	}
	chats, _ := s.ListChats(ctx, "", 10, 0, true, "last_active")
	if len(chats) != 2 || chats[0].JID != "123-456@g.us" || !chats[0].IsGroup || chats[0].LastMessage != "" || chats[1].LastMessage != "hi Alice" {
		t.Fatalf("chats: %+v", chats)
	}
	byName, _ := s.ListChats(ctx, "", 10, 0, false, "name")
	if byName[0].Name != "Alice" || byName[0].LastMessage != "" {
		t.Fatalf("chats by name: %+v", byName)
	}
	c, err := s.GetMessageContext(ctx, "A2", "", 5, 5)
	if err != nil || len(c.Before) != 1 || len(c.After) != 0 {
		t.Fatalf("context: %+v %v", c, err)
	}
	if _, err := s.GetMessageContext(ctx, "nope", "", 1, 1); err == nil {
		t.Fatal("missing message found")
	}
	cc, _ := s.GetContactChats(ctx, "111111111@s.whatsapp.net", 10, 0)
	if len(cc) != 2 {
		t.Fatalf("contact chats: %+v", cc)
	}
	li, err := s.LastInteraction(ctx, "111111111@s.whatsapp.net")
	if err != nil || li.ID != "G1" {
		t.Fatalf("last interaction: %+v %v", li, err)
	}
	dc, _ := s.DirectChatContacts(ctx, "ali", 10)
	if len(dc) != 1 || dc[0].JID != "111111111@s.whatsapp.net" {
		t.Fatalf("contacts: %+v", dc)
	}
	m, err := s.GetMediaMessage(ctx, "G2", "123-456@g.us")
	if err != nil || m.DirectPath != "/x" {
		t.Fatalf("media: %+v %v", m, err)
	}
	if _, err := s.GetMediaMessage(ctx, "G1", "123-456@g.us"); err == nil {
		t.Fatal("text message returned as media")
	}
	// Name is kept when an update has no name; last time only moves forward.
	s.UpsertChat(ctx, "111111111@s.whatsapp.net", "", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	ch, _ := s.GetChat(ctx, "111111111@s.whatsapp.net", false)
	if ch.Name != "Alice" || ch.LastMessageTime.Year() != 2026 {
		t.Fatalf("upsert: %+v", ch)
	}
}
