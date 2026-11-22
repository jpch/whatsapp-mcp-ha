# WhatsApp MCP

Links your personal WhatsApp account as a device and serves it to AI agents as an MCP server.

1. Choose **Authentication** (`auth_method`):
   - `api_key` (default): set `auth_token` (32+ random characters), the **read-only** token. Empty = a generated token, shown
     **once** on the add-on page.
   - `cloudflare_jwt`: set `cf_team_domain` (your team name) and `cf_access_aud` (the AUD of the Access application
     that issued the JWT). Put this add-on behind that application, or behind an MCP portal that forwards the
     JWT. Callers do not send `auth_token`.
   - `none`: no check at all. **Not recommended.** Only if port 8120 is unreachable from anything you do not trust.
2. Start the add-on and open the add-on page (**Open Web UI**).
3. On your phone: **WhatsApp → Settings → Linked devices → Link a device**, scan the QR code on the page. The QR code is not written to the add-on log.
4. Point your MCP client at `http://<add-on hostname>:8120/mcp` (shown on the page). With `api_key`, send
   `Authorization: Bearer <token>`. With `cloudflare_jwt`, reach the hostname through Cloudflare Access (a service
   token on the client is `CF-Access-Client-Id` / `CF-Access-Client-Secret`; the add-on checks the JWT Cloudflare
   adds, or the same JWT forwarded as `Cf-Access-Token` or `Authorization: Bearer`, not those secrets).

**Sending is off by default.** To let one agent send: set `enable_send: true` and list the numbers or group JIDs it
may write to in `allowed_recipients` (empty = every send is refused). With `api_key`, also give that agent the `send_token`
(set your own, or leave it empty and copy the generated one from the add-on page). Only the send token sees the
send tools. With `cloudflare_jwt` or `none` there is no send token: every accepted caller can send, so keep the
Access policy (or the network) to that one caller. Never put the send token into a shared or portal client. For higher assurance set `send_token` (and
`auth_token`) yourself: a configured token is never shown or replaced from the add-on page.

Files to send go into `/share/whatsapp-mcp/outbox/`. `retention_days` deletes old stored messages (0 = keep).
`allow_media_download` (off by default) saves received media into `/share/whatsapp-mcp/downloads/`.

Risks: messages from anyone can try to trick an agent with send tools into sending (prompt injection), and
WhatsApp can restrict numbers that use unofficial clients.

Full documentation: https://github.com/jpch/whatsapp-mcp-ha
