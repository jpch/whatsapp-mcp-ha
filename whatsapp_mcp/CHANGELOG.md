# Changelog

## 0.1.0

- First release: personal WhatsApp account linked as a device (whatsmeow), MCP over streamable HTTP on port 8120
  at `/mcp`, bearer token required (configured or generated), pairing QR code in the log and on an ingress page,
  re-pairing after an unlink.
- Tools: get_status, search_contacts, list_chats, get_chat, get_direct_chat_by_contact, get_contact_chats,
  list_messages, get_message_context, get_last_interaction, send_message, send_file, send_audio_message,
  download_media (off by default).
- Text written by other people (message text, captions, file names, contact, chat and group names) is returned in
  an `untrusted` object.
- Sending: `allowed_recipients` (optional, recommended), rate limits (6 per minute, 50 per 24 hours by default),
  files only from the outbox folder.
- Media downloads from WhatsApp's servers are capped at `max_media_mb` while streaming.
- `retention_days` (default 0 = keep) deletes old messages every 6 hours and frees the space.
- Generated token shown once on the add-on page, with token rotation; weak configured tokens are refused.
- AppArmor profile: the server may only write to `/data` and `/share/whatsapp-mcp`.
- Watchdog on `/healthz`; an invalid configuration stops the add-on instead of restarting it in a loop.
- Base images pinned by digest.
