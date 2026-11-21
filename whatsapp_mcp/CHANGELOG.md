# Changelog

## 0.1.0

- First release: personal WhatsApp account linked as a device (whatsmeow), MCP over streamable HTTP on port 8120
  at `/mcp`, bearer token required (configured or generated), pairing QR code in the log and on an ingress page,
  re-pairing after an unlink.
- Tools: get_status, search_contacts, list_chats, get_chat, get_direct_chat_by_contact, get_contact_chats,
  list_messages, get_message_context, get_last_interaction, send_message, send_file, send_audio_message,
  download_media (off by default).
- Send rate limits, outbox-only file sending, session and messages in `/data`.
