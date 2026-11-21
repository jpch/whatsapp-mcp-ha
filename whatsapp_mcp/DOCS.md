# WhatsApp MCP

Links your personal WhatsApp account as a device and serves it to AI agents as an MCP server.

1. Optional: set `auth_token` (32+ characters). Empty = a generated token, shown on the add-on page.
2. Start the add-on and open the add-on page (**Open Web UI**).
3. On your phone: **WhatsApp → Settings → Linked devices → Link a device**, scan the QR code (also printed in the log).
4. Point your MCP client at `http://<add-on hostname>:8120/mcp` (shown on the page) with the header
   `Authorization: Bearer <token>`.

Files to send go into `/share/whatsapp-mcp/outbox/`. `enable_send: false` makes the server read-only.
`allow_media_download` (off by default) saves received media into `/share/whatsapp-mcp/downloads/`.

Full documentation: https://github.com/jpch/whatsapp-mcp-ha
