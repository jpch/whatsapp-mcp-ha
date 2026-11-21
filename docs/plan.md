# Technical plan (0.1.0)

## Goal

Personal WhatsApp account, linked as a device, served to remote AI agents as an MCP server over streamable HTTP,
packaged as a Home Assistant add-on (HAOS on a Raspberry Pi, aarch64; amd64 for CI and x86 hosts), with a plain
Docker alternative. Reachable by other add-ons (a Cloudflare tunnel) by add-on hostname.

## Upstream choice

| Option | Licence | Activity (Nov 2026) | Verdict |
|---|---|---|---|
| lharries/whatsapp-mcp (Go bridge + Python MCP) | MIT | last push Jul 2025, ~280 open issues, whatsmeow from Mar 2025 | base for the tool set and schema, not forked as-is |
| aldinokemal/go-whatsapp-web-multidevice | MIT | very active, has an MCP mode | large REST gateway (groups, webhooks, UI, basic auth); much wider surface than needed |
| tulir/whatsmeow | MPL-2.0 | active (weekly) | the WhatsApp library used directly |

lharries/whatsapp-mcp is unmaintained and its design needs a Python process reading the bridge's SQLite file plus
an unauthenticated bridge REST API on `:8080` for sends. Instead this add-on is one Go binary that keeps the
upstream tool names, parameters and message schema (so existing prompts and clients keep working) and uses current
whatsmeow plus the official MCP Go SDK. Removing the internal REST API removes the `:8080` exposure entirely rather
than locking it down. whatsmeow is pinned to a recent commit (`go.mod`) and is updated by bumping it there.

## Design

- `wa.go`: whatsmeow client; pairing through the QR channel (codes kept in memory for the
  page, printed in the log only when there is no page, i.e. plain Docker); re-pairing after `LoggedOut` or an unlink from the page; stores live messages and history sync into
  `messages.db`; LID senders mapped to phone-number JIDs when known; sends and media download.
- `store.go`: SQLite message store (`/data/messages.db`), parameterised queries, LIKE wildcards escaped.
- `tools.go`: MCP tools. Two tool sets chosen per request by token: the read-only token (`auth_token`) gets the
  read tools; the send token (`send_token`) gets read and send tools, and only when `enable_send` is on (off by
  default). A send call without the send token is a tool error. Sends only to `allowed_recipients` (empty refuses
  every send). `download_media` only when `allow_media_download`. Recipient validation (phone digits or user/LID/group JID; no broadcast, status or
  newsletter). Send limiter (per minute token bucket, rolling 24 h cap).
- `media.go`: outbox reads through `os.Root` (no escape via `..` or symlinks), size caps, content sniffing,
  bounds-checked Ogg Opus parser.
- `http.go`: exposed port 8120: `/mcp` (bearer tokens with roles, SHA-256 + constant-time compare, failed attempts throttled
  per client, 1 MiB body cap, cross-origin protection) and `GET /healthz`; everything else 404.
- `page.go`: ingress port 8121: status, QR PNG, generated read and send tokens (POST, shown once, rotation), new QR, unlink. Only the ingress proxy
  address `172.30.32.2` may connect; POSTs need `X-Requested-With`; strict CSP, no inline script.
- `config.go`: `/data/options.json` or `WA_MCP_*` env; read token required: configured (≥ 32 chars) or generated
  into `/data/auth_token` (0600); send token likewise (`/data/send_token`, only when sending is on, must differ);
  generated tokens only shown on the admin-only page; never logged.

## Threat model (summary)

Assets: the WhatsApp session keys (full account takeover), message history, the bearer token, the ability to send
as the user. Entry points: `/mcp` through the tunnel (internet), the hassio network (other add-ons), the ingress
page (HA admins), message content from any WhatsApp contact (prompt injection into agents), files in `/share`.
Main risks and mitigations: token guessing (≥ 32 chars, throttling), unauthenticated routes (only `/healthz`),
arbitrary file read via send tools (outbox `os.Root`), arbitrary write via downloads (`os.Root`, off by default),
spam or ban from runaway agents (rate limits, `enable_send`), logs leaking content or token (never logged,
whatsmeow debug never enabled), other add-ons reaching the page (source IP check).

## Tests

Unit and integration tests in Go (`go test -race`): config validation, token generation, auth (missing, wrong,
wrong scheme, throttling), routes (404 except `/mcp` and `/healthz`), MCP end-to-end with the SDK client and a fake
WhatsApp, tool registration with send off / download on, path traversal and symlink escapes, rate limits, store
queries, ingress-only page. CI also builds both architectures natively and smoke-tests the container without ever
contacting WhatsApp (`WA_MCP_NO_CONNECT=1`), checking file permissions and that the token is not in the log.
Pairing with a real phone is a manual step after installation.
