# Security review: 0.1.0

Scope: everything in this repository at the 0.1.0 pull request (Go server, add-on config, Dockerfile, CI).
Reviewer stance: adversarial, local proofs only (unit tests and a local binary with `WA_MCP_NO_CONNECT=1`);
no WhatsApp account was paired and no third-party system was probed.

**Verdict: no Critical or High findings. Release not blocked.** Mediums are accepted and documented below.

## Threat model

| Asset | Impact if lost |
|---|---|
| whatsmeow session keys (`/data/whatsmeow.db`) | full impersonation of the WhatsApp account |
| message history (`/data/messages.db`) | privacy of all chats |
| bearer token | read all chats, send as the user |
| ability to send | spam, harassment, account ban |

Entry points: `/mcp` from the internet through a tunnel; the hassio Docker network (any other add-on); the
ingress page (Home Assistant administrators); incoming WhatsApp messages from anyone (content reaches agents);
files under `/share` (writable by other add-ons with `share:rw`); CI (pull requests, published images).

## Checks and results

| Area | Result |
|---|---|
| Authentication | Every `/mcp` method needs `Authorization: Bearer`. SHA-256 of both sides compared with `subtle.ConstantTimeCompare`. Configured tokens ≥ 32 printable chars; generated tokens 256 bits, stored 0600, never logged (CI asserts the token is absent from the container log). |
| Routes | Exposed port: `/mcp` and `GET /healthz` (`{"status":"ok"}` only). Everything else 404; `//mcp`, `/./mcp` redirect, `/mcp/`, `/MCP`, encoded variants 404 (verified locally). No bridge REST API exists. |
| Ingress page | Separate port, not published; only `172.30.32.2` (Supervisor ingress proxy) may connect, so other add-ons get 403; `panel_admin: true`; POSTs need `X-Requested-With` and are refused for `Sec-Fetch-Site: cross-site`; token reveal is POST-only and only for generated tokens; CSP `default-src 'none'`, no inline script, DOM writes via `textContent`. |
| CSRF / browsers | `/mcp` wrapped in `http.CrossOriginProtection` (cross-site browser POST → 403, verified); DNS-rebinding protection of the MCP SDK stays on. |
| Injection | All SQL parameterised; `LIKE` wildcards escaped; recipients validated (digits or user/LID/group JID; broadcast, status, newsletter, bot refused). |
| File read (send) | `send_file`/`send_audio_message` read through `os.Root` on `/share/whatsapp-mcp/outbox`: `..`, absolute paths elsewhere, symlinks out of the folder and a symlinked outbox folder itself are refused (tests). Size cap `max_media_mb`. |
| File write (download) | Off by default. Writes through `os.Root` into `/share/whatsapp-mcp/downloads` with sanitised names; planted symlinks are not followed (test); size checked before and after download. |
| Rate limits / DoS | Sends: token bucket per minute + rolling 24 h cap, global. Request body 1 MiB (413 verified), header timeouts, list limits capped, stateless MCP (no session table to exhaust). Failed auth throttled per client (10, then 1 per 6 s). |
| Logging | Message contents, captions and tokens are never logged; whatsmeow debug logging is never enabled (debug add-on level maps whatsmeow to INFO). |
| Data at rest | `/data` 0700, databases and token 0600 (umask 077; asserted in CI). |
| Supply chain | Go modules verified (`go mod verify` in the image build), `govulncheck` in CI (found and fixed GO-2026-6859 by moving to Go 1.27.4), actions pinned by commit SHA, workflow `contents: read` by default, `packages: write` only for the build job, publishing only on push to main/tags and only when `PUBLISH_IMAGES` is set, no `pull_request_target`. |

## Findings

| # | Severity | Finding | Status |
|---|---|---|---|
| 1 | Medium | **Prompt injection to send.** Any contact can put instructions in a message; an agent with send tools may follow them and message arbitrary numbers or forward outbox files. | Accepted for 0.1.0: server instructions mark content as untrusted, sends are rate limited, `enable_send: false` gives a read-only server, README warns. Follow-up idea: optional recipient allowlist. |
| 2 | Medium | **Single shared token.** All clients share one token; no per-client revocation or audit. | Accepted: rotate by changing `auth_token`. Follow-up idea: multiple named tokens. |
| 3 | Low | Failed-auth throttling is keyed by remote address + `CF-Connecting-IP`, which a caller can vary, and the table resets at 10 000 keys. It does not stop a determined guesser; token entropy does. | Documented in code. |
| 4 | Low | Base images referenced by tag (`…-base:3.24`, `golang:1.27-alpine3.24`), not digest. Same as the other add-ons of this owner. | Open. |
| 5 | Low | `actions/checkout` v4, `docker/*` v3/v6 pins run on the deprecated Node 20 runtime (CI warning). | Open: bump pins in a later PR. |
| 6 | Low | `skip2/go-qrcode` (QR PNG for the page) has had no release since 2020. Input is only the pairing code from whatsmeow. | Accepted. |
| 7 | Info | Container runs as root (Home Assistant default). The token and session are in Home Assistant backups. | Accepted. |
| 8 | Info | Connecting an unofficial client may violate WhatsApp's terms and risk a ban. | README warns. |

Re-verification after fixes: `go test -race ./...` green, `govulncheck` clean, CI build and container smoke test
green on aarch64 and amd64.
