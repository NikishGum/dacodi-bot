# Fix Plan — Security Vulnerabilities & Reliability Bugs

This document is a self-contained work plan for fixing the issues found during a
full security/code review (2026-06-11). It is written for an implementer who has
not seen the review conversation. Read `CLAUDE.md` and `README.md` first — they
describe the architecture, data flow, and deployment environment.

## Context the implementer must preserve

- **Zoho API credits are a scarce resource.** The bot deliberately runs in
  `ZOHO_SYNC_MODE=poll` with a credit-frugal poller (`poller/poller.go`). No fix
  may add per-message or per-cycle Zoho calls without an explicit bound.
- **Echo-safety invariant:** client→operator messages are *private* Zoho
  comments; operator→client replies are *public* comments. Never change comment
  visibility (`zoho/desk.go`, `AddComment`).
- **Single active ticket per client** and **per-chat FSM serialization** via
  `bot.chatLock` must keep working.
- All client-facing strings are **Russian**. Logging is `log/slog`, structured,
  lowercase messages. No new third-party dependencies unless a task says so.
- Production runs as a single binary behind FastPanel-managed nginx with
  BitNinja (no manual nginx edits — `nginx/supportbot.conf` is a reference
  file). `.env` is a systemd `EnvironmentFile` (values are NOT trimmed by
  systemd — secrets are already defensively trimmed in code; keep that).
- Verification baseline for every task: `make check` (vet + build) passes, and
  the local polling-mode flow (`README.md` → "Быстрый старт") still works.

## Severity legend

- **P0** — exploitable now or production-breaking; do first.
- **P1** — real risk, currently mitigated by configuration/deployment accident.
- **P2** — hardening, hygiene, correctness debt.

---

## Phase 1 — P0

### FIX-1: Unauthenticated Zoho credit-drain via cold-cache account scans

**Problem.** Any stranger who messages the bot triggers
`registry.Authorize` ([bot/handlers.go:48](bot/handlers.go#L48)) → on cache miss
`resolveByChatID` ([registry/registry.go:63](registry/registry.go#L63)) →
`Desk.AccountByChatID` ([zoho/desk.go:380](zoho/desk.go#L380)), which performs a
**full account list plus one GET per account** (N+1 Zoho calls). The only guard
is a 5-minute negative cache per chat (`authUnknownTTL`,
[redis/client.go:22](redis/client.go#L22)). An attacker with throwaway Telegram
accounts can force sustained scans, exhausting the Zoho API quota and starving
ticket creation and the poller with 429s.

**Fix.**
1. Add a **process-wide rate limiter for cold-miss Zoho account scans** in
   `registry.Registry`. Implementation: a Redis-based limiter (e.g.
   `SET NX scan_limiter:<bucket> EX <window>`) or an in-process
   `golang.org/x/time/rate`-style token bucket (stdlib-only equivalent is fine:
   a mutex + last-scan timestamp). Policy: at most **1 full scan per 60s**
   process-wide, configurable via env `ZOHO_SCAN_MIN_INTERVAL` (default `60s`).
2. When the limiter denies a scan for an *unknown* chat, do **not** call Zoho.
   Cache the chat as unknown (`SetAuthUnknown`) with a **shorter** TTL (e.g.
   1 minute) so a legitimate client hit by the limiter recovers quickly.
3. Raise the default negative-cache TTL `authUnknownTTL` from 5 minutes to
   **30–60 minutes** (make it a named constant; document the trade-off: a chat
   bound manually in Zoho — outside the token flow — is not recognized until
   the TTL expires; the normal token-binding flow writes the positive cache
   directly via `registry.cache`, which overwrites the `auth:{chat}` key, so it
   is unaffected).
4. Apply the same limiter to `resolveByBotID`'s fallback scan path
   ([registry/registry.go:174](registry/registry.go#L174)). Token redemption on
   a limiter denial should return a transient error so `handleBinding` replies
   «Временная ошибка. Пожалуйста, попробуйте позже.» and releases the jti claim
   (this error path already exists).
5. *(Optional, separate commit, verify against Zoho docs first)*: replace the
   N+1 scan with the Zoho Desk **account search API**
   (`GET /api/v1/accounts/search?customField1=<cf_api_name>:<value>` or
   equivalent). If the endpoint works on your Zoho plan/DC, the scan path
   becomes a single call and the limiter becomes a safety net. Do not assume
   the endpoint shape — test it manually with the real org first.

**Acceptance.**
- Sending messages from N unbound chats produces at most one Zoho scan per
  configured interval (observe via `LOG_LEVEL=debug` logs).
- A bound client (positive cache) is entirely unaffected.
- Token binding still works when the limiter is cold and when it is saturated
  (transient-error reply, retry succeeds later).

### FIX-2: `/telegram/webhook` is unauthenticated in polling mode

**Problem.** The handler is mounted unconditionally
([main.go:85](main.go#L85)), but `TELEGRAM_WEBHOOK_SECRET` is only required
when `TELEGRAM_MODE=webhook`
([config/config.go:151-154](config/config.go#L151-L154)). With an empty secret
the constant-time compare at [bot/bot.go:93](bot/bot.go#L93) passes for an
empty header — anyone who can reach the port can inject a forged update with an
arbitrary `chat_id`. Since `chat_id` is the authentication principal, this is
full client impersonation. Today `SERVER_ADDR` defaults to loopback, so it
requires local access or a changed bind address — fix it before that accident
happens.

**Fix.**
1. In `main.go`, register `/telegram/webhook` **only** when
   `cfg.TelegramMode == "webhook"`.
2. Defense in depth: in `WebhookHandler` ([bot/bot.go:87](bot/bot.go#L87)),
   reject all requests with 403 if `b.cfg.TelegramWebhookSecret == ""` (before
   the compare), and log an error once at startup if the handler is somehow
   mounted without a secret.

**Acceptance.**
- `TELEGRAM_MODE=polling`: `POST /telegram/webhook` → 404.
- `TELEGRAM_MODE=webhook` + wrong/absent secret header → 403; correct secret →
  200 and the update is processed.

---

## Phase 2 — Reliability bugs (production-breaking under real conditions)

### FIX-3: Retry-queue worker can hang forever on attachment downloads

**Problem.** `downloadTelegramFile` in the worker
([queue/retry.go:147-165](queue/retry.go#L147-L165)) uses `http.DefaultClient`
(no timeout) with the worker's root context (no deadline — it comes from
`Run(ctx)` in [main.go:76-77](main.go#L76-L77)). One stalled download freezes
the entire retry queue until restart. The bot-side copy
([bot/bot.go:215-233](bot/bot.go#L215-L233)) is bounded by the 30s update
timeout but also uses `http.DefaultClient`.

**Fix.**
1. Both `bot` and `queue` packages contain identical
   `downloadTelegramFile`/`uploadAttachments` code. Extract a single shared
   helper (suggested location: a small `internal/tgfile` package, or export
   from `bot` — implementer's choice; avoid an import cycle: `queue` must not
   import `bot` if `bot` imports `queue` interfaces — it currently doesn't, but
   a neutral package is safest).
2. The helper uses a dedicated `http.Client{Timeout: 2 * time.Minute}`
   (20 MiB max file on a slow link needs headroom).
3. In `queue.Worker.process` ([queue/retry.go:102](queue/retry.go#L102)), wrap
   each queued item in `context.WithTimeout(ctx, 5*time.Minute)` so no single
   item can stall the drain loop indefinitely (ticket creation + several
   attachments must fit).

**Acceptance.**
- `make check` passes; no duplicated download code remains.
- Simulate a hang (e.g. point the download at a non-routable address via a
  test) — the worker logs an error and proceeds to the next tick instead of
  freezing.

### FIX-4: Byte-level truncation corrupts long Cyrillic operator replies

**Problem.** `trimContent` ([poller/poller.go:300-306](poller/poller.go#L300-L306))
slices at a **byte** offset (`s[:3900]`), which can split a multi-byte UTF-8
rune. Telegram rejects invalid UTF-8 with a 400, so a long Russian operator
reply silently never reaches the client. The same bug class was already fixed
in `Conversation.subject()` ([bot/fsm.go:222-227](bot/fsm.go#L222-L227)) using
rune slicing.

**Fix.**
1. Replace truncation with **splitting**: a helper
   `splitMessage(s string, maxRunes int) []string` that splits on rune
   boundaries, preferring to break at the last `\n` (then space) before the
   limit. Use a conservative `maxRunes` of **3500** (Telegram's limit is 4096
   UTF-16 code units; 3500 runes is safely under it for any input).
2. In `forwardNewComments` ([poller/poller.go:236](poller/poller.go#L236)) send
   the parts sequentially, in order. If any part fails to send, log and stop
   sending the remaining parts of that comment (do not advance past it
   silently — but keep the existing marker semantics: the marker already
   advances per cycle, so just log; do not redesign marker logic in this task).
3. Apply the same helper anywhere operator content is sent to Telegram
   (`webhook/zoho_handler.go` sends `p.Content` unbounded at
   [zoho_handler.go:112](webhook/zoho_handler.go#L112) — same fix there).

**Acceptance.** A 10,000-character Cyrillic comment is delivered as multiple
valid messages in correct order; no 400 errors from Telegram.

### FIX-5: Duplicate ticket creation — non-idempotent retries

**Problem.** `Client.do` retries `POST /api/v1/tickets` on 429/5xx/transport
errors ([zoho/client.go:132-142](zoho/client.go#L132-L142)). If Zoho actually
created the ticket but the response was lost, the retry — or the queue worker
re-processing the same request — creates a duplicate ticket. The queue path
(enqueue → retry every 30s) widens the window.

**Fix.**
1. Add `IdempotencyKey string` (json `idempotency_key`) to
   `zoho.CreateTicketRequest` ([zoho/desk.go:32](zoho/desk.go#L32)). Generate it
   in `finalizeTicket` ([bot/handlers.go:329](bot/handlers.go#L329)) with
   `crypto/rand` (16 bytes, hex) — no new dependency. Because the request is
   JSON-persisted in the Redis queue, the key survives restarts.
2. **Prerequisite (manual, document in README):** create a ticket custom field
   in Zoho Desk for the key (e.g. `cf_bot_request_id`), configurable via env
   `ZOHO_CF_REQUEST_ID`. Write the key into the ticket's `cf` map in
   `CreateTicket` alongside the chat id ([zoho/desk.go:122-125](zoho/desk.go#L122-L125)).
3. Change ticket creation to a **check-then-create** retry strategy:
   - First attempt: plain POST. On success, done.
   - On ambiguous failure (timeout/5xx/transport error), **before** the next
     attempt, search Zoho for a ticket carrying that idempotency key
     (Zoho Desk ticket search API, e.g.
     `GET /api/v1/tickets/search?customField1=cf_bot_request_id:<key>` —
     **verify the exact endpoint/params against Zoho Desk API docs for your DC
     before implementing**; if search proves unavailable, fall back to listing
     recent department tickets and matching the cf, bounded to one page).
   - If found: return that ticket as the created one. If not found: retry POST.
   - Apply the same check at the top of `queue.Worker.process`
     ([queue/retry.go:102](queue/retry.go#L102)) so a request that succeeded
     just before a crash/restart is not re-created.
4. 4xx responses (other than 429) must remain non-retried (current behavior).

**Acceptance.**
- Simulated 500-after-create (mock or manual Zoho test) yields exactly one
  ticket.
- Queue items processed twice (kill the bot between create and `SetTicket`)
  yield exactly one ticket.
- When `ZOHO_CF_REQUEST_ID` is unset, behavior degrades gracefully to today's
  (log a startup warning that idempotency is disabled).

---

## Phase 3 — P1 security hardening

### FIX-6: Zoho webhook token accepted via query string

**Problem.** `validToken` ([webhook/zoho_handler.go:155-164](webhook/zoho_handler.go#L155-L164))
accepts `?token=`, and the README instructs configuring it that way. Query
strings persist in nginx/FastPanel/BitNinja access logs and intermediate
proxies. The token authorizes sending **arbitrary Telegram messages to any
bound client** and resetting their ticket state. Currently dormant
(`ZOHO_SYNC_MODE=poll`), but must be fixed before webhook mode is ever enabled.

**Fix.**
1. Remove the query-parameter fallback; accept the token **only** from the
   `X-Webhook-Token` header.
2. Update `README.md` (section «Регистрация вебхуков», item 8) to configure the
   Zoho webhook with a custom header instead of `?token=`.
3. Add a startup log line in webhook mode reminding that the Zoho webhook rule
   must send the header.

**Acceptance.** `POST /zoho/webhook?token=<correct>` → 403; header-based → 200.

### FIX-7: Binding rate limiter fails open

**Problem.** If the Redis counter errors, binding proceeds without a limit
([bot/handlers.go:115-118](bot/handlers.go#L115-L118)).

**Fix.** Fail closed: on `IncrBindAttempts` error, reply «Временная ошибка.
Пожалуйста, попробуйте позже.» and return. This is consistent — `ClaimToken`
already requires Redis, so binding cannot succeed without Redis anyway.

**Acceptance.** With Redis stopped, a binding attempt gets the transient-error
reply and no Zoho calls are made.

### FIX-8: Token replay window if `BINDING_TOKEN_TTL` is raised above 24h

**Problem.** One-time use is enforced by remembering the jti for a fixed
`bindingClaimTTL = 24h` ([bot/handlers.go:24](bot/handlers.go#L24)). Nothing
validates `BINDING_TOKEN_TTL ≤ 24h`; with a 72h token TTL, a redeemed token
becomes replayable after a day.

**Fix.** Derive the claim TTL from configuration instead of a constant:
`claimTTL = cfg.BindingTokenTTL + 1h` with a floor of 24h. Pass it through
`bot.New` (the bot already has `cfg`). Remove the stale comment.

**Acceptance.** Unit-style check or manual reasoning documented in the commit:
for any configured TTL, claim TTL strictly exceeds token validity.

### FIX-9: tokengen is over-privileged

**Problem.** [systemd/tokengen.service:10](systemd/tokengen.service#L10) loads
the same `/opt/bot/.env` as the bot, so the token-minting service also holds
Zoho OAuth credentials and the Telegram bot token in its environment.

**Fix.**
1. Change `tokengen.service` to `EnvironmentFile=/opt/bot/tokengen.env`.
2. Add a `tokengen.env.example` (or a section in `.env.example`) listing only:
   `BINDING_TOKEN_SECRET`, `BINDING_TOKEN_TTL`, `BOT_USERNAME`,
   `TOKENGEN_ADDR`, `TOKENGEN_API_KEY`.
3. Update README deploy checklist (step 5) accordingly, including the note that
   `BINDING_TOKEN_SECRET` must stay byte-identical in both files (trailing
   whitespace pitfall is already handled in code — keep the trim).
4. While in the file: add `ReadTimeout`/`WriteTimeout` (e.g. 10s/10s) to the
   tokengen `http.Server` ([cmd/tokengen/main.go:64-68](cmd/tokengen/main.go#L64-L68)).

**Acceptance.** tokengen starts with only its own variables set; bot variables
absent from its environment (`systemctl show tokengen -p Environment` on VPS,
or local equivalent).

---

## Phase 4 — P2 hygiene and correctness debt

### FIX-10: Scrub infrastructure/PII from the repository

**Problem.** [Makefile:6](Makefile#L6) hardcodes the real production address
(`root@91.149.179.207`); [config/clients.json](config/clients.json) contains
real-looking Telegram chat IDs and names.

**Fix.**
1. Makefile: `VPS ?= deploy@your-vps-host` placeholder; document
   `make deploy VPS=user@host` in README. (Moving away from root SSH is an ops
   task outside this plan, but note it in README's checklist.)
2. **Before editing `clients.json`, confirm with the owner whether those chat
   IDs are live test chats** — `make deploy-config` would push changes to
   production. If they are real: move real values to an untracked
   `config/clients.local.json` (and support it via `CLIENTS_FILE`), keep only
   obviously fake placeholders (e.g. `111111111`) in the repo copy.

**Acceptance.** No real host addresses, chat IDs, or personal names remain in
tracked files; deploy still works via the override.

### FIX-11: Telegram `update_id` deduplication

**Problem.** Telegram redelivers an update if the 200 response is slow — and
under a Zoho outage the synchronous handler can take up to 30s
([bot/bot.go:112-114](bot/bot.go#L112-L114)). The FSM tolerates duplicate
callbacks (state checks) but duplicate free-text answers get recorded twice.

**Fix.** In `handleUpdate` (covers webhook and polling), skip the update if
`SETNX tg_update:<update_id> EX 600` returns false. Add the store method to
`redis/client.go`. Place the check before any processing; treat Redis errors
as "not a duplicate" (fail open here — losing dedup is better than dropping
updates).

**Acceptance.** Replaying the same webhook body twice processes it once.

### FIX-12: Unbounded per-chat mutex map

**Problem.** `chatLock` ([bot/bot.go:73-82](bot/bot.go#L73-L82)) allocates a
mutex per chat ever seen — including unauthorized strangers — and never evicts.

**Fix.** Replace the map with a **fixed shard array**: `[1024]sync.Mutex`
indexed by `uint64(chatID) % 1024`. Bounded memory; the rare cross-chat
collision only serializes two unrelated chats momentarily, which is harmless at
this scale. Do **not** implement evicting-map schemes (eviction races can hand
two goroutines different mutexes for the same chat).

**Acceptance.** `make check`; concurrent updates for the same chat remain
serialized (existing behavior preserved by construction).

### FIX-13: HTML injection into the ticket description — verify, then escape

**Problem.** Client free-text answers are concatenated raw into the ticket
description ([bot/fsm.go:231-258](bot/fsm.go#L231-L258)) and sent without a
content type ([zoho/desk.go:111-131](zoho/desk.go#L111-L131)). If Zoho Desk
renders descriptions as HTML, a client can inject markup (and, depending on
Zoho's sanitizer, scripts) into the operator's view. Client comments are
already safe (`contentType: plainText`).

**Fix.**
1. **First verify**: create a test ticket whose description contains
   `<b>test</b><script>alert(1)</script>` and inspect how Zoho Desk renders it.
2. If rendered as HTML: escape each answer value (and company name) with
   `html.EscapeString` when building `description()`, and convert newlines to
   `<br>` only if needed for readability — match whatever the verification
   shows.
3. If rendered as plain text: no code change; add a code comment on
   `description()` documenting the verification result and date, so the next
   person doesn't re-flag it.

**Acceptance.** Documented verification outcome; if escaping was added, a
description containing `<`/`>`/`&` displays correctly and inertly in Zoho.

### FIX-14: `cf_bot_priority` is fetched but never used

**Problem.** The account custom field is loaded into `Profile.Priority`
([registry/registry.go:272-280](registry/registry.go#L272-L280)) and documented
in `CLAUDE.md` as the default priority, but `finalizeTicket` only uses
`conv.detectPriority()` ([bot/handlers.go:330](bot/handlers.go#L330)).

**Fix.** Wire it in as a **floor**: if `Profile.Priority` equals `HIGH` or
`key` (case-insensitive, trimmed), the ticket priority is `PriorityHigh`
regardless of detection; otherwise use `detectPriority()`. Update `CLAUDE.md`'s
description of the field to match the implemented semantics. (If the owner
prefers removal instead, delete the field end-to-end — but pick one; do not
leave it dangling.)

**Acceptance.** A bootstrap client with `"priority": "key"` produces a High
ticket even for a mundane description; others unchanged.

### FIX-15: Documentation drift — unknown chats are not "silently ignored"

**Problem.** `CLAUDE.md` («Client Resolution», item 4) and `README.md` say
unknown chats are silently ignored, but `handleUnboundPrompt`
([bot/handlers.go:82-110](bot/handlers.go#L82-L110)) actively replies asking
for an access key.

**Fix.** Update both documents to describe the actual unbound flow (greeting →
request access key → rate-limited redemption), including the rate-limit
behavior from FIX-7.

### FIX-16: Redis hardening (ops + config)

**Problem.** [systemd/redis.service:9](systemd/redis.service#L9) runs Redis on
loopback **without a password**; the AOF on disk contains conversation
contents, auth bindings, and cached OAuth tokens.

**Fix.**
1. Add `--requirepass` support to the unit (read from a file or env-drop-in;
   document the chosen mechanism) and set `REDIS_PASSWORD` in `/opt/bot/.env`.
   The Go code already supports a password ([config/config.go:92](config/config.go#L92)).
2. Document AOF file permissions (`/var/lib/redis`, mode 700, owner `redis`)
   in the README production checklist.

**Acceptance.** `redis-cli ping` without auth fails; bot and tokengen still
start; health check passes.

---

## Suggested execution order and sizing

| Order | Task | Size | Depends on |
|------|------|------|------------|
| 1 | FIX-2 webhook gating | S | — |
| 2 | FIX-1 credit-drain limiter | M | — |
| 3 | FIX-3 worker hang | S/M | — |
| 4 | FIX-4 UTF-8 splitting | S | — |
| 5 | FIX-7 fail-closed rate limit | S | — |
| 6 | FIX-8 claim TTL | S | — |
| 7 | FIX-6 webhook token header-only | S | — |
| 8 | FIX-11 update dedup | S | — |
| 9 | FIX-12 lock shards | S | — |
| 10 | FIX-5 idempotent ticket creation | L | Zoho cf + search API verification |
| 11 | FIX-9 tokengen env split | S | ops coordination |
| 12 | FIX-13 HTML verification/escape | S | manual Zoho test |
| 13 | FIX-14 priority floor | S | owner decision (floor vs remove) |
| 14 | FIX-10 repo scrub | S | owner confirmation on clients.json |
| 15 | FIX-15 docs drift | S | FIX-7 |
| 16 | FIX-16 Redis hardening | S | ops window |

Tasks 1–9 are independent and safe to land in any order. FIX-5 is the only
large item; it needs a Zoho custom field created manually and the search API
verified against the live org before coding.
