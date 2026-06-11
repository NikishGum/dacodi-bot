# Scaling & Production-Maturity Plan

This document is a self-contained work plan for evolving the support bot from a
working single-instance service into a professionally operated, scalable
product. It is written for an implementer who has not seen the original review
conversation. Read `CLAUDE.md` and `README.md` first — they describe the
architecture, data flow, and the FastPanel/BitNinja production environment.

**Explicitly out of scope** (handled separately by the project owner): version
control setup, tests, CI/CD, linters, and the Telegram library migration. Do
not add test scaffolding or CI files as part of this plan.

**Related document:** `FIX-PLAN.md` (security/reliability fixes). Two items
there create primitives this plan builds on: FIX-5 (idempotency keys for ticket
creation) and FIX-11 (Telegram update dedup). Where a task below depends on
them, it says so.

## Invariants every task must preserve

- **Zoho API credit frugality.** The poller makes zero calls when no ticket is
  open and one shared list call per cycle otherwise. Nothing here may introduce
  unbounded per-cycle or per-message Zoho traffic.
- **Echo-safety:** client→operator = private Zoho comments; operator→client =
  public comments. Never flip visibility.
- **Single active ticket per client; per-chat FSM serialization; complete
  client isolation.**
- Client-facing strings are Russian. Logging is structured `log/slog`.
- Production = static binaries + systemd + FastPanel nginx + BitNinja + local
  Redis (AOF). Keep deployments compatible with `make deploy`.

---

## Track A — Operations & Observability

### A1. Prometheus metrics endpoint

**Goal.** Make the service's health and the Zoho credit budget observable.

**Implementation.**
1. Add `github.com/prometheus/client_golang` (the one allowed new dependency
   for this track) and expose `GET /metrics` on the existing mux. **Do not**
   proxy it through nginx/FastPanel — it stays loopback-only, scraped locally
   or via an SSH tunnel/node-exporter-style setup.
2. Instrument:
   - `zoho_api_requests_total{method,path_class,status}` — counter in
     `zoho.Client.do`/`doMultipart` (normalize paths: `/tickets/{id}/comments`
     → `tickets_comments` etc. — never use raw IDs as label values).
   - `zoho_api_retry_total`, `zoho_token_refresh_total`.
   - `ticket_queue_depth` — gauge from `LLEN queue:tickets` (collect in the
     worker tick or via a custom collector).
   - `tickets_created_total{path="sync|retry"}`, `ticket_create_seconds`
     histogram.
   - `telegram_send_failures_total`, `telegram_updates_total{type}`.
   - `poller_cycle_seconds` histogram, `poller_active_tickets` gauge.
   - `binding_attempts_total{result="ok|invalid|expired|rate_limited"}`.
3. Add build-info gauge (`app_info{version=...}` — see A3).

**Acceptance.** `curl 127.0.0.1:8080/metrics` shows the series above; a full
conversation flow moves the counters; cardinality is bounded (no IDs in
labels).

### A2. Dead-letter queue for failed ticket creation

**Goal.** A poison or permanently-failing queued request must not retry forever
and silently sit behind the 30s tick; an operator must find out.

**Implementation.**
1. Wrap the queued payload in an envelope: `{attempts int, first_failed_at
   time, req CreateTicketRequest}`. Maintain backward compatibility while the
   queue may hold old-format entries: try the envelope first, fall back to a
   bare `CreateTicketRequest` (attempts=0). (`queue/retry.go`,
   `redis/client.go`.)
2. After **N attempts** (suggest 240 ≈ 2h at the 30s tick, env-configurable
   `TICKET_MAX_RETRIES`), move the envelope to `queue:tickets:dead` (RPUSH) and:
   - log at `error` with chat_id and subject;
   - increment `ticket_dlq_total` (A1);
   - notify the client once: «Не удалось зарегистрировать обращение
     автоматически. Мы уже занимаемся этим вручную.» — and notify the internal
     operator channel (A5) if configured.
3. Add a minimal requeue path: a small admin action (see C2's admin surface)
   or, at minimum, a documented `redis-cli` runbook command
   (`LMOVE queue:tickets:dead queue:tickets RIGHT LEFT`) in the README.

**Acceptance.** A request that always 400s lands in the DLQ after N attempts,
the queue keeps draining other items, and the metric/log fire.

### A3. Build/version stamping & one-command rollback

**Goal.** Know what is running; roll back in one command.

**Implementation.**
1. Embed version: `-ldflags "-X main.version=$(git describe --always --dirty)"`
   in the Makefile build targets; log it at startup; include it in `/healthz`
   output (switch the body to a tiny JSON: `{"status":"ok","version":"..."}` —
   keep returning 200/503 semantics so `make health` still works) and in the
   `app_info` metric (A1).
2. Makefile `deploy`: before `mv`, copy the current binary to
   `supportbot.prev` / `tokengen.prev` on the VPS. Add a `rollback` target that
   swaps `.prev` back and restarts. (README mentions `.prev` but nothing
   implements it.)

**Acceptance.** `make deploy && make health` shows the new version;
`make rollback` restores the previous binary and the old version shows again.

### A4. Redis backup & restore runbook

**Goal.** Redis is the only state (FSM, bindings, queue); losing it must be a
recoverable event with a written procedure.

**Implementation.**
1. Nightly cron on the VPS: `BGSAVE` (RDB snapshot alongside AOF), then copy
   the dump plus the AOF directory to an off-box destination (owner provides
   target; plan for `scp`/rclone). Keep 7 daily copies.
2. Write `docs/RUNBOOK-redis.md`: how to restore a backup, what is lost in a
   restore window (active conversations, bindings since snapshot), and what
   self-heals (bindings re-resolve from Zoho — note this triggers the
   rate-limited scan path from FIX-1, so recovery is gradual by design).
3. Add a `make backup-now` convenience target (optional).

**Acceptance.** A documented, tested restore on a scratch Redis instance
(restore the dump, run the bot against it, verify an existing binding works).

### A5. Operator alert channel (internal Telegram chat)

**Goal.** Ops events (DLQ entries, poller failures, Zoho auth failures) should
reach humans without a monitoring stack.

**Implementation.**
1. Optional env `OPS_CHAT_ID` (int64). When set, a tiny `opsnotify` helper
   (shared package) sends rate-limited alerts (max ~1 per event type per
   10 min, in-process throttle) to that chat via the existing bot API client.
2. Wire into: DLQ moves (A2), repeated Zoho token refresh failures
   (`zoho/auth.go` exhausted-retries path), poller cycle errors persisting
   > 5 consecutive cycles.
3. The ops chat id must never collide with client logic: exclude it in
   `handleUpdate` (ignore all updates from `OPS_CHAT_ID`).

**Acceptance.** Forcing a DLQ entry produces one alert message in the ops chat;
spamming failures does not flood it.

### A6. Secrets management hardening

**Goal.** Reduce secret sprawl beyond the FIX-9 env split.

**Implementation.**
1. Move secrets from `EnvironmentFile` to systemd `LoadCredential=`/
   `ImportCredential=` where practical (`TELEGRAM_BOT_TOKEN`,
   `ZOHO_CLIENT_SECRET`, `ZOHO_REFRESH_TOKEN`, `BINDING_TOKEN_SECRET`,
   `TOKENGEN_API_KEY`); read them in `config.Load` via
   `$CREDENTIALS_DIRECTORY` with env-var fallback (keep local dev working with
   `.env`).
2. Write a rotation runbook (`docs/RUNBOOK-secrets.md`): order of operations
   for rotating each secret without downtime (e.g. webhook secret: set new in
   Telegram via `setWebhook` after deploying the new value; binding secret:
   rotate during a window with no outstanding unredeemed tokens).

**Acceptance.** Services start with credentials supplied via systemd
credentials; `.env` no longer needs to contain them on the VPS; local
`make run` still works from `.env`.

---

## Track B — Architecture

### B1. Question scripts as data (+ i18n foundation)

**Goal.** Remove the two hardcoded limitations called out in `CLAUDE.md`:
question scripts in code, Russian-only strings.

**Implementation.**
1. Move `questionSets`, `serviceGroups`, `categoryByGroup`
   ([bot/fsm.go:51-125](bot/fsm.go#L51-L125)) into a versioned data file
   (`config/questions.yaml` or JSON; YAML needs `gopkg.in/yaml.v3` — acceptable
   here). Schema: groups → ordered questions (`key`, `prompt`, `options[]`),
   service→group map, group→category map.
2. Load and **validate at startup** (fail fast: unknown group references, empty
   prompts, duplicate keys). Keep the compiled-in defaults as fallback when the
   file is absent, so local dev needs no extra setup.
3. The conversation FSM stores `Group` + `QuestionIndex` in Redis; a deploy
   that edits scripts mid-conversation can desync indices. Mitigate: include a
   `script_version` (hash of the file) in `Conversation`; on mismatch while
   `StateAskingQuestions`, restart that conversation's question flow with an
   apologetic prompt.
4. i18n foundation (do not over-build): extract all client-facing strings from
   `bot/`, `webhook/`, `poller/`, `queue/` into a message catalog keyed by id
   (`config/messages.<lang>.yaml`), default `ru`. A per-client language could
   later come from a Zoho custom field — design the lookup signature as
   `msg(lang, key, args...)` now, hardcode `lang="ru"` until that field exists.

**Acceptance.** Editing a prompt in the YAML and restarting changes the bot's
question without recompiling; startup fails loudly on a malformed file;
mid-conversation script change degrades gracefully.

### B2. Horizontal-scaling readiness (multi-instance)

**Goal.** Make running ≥2 bot instances *possible and safe*. Until completed,
the constraint is: **exactly one instance** — add that sentence to README now.

**Implementation.**
1. **Per-chat locks across instances.** The in-process `chatLock`
   ([bot/bot.go:73](bot/bot.go#L73)) does not serialize across instances.
   Introduce a Redis lock (`SET NX PX` with token + Lua release — or
   `github.com/go-redsync/redsync/v4`) keyed `lock:chat:{id}`, TTL ~45s
   (> updateTimeout), acquired in `handleUpdate` around the existing local
   lock. Keep the local lock too (cheap fast path).
2. **Poller leader election.** Two pollers would double-forward operator
   replies (the seen-markers race). Add a leader lease in Redis
   (`SET NX PX poller:leader <instance_id>` with renewal at interval/2); only
   the leader polls. Same for the retry worker (or make the worker safe instead:
   `LPop` is atomic, so multiple workers are *almost* safe — the unsafe part is
   the FIX-5 idempotency check racing; with FIX-5 done, multiple workers are
   acceptable. Decide and document).
3. **Telegram ingress.** Webhook mode load-balances naturally (any instance can
   take any update; the distributed chat lock preserves per-chat ordering only
   if updates for one chat are processed under the lock — ordering across
   instances is then lock-acquisition order, which can reorder near-simultaneous
   messages. Document this honestly; if strict ordering matters, route by
   `chat_id % N` at nginx or move to B3's stream design, which solves it
   properly).
4. Config: `INSTANCE_ID` env (default hostname+pid) for lock ownership and logs.

**Dependencies.** FIX-5 (idempotent ticket creation) before allowing concurrent
workers.

**Acceptance.** Two local instances against one Redis: a rapid message burst to
one chat produces no interleaved/duplicated FSM transitions; exactly one
instance polls Zoho (kill it — the other takes over within one lease TTL).

### B3. Asynchronous webhook processing (ack-then-process)

**Goal.** Stop holding Telegram's webhook request for up to 30s under Zoho
outages (today's synchronous design causes Telegram redelivery storms; see
[bot/bot.go:112-117](bot/bot.go#L112-L117)).

**Implementation.**
1. On webhook receipt: validate secret, dedup `update_id` (FIX-11), push the
   raw update to a per-chat Redis list/stream (`updates:{chat_id}`), and
   register the chat in a "dirty chats" set; return 200 immediately.
2. A dispatcher goroutine pool consumes dirty chats; one worker drains one
   chat's list FIFO under the (distributed, B2) chat lock — this preserves
   strict per-chat ordering *better* than today's design and scales across
   instances.
3. Bound everything: list length cap per chat (drop + log beyond ~100 pending),
   per-update processing timeout (existing `updateTimeout`), metric for
   backlog size (A1).
4. Keep polling mode unchanged (it is already sequential and local-dev only).
5. This is the largest architectural change in the plan — implement after B2's
   locks exist, behind a config flag (`UPDATE_PIPELINE=sync|async`, default
   `sync`) so it can be rolled back in production by env change alone.

**Acceptance.** With Zoho stalled (simulate 30s hangs), the webhook endpoint
answers in <100ms, updates process in order once Zoho recovers, and Telegram
shows no pending-update buildup.

### B4. Generalized idempotency/dedup layer

**Goal.** One consistent Redis-backed primitive instead of scattered SETNX
calls.

**Implementation.** After FIX-5/FIX-11 land, extract their patterns into a tiny
store helper: `Seen(ctx, namespace, id string, ttl) (firstTime bool, err)` in
`redis/client.go`, and migrate callers (update dedup, ticket idempotency check,
binding jti claim can stay specialized due to its release semantics). Use it
for any new event source (e.g. Zoho webhook event ids if webhook mode is ever
enabled — Zoho retries deliveries too).

**Acceptance.** Single implementation, callers migrated, behavior unchanged.

---

## Track C — Product features

### C1. `/status` command for clients

**Goal.** Let a client check their open ticket without pinging the operator.

**Implementation.**
1. Handle `/status` in `handleMessage` ([bot/handlers.go:171](bot/handlers.go#L171))
   for authorized chats: if a ticket mapping exists, reply with number and — at
   zero extra Zoho cost — the last-known state from the poller's markers; if
   `StateTicketOpen` with no ticket yet (queued), say it is being registered;
   otherwise say no active ticket and suggest `/start`.
2. Optionally (bounded cost): one `GetTicketByID` call per `/status`,
   rate-limited per chat (e.g. once per 5 min via the B4 `Seen` helper) to show
   the live Zoho status name.
3. Register the command via Telegram `setMyCommands` at startup (`/start`,
   `/status`) so clients discover it.

**Acceptance.** `/status` answers correctly in all four states (no ticket,
queued, open, after closure) and cannot be used to drain Zoho credits.

### C2. Unbind/rebind admin surface

**Goal.** `registry.Revoke` ([registry/registry.go:144](registry/registry.go#L144))
exists but nothing calls it; offboarding currently requires manual Zoho edits
plus waiting out Redis TTLs.

**Implementation.**
1. Extend **tokengen** (it is already the internal, API-key-guarded admin
   service) with `POST /unbind {bot_client_id}`:
   - tokengen has no Zoho/Redis access today and should stay that way; so
     instead add the endpoint to the **bot** on a loopback-only admin mux.
     Concretely: a second `http.Server` on `ADMIN_ADDR` (default
     `127.0.0.1:8081`, never proxied), API key header (`ADMIN_API_KEY`),
     endpoints: `POST /admin/unbind {chat_id}` → `registry.Revoke` + clear FSM/
     ticket mapping; `GET /admin/queue` → queue + DLQ depths (pairs with A2);
     `POST /admin/requeue-dead` (A2).
2. Notify the affected chat on unbind: «Доступ к поддержке для этого чата
   отключён.»
3. Document in README (operations section).

**Acceptance.** Unbinding a test chat makes the bot treat it as unknown on the
next message; rebinding via a fresh token works; admin port is unreachable from
outside the VPS.

### C3. Operator attachments → Telegram

**Goal.** The poller forwards only comment **text**; files an operator attaches
in Zoho never reach the client.

**Implementation.**
1. In the poller's reconcile path, for a changed ticket also list its
   attachments (Zoho Desk `GET /api/v1/tickets/{id}/attachments` — verify
   exact shape against docs), filtered to ones newer than a new per-ticket
   `attachment seen` marker (same pattern as comment markers in
   `redis/client.go`).
2. **Credit budget:** only call the attachments endpoint when the ticket's
   `modifiedTime` advanced (the existing gate), and cap forwarded attachments
   per cycle. Only forward attachments marked public, if the API exposes
   visibility — verify; if it does not distinguish, forward only attachments
   whose creator is an agent (operator), never the bot's own uploads
   (`isPublic:false` uploads from the bot must not echo back).
3. Download via the authenticated Zoho client, send to Telegram with
   `tgbotapi.NewDocument`/`NewPhoto` (size cap 20 MiB; skip + notify
   «Оператор приложил файл, который слишком велик для Telegram» otherwise).

**Acceptance.** An operator attaching an image to an open ticket results in
the client receiving it within one poll interval; the bot's own uploaded
client attachments are never echoed back; idle tickets cost no extra calls.

### C4. Post-closure CSAT rating

**Goal.** A one-tap satisfaction score after closure; written back to the
ticket.

**Implementation.**
1. After the closure notice (both `poller.handleClosure` and
   `webhook.handleClosure`), send a rating prompt with an inline keyboard 1–5
   (callback prefix `rate:<ticketID>:<n>`; mind Telegram's 64-byte callback
   limit — ticket IDs are long, so store `pending_rating:{chat}` → ticketID in
   Redis with e.g. 48h TTL and put only `rate:<n>` in the callback).
2. **Do not add an FSM state** — closure must still fully reset the
   conversation so `/start` works immediately (single-active-ticket invariant).
   Handle the rating callback statelessly: look up `pending_rating:{chat}`,
   write the score as a **private** comment on the closed ticket
   («Оценка клиента: 4/5») via `Desk.AddComment`, delete the pending key,
   answer the callback with «Спасибо за оценку!», and edit the keyboard away.
3. Ignore rating callbacks with no pending key (stale buttons) — answer the
   callback silently, mirroring existing stale-button handling.
4. Metric: `csat_score` histogram or counter by score (A1).

**Acceptance.** Closing a ticket prompts for a rating; tapping writes exactly
one private comment; `/start` works immediately regardless of whether the
client rated; tapping twice does not double-post.

### C5. Broader client media support

**Goal.** Clients send voice notes, videos, and screen recordings of incidents;
today only photos and documents are captured
([bot/handlers.go:492-509](bot/handlers.go#L492-L509)).

**Implementation.**
1. Extend `attachmentsFromMessage` and `messageContent` to handle: `Voice`
   (→ `voice-<unique_id>.ogg`, note «[приложено голосовое сообщение]»), `Video`
   and `VideoNote` (→ `.mp4`), `Audio` (use original filename when present),
   `Animation`. Respect the existing 20 MiB Bot API download cap — if Telegram
   reports a larger `FileSize`, record the note but skip the attachment and
   tell the client the file is too large to attach.
2. Keep the queue-persistence property: only `file_id`/`file_name` travel
   through Redis (existing `zoho.Attachment` shape — unchanged).
3. Telegram file_ids for media expire eventually; the retry path already
   tolerates download failure (logs and continues) — verify that behavior
   covers the new types.

**Acceptance.** Each new media type sent during the proof/logs question (and
while a ticket is open) appears on the Zoho ticket; oversized files produce a
polite client message instead of a silent failure.

---

## Suggested sequencing

| Order | Task | Size | Depends on |
|------|------|------|------------|
| 1 | A3 version stamping + rollback | S | — |
| 2 | A1 metrics | M | — |
| 3 | A2 dead-letter queue | M | A1 (metrics), optionally A5 |
| 4 | A5 ops alert channel | S | — |
| 5 | A4 backups + runbook | S | — |
| 6 | C1 /status | S | — |
| 7 | C2 admin surface (unbind/requeue) | M | A2 |
| 8 | C5 media support | M | — |
| 9 | B1 scripts-as-data + i18n base | M/L | — |
| 10 | C3 operator attachments | M | Zoho API verification |
| 11 | C4 CSAT | M | — |
| 12 | A6 secrets via systemd credentials | M | FIX-9 from FIX-PLAN.md |
| 13 | B4 dedup layer extraction | S | FIX-5, FIX-11 |
| 14 | B2 multi-instance readiness | L | FIX-5, B4 |
| 15 | B3 async update pipeline | L | B2 |

Tracks are independent; within Track B the order is fixed (B1 anytime,
B4 → B2 → B3). The two `L` items at the end are only worth doing when client
volume actually demands a second instance — everything before them makes the
single-instance deployment professional and observable.
