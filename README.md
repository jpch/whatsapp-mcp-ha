# WhatsApp MCP add-on for Home Assistant

A Home Assistant add-on that links **your personal WhatsApp account as a device** (the same mechanism as WhatsApp
Web, through [whatsmeow](https://github.com/tulir/whatsmeow)) and exposes it as an **MCP server over streamable
HTTP**, so AI agents can search and read your chats and send messages and files.

- Personal use only. It does **not** use the WhatsApp Business API. Unofficial clients are against WhatsApp's
  terms of service and can get a number restricted; use it at your own risk and keep sending volumes low.
- One Go binary: the whatsmeow client, the local message store and the MCP server. There is no separate bridge
  REST API, so nothing besides `/mcp` and `/healthz` listens on the exposed port.
- Session and messages are stored in the add-on's `/data` (included in Home Assistant backups).

Independent project, not affiliated with WhatsApp or Meta. The tool set and message store are derived from
[lharries/whatsapp-mcp](https://github.com/lharries/whatsapp-mcp) (MIT).

## Install (Home Assistant OS, aarch64 or amd64)

1. **Settings → Add-ons → Add-on store → ⋮ → Repositories**, add `https://github.com/jpch/whatsapp-mcp-ha`.
2. Install **WhatsApp MCP**. The first install builds the image on the device and takes a few minutes.
3. Optional: in **Configuration**, set `auth_token` (32+ printable characters, e.g. `openssl rand -base64 32`).
   If you leave it empty, the add-on generates one and shows it on its page.
4. **Start** the add-on and open **Open Web UI** (or the sidebar entry **WhatsApp MCP**, administrators only).
5. On your phone: **WhatsApp → Settings → Linked devices → Link a device**, and scan the QR code shown on the page.
   The same QR code is printed in the add-on **Log**. The page shows **Connected** when done.

If the QR codes expire, press **Show a new QR code** on the page (or restart the add-on). If WhatsApp unlinks the
device (from the phone, or after a long time offline), the add-on goes back to showing a QR code. **Unlink this
device** on the page logs out and starts a new pairing.

## Connect an MCP client

| | |
|---|---|
| Transport | streamable HTTP (stateless, JSON responses) |
| Path | `/mcp` |
| Port | `8120` (container port) |
| From other add-ons (e.g. cloudflared) | `http://2b052fb0-whatsapp-mcp:8120/mcp` |
| Header | `Authorization: Bearer <token>` |

The hostname is `<repository hash>-whatsapp-mcp`; the hash is `2b052fb0` when the repository was added as
`https://github.com/jpch/whatsapp-mcp-ha`. The add-on page shows the exact internal URL.

The port is **not published on the host** by default. To use it from the LAN, set a host port under
**Configuration → Network**. To use it from outside, put a tunnel in front (e.g. a Cloudflare Tunnel with the
internal URL above as the service, ideally with Cloudflare Access service tokens in addition to the bearer token).
Do not forward the port on your router.

```json
{ "mcpServers": { "whatsapp": { "type": "http", "url": "https://<your-hostname>/mcp",
  "headers": { "Authorization": "Bearer <token>" } } } }
```

`GET /healthz` answers `{"status":"ok"}` without authentication and without any other detail.

## Tools

| Tool | |
|---|---|
| `get_status` | connection state, limits, outbox folder |
| `search_contacts` | by name or number (address book synced from the phone, plus known chats) |
| `list_chats`, `get_chat`, `get_direct_chat_by_contact`, `get_contact_chats` | chats |
| `list_messages`, `get_message_context`, `get_last_interaction` | messages (search, filters, context) |
| `send_message` | text to a phone number (country code, digits only) or a JID (`…@s.whatsapp.net`, `…@lid`, group `…@g.us`) |
| `send_file` | image, video, audio or document **from the outbox folder only** |
| `send_audio_message` | an Ogg Opus `.ogg` file from the outbox as a voice note |
| `download_media` | only when `allow_media_download` is on; saves into `/share/whatsapp-mcp/downloads` |

Only messages received while the add-on runs, plus what WhatsApp sends in the history sync after pairing, are
searchable.

Limitations in 0.1.0: edits, deletions ("deleted for everyone") and reactions are not applied to stored messages;
voice notes must already be Ogg Opus (no ffmpeg in the image); passkey pairing is not supported (use the QR code).

## Configuration

| Option | Default | |
|---|---|---|
| `auth_token` | empty | bearer token; empty = generated, shown on the add-on page |
| `enable_send` | `true` | `false` hides and disables all send tools (read-only server) |
| `send_rate_per_minute` | `6` | sends per minute, all clients together |
| `send_daily_limit` | `100` | sends per rolling 24 hours |
| `allow_media_download` | `false` | enables `download_media` |
| `max_media_mb` | `16` | largest file sent or downloaded |
| `log_level` | `info` | message contents and tokens are never logged |

**Sending files:** copy them to `/share/whatsapp-mcp/outbox/` (e.g. with the Samba or File editor add-on) and pass
the file name as `media_path`. Paths outside that folder, `..` and symlinks pointing out of it are refused.

## Plain Docker (e.g. a NAS)

```sh
git clone https://github.com/jpch/whatsapp-mcp-ha && cd whatsapp-mcp-ha
docker build -t whatsapp-mcp --build-arg BUILD_FROM=ghcr.io/home-assistant/amd64-base:3.24 whatsapp_mcp
docker run -d --name whatsapp-mcp --restart unless-stopped \
  -p 8120:8120 \
  -v "$PWD/data:/data" -v "$PWD/media:/share/whatsapp-mcp" \
  -e WA_MCP_TOKEN="<32+ random characters>" \
  whatsapp-mcp
docker logs -f whatsapp-mcp   # scan the QR code printed here
```

Use `aarch64-base` on ARM. Without `WA_MCP_TOKEN` a token is generated into `data/auth_token`. Other variables:
`WA_MCP_ENABLE_SEND`, `WA_MCP_SEND_RATE_PER_MINUTE`, `WA_MCP_SEND_DAILY_LIMIT`, `WA_MCP_ALLOW_MEDIA_DOWNLOAD`,
`WA_MCP_MAX_MEDIA_MB`, `WA_MCP_LOG_LEVEL`. The pairing page is off in plain Docker (it has no login of its own).
To re-pair after an unlink, run `docker restart whatsapp-mcp` and scan the new code from the log.

## Security

See [SECURITY.md](SECURITY.md) and [docs/security-review.md](docs/security-review.md). In short: bearer token
required (constant-time check, failed attempts throttled), no other routes on the exposed port, pairing page only
through Home Assistant ingress for administrators, sends rate limited, file sending confined to the outbox,
media download off by default, message contents and tokens never logged, session files readable by the owner only.

Message contents are untrusted input for the agent (prompt injection). Be careful what an agent with send tools
may do on its own, or set `enable_send: false`.

## License

MIT, see [LICENSE](LICENSE). whatsmeow is MPL-2.0 (used unmodified as a Go module).
