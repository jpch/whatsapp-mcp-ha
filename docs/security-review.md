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
| Authentication | Every `/mcp` method needs `Authorization: Bearer`. SHA-256 of both sides compared with `subtle.ConstantTimeCompare`. Configured tokens ≥ 32 printable chars, ≥ 10 distinct characters and not a short repeated pattern; generated tokens 256 bits, stored 0600, never logged (CI asserts the token is absent from the container log). |
| Routes | Exposed port: `/mcp` and `GET /healthz` (`{"status":"ok"}` only). Everything else 404; `//mcp`, `/./mcp` redirect, `/mcp/`, `/MCP`, encoded variants 404 (verified locally). No bridge REST API exists. |
| Ingress page | Separate port, not published; only `172.30.32.2` (Supervisor ingress proxy) may connect, so other add-ons get 403; `panel_admin: true`; POSTs need `X-Requested-With` and are refused for `Sec-Fetch-Site: cross-site`; CSP `default-src 'none'`, no inline script, DOM writes via `textContent`. All ingress pages share the Home Assistant origin, so another add-on's page could call these endpoints with the custom header; therefore a generated token is revealed only once (POST, persisted marker) and can afterwards only be rotated. |
| CSRF / browsers | `/mcp` wrapped in `http.CrossOriginProtection` (cross-site browser POST → 403, verified). The MCP SDK's DNS-rebinding protection only applies to requests that arrive on a loopback address, which this server never receives in the add-on (it listens on the container network), so it is not relied on; the bearer token is what stops a rebinding page. |
| Injection | All SQL parameterised; `LIKE` wildcards escaped; recipients validated (digits or user/LID/group JID; broadcast, status, newsletter, bot refused). |
| File read (send) | `send_file`/`send_audio_message` read through `os.Root` on `/share/whatsapp-mcp/outbox`: `..`, absolute paths elsewhere, symlinks out of the folder, a symlinked outbox and an outbox behind a symlinked parent are refused (tests). The root is opened first and its device/inode compared with `Lstat` of the symlink-free path. Size cap `max_media_mb`. Recipients can be restricted with `allowed_recipients`. |
| File write (download) | Off by default. Writes through `os.Root` into `/share/whatsapp-mcp/downloads` with sanitised names; planted symlinks are not followed (test). The whatsmeow media HTTP client refuses responses announcing more than `max_media_mb` + 64 KiB and aborts bodies that grow past it (test), so a sender-controlled size cannot exhaust memory. AppArmor confines all server writes to `/data` and `/share/whatsapp-mcp` (enforced and tested in CI). |
| Rate limits / DoS | Sends: token bucket per minute + rolling 24 h cap (default 50), global. Request body 1 MiB (413 verified), header timeouts, list limits and `page` (≤ 1000) capped, stateless MCP (no session table to exhaust). Failed auth throttled per client (10, then 1 per 6 s); the table evicts the least recently seen entry when full; failed-auth log lines are globally rate limited. |
| Logging | Message contents, captions and tokens are never logged; whatsmeow debug logging is never enabled (debug add-on level maps whatsmeow to INFO). |
| Data at rest | `/data` 0700, databases and token 0600 (umask 077; asserted in CI). Optional `retention_days` deletes old messages and vacuums incrementally. |
| Supply chain | Go modules verified (`go mod verify` in the image build), `govulncheck` in CI (found and fixed GO-2026-6859 by moving to Go 1.27.4), Go and Home Assistant base images pinned by `@sha256` digest, actions pinned by commit SHA, workflow `contents: read` by default, `packages: write` only for the build job, publishing only on push to main/tags and only when `PUBLISH_IMAGES` is set, no `pull_request_target`. |

## Findings

| # | Severity | Finding | Status |
|---|---|---|---|
| 1 | Medium | **Prompt injection to send.** Any contact can put instructions in a message; an agent with send tools may follow them and message arbitrary numbers or forward outbox files. | Mitigated (send stays on by default at the owner's request): optional `allowed_recipients` (recommended in README and option help), daily cap lowered to 50, other people's text returned in an `untrusted` object, server instructions, README warning. Residual risk when the allowlist is empty. |
| 2 | Medium | **Single shared token.** All clients share one token; no per-client revocation or audit. | Accepted: rotate by changing `auth_token`. Follow-up idea: multiple named tokens. |
| 3 | Low | Failed-auth throttling is keyed by remote address + `CF-Connecting-IP`, which a caller can vary. It does not stop a determined guesser; token entropy does. | Documented; full reset replaced by evict-oldest, logs rate limited (round 2). |
| 4 | Low | Base images referenced by tag, not digest. | Fixed: pinned by `@sha256` (round 2). |
| 5 | Low | `actions/checkout` v4, `docker/*` v3/v6 pins run on the deprecated Node 20 runtime (CI warning). | Open: bump pins in a later PR. |
| 6 | Low | `skip2/go-qrcode` (QR PNG for the page) has had no release since 2020. Input is only the pairing code from whatsmeow. | Accepted. |
| 7 | Info | Container runs as root (Home Assistant default). The token and session are in Home Assistant backups. | Accepted. |
| 8 | Info | Connecting an unofficial client may violate WhatsApp's terms and risk a ban. | README warns. |

Re-verification after fixes: `go test -race ./...` green, `govulncheck` clean, CI build and container smoke test
green on aarch64 and amd64.

## Independent review (round 2, at 64ea993)

An independent reviewer found no Critical or High issues. Findings and resolution:

| Finding | Severity | Resolution |
|---|---|---|
| Media download OOM: whatsmeow reads media with an unbounded `io.ReadAll`, and the advertised file length comes from the sender | Medium | `SetMediaHTTPClient` with a transport that refuses `Content-Length` > `max_media_mb` + 64 KiB and wraps bodies in a reader that errors on overflow; uploads (POST) unaffected. `TestMediaHTTPClientLimits`. |
| Prompt-injection sends | Medium | `allowed_recipients` (empty = anyone, recommended), daily cap 50, `untrusted` object for other people's text, README warning. `TestAllowedRecipientsEnforced`, `TestReadTools`. |
| Writes not confined | Low | `apparmor.txt`: the server runs in a child profile that can only write to `/data`, `/share/whatsapp-mcp`, `/tmp`. CI loads the profile, checks the server's label and that a write to `/opt` fails. |
| No message retention | Low | `retention_days` (default 0 = keep): periodic `DELETE` + `PRAGMA incremental_vacuum` (`auto_vacuum=INCREMENTAL`). `TestPruneRetention`. |
| Failed-auth log flood; full reset of the throttle table | Low | Global log rate limit with a suppressed counter; evict least recently seen entry. `TestAuthEvictsOldestAndLimitsLogs`. |
| Check-then-open race on media folders | Low | Open the `os.Root` first, then compare its `Stat` with `Lstat` of the symlink-free path (`os.SameFile`, device + inode). `TestOutboxBehindSymlinkedParentRefused`. |
| Shared ingress origin can read the generated token | Low | Token shown once, rotation endpoint, documented in README and on the page; every reveal and rotation is logged (without the token) so an unexpected one is visible. Residual: rotation also returns the new token, so a malicious ingress page opened by an admin could still obtain one; setting `auth_token` removes this. `TestPageIngressOnly`. |
| No watchdog; halt on every exit | Low | `watchdog: http://[HOST]:[PORT:8120]/healthz`; exit code 2 (configuration error) stops the add-on, other exits restart after 5 s. CI checks exit code 2. |
| Inaccurate DNS-rebinding claim; images not pinned by digest | Low | Text corrected above; Go and Home Assistant base images pinned by `@sha256`. |
| Unbounded `page`; weak configured tokens | Low | `page` capped at 1000; tokens need ≥ 10 distinct characters and no short repeated pattern. `TestPageCapped`, `TestWeakTokensRejected`. |

Re-check after the fixes (7e91b00 and later): no Critical or High findings; AppArmor confinement observed enforcing
in CI (`mkdir /opt/evil: permission denied`).

Not changed: the ghcr image publication (no `image:` in `config.yaml`, publishing gated by `PUBLISH_IMAGES`), as
requested. Still open: Low 5 (Node 20 action pins) and Low 6 (go-qrcode), see the table above.
