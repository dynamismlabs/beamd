# Per-request events spec — edge → control plane

Status: proposed. Owners: edge (`~/dynamism/beamd`), control plane (`~/dynamism/beamd-web`).

## 1. Goal & decision

Replace the periodic per-slug **delta** usage pipeline with a **per-request event
log**: the edge emits one self-contained event when a request completes; the
sink is the source of truth. This gives per-URL usage *and* rich analytics
(method, path, status, latency, geo) from one pipeline.

Decisions already settled in discussion:

- **Fire-and-forget, not aggregate-and-delta.** No watermarks, no cumulative
  counters on the edge. Each event carries its own bytes — totals are
  `SUM(rows)` at read time.
- **The file is the buffer.** The edge always appends events to a local
  append-only file. OSS stops there. Hosted runs a shipper that *tails the file
  from a persisted byte offset* and bulk-POSTs to the control plane. The cursor
  is the only long-lived edge state — a few bytes, not a counter.
- **Write before you ship.** Append to disk first, then tail-and-ship. A crash
  mid-batch never loses an event that was already written; the shipper resumes
  from the offset. Lossless across edge restarts and control-plane outages.
- **One source.** Billing reads the same rows. The only loss window is events
  in memory not yet written (sub-ms with write-through). Any loss is an
  *under*-count (customer's favor), acceptable at current scale. A durable
  billing backstop can be added later additively (§7) — not now. (Note: drop is
  *load-correlated* — the channel fills when event rate is highest = most billable —
  so size the channel generously and **alert on the drop counter**, §9.)
- **Privacy / data minimization.** `client_ip` is **truncated/hashed at the edge**
  before it ships — raw visitor IPs never leave the edge. `path` / `user_agent` /
  `referer` are the **analytics tier**: captured by default but individually
  disableable (`capture:` config, §4.6), so a privacy-strict or self-host operator
  ships billing-only. Billing-essential fields (bytes, status, outcome, host, slug,
  timestamps) always ship.
- **Long-lived connections (WS/SSE) are time-bucketed, not per-message.** A chatty
  stream would explode the event count and emit-on-close hides hours-long sessions.
  Emit a **heartbeat every `heartbeat_seconds` (default 60)** carrying that window's
  *delta* bytes (its own per-emit `request_id`, a shared `connection_id`, window-start
  `started_at`, `outcome:in_progress`), plus a final event on close — `SUM(rows)` stays
  correct, volume is ~1/min/conn.
- **Keep all data for now.** No retention deletion. `request_event` is a **plain table**
  with a single-column **edge-minted uuidv7 `id` PK**; **partitioning is deferred** — when
  volume needs it we `PARTITION BY RANGE (id)`, which keeps the PK unchanged (§5.3).
  `usage_daily` stays the fast read path over the growing raw table.

Net effect on code: this **deletes** the 60s reporter machinery
(`internal/usage/reporter.go`'s delta/watermark/state-file logic) rather than
adding to it.

## 2. Architecture

```
  request completes
        │
        ▼
  edge.handler  ──emit RequestEvent──▶ append-only file   (OSS: done. durable here.)
   (per request)                          beamd-requests.log
                                              │
                              hosted only:    │  tail from persisted offset
                                              ▼
                                        shipper goroutine ──batch POST──▶  POST /api/internal/requests
                                          (size/time flush)                 (shared secret)
                                              │                                   │ bulk insert
                                          advance + fsync cursor                  ▼
                                          beamd-requests.cursor            request_event (raw; partition later, by id)
                                                                                  │  derive on read / nightly
                                                                          ┌───────┴────────┐
                                                                          ▼                ▼
                                                                   tunnel registry    usage_daily rollup
                                                                   (firstSeen/last)   (org × host × day)
```

Billing/dashboard = `SUM`/`COUNT` over `usage_daily` (long retention) for old
periods, `request_event` for the recent window. Raw rows can be aged out
aggressively because the daily rollup preserves the durable totals.

## 3. The event (wire contract)

One JSON object per completed request. Field names are the wire contract — they
must match byte-for-byte across the Zod schema (beamd-web), the generated Go
type (`internal/beamdapi`), and the hand-written sink struct (`internal/reqlog`).

| field | type | required | notes |
|---|---|---|---|
| `request_id`   | string  | yes | Edge-minted **uuidv7**, **per-emit** — each request *and* each heartbeat window gets its own. The idempotency key (dedupe via `onConflictDoNothing`); maps to the DB `id` PK. |
| `connection_id`| string  | no  | Edge-minted; **shared across a long connection's heartbeat events** so they correlate. Absent for one-shot requests. |
| `slug`         | string  | no  | From `route.session.slug` when there's a route; **empty on `no_route`** (no session). **Billing attribution is slug-first** (alias-aware `claimed_slug → org`; present on every routed request, no host parse). `host` is the URL **identity** + the org key only for the rare `no_route` enrichment (§5.2). |
| `host`         | string  | yes | Port-stripped `Host` (`edge.go:643`). The URL **identity** (links to the `tunnel` registry, per-URL grouping); org key **only** for `no_route` enrichment (§5.2) — routed traffic attributes slug-first. |
| `method`       | string  | yes | `r.Method`. |
| `path`         | string  | no  | `r.URL.Path` — **query string stripped** (secrets leak). Analytics tier (`capture.path`, §4.6). Optional edge redaction of token-shaped segments (JWT / long hex/base64url / UUID / email → `«redacted»`) — paths carry secrets too. |
| `status`       | int     | yes | `rr.status`. 0 → see `outcome`. |
| `outcome`      | enum    | yes | `ok \| in_progress \| no_route \| backend_error \| timeout \| size_limit \| client_closed`. `in_progress` = a long-conn heartbeat (§4.4). Edge-only knowledge the status can't express. |
| `bytes_in`     | int64   | yes | Request bytes (counted on `r.Body`, see §4.4). |
| `bytes_out`    | int64   | yes | Response bytes (`rr.bytes`; for WS, from the hijacked conn). |
| `ttfb_ms`      | int     | no  | Time to first response byte. Omitted when there is no response (`no_route`). Not derivable from start/end. |
| `is_websocket` | bool    | yes | Upgraded/hijacked connection — exclude from latency percentiles. |
| `client_ip`    | string  | no  | Visitor IP from the **TCP peer** (`r.RemoteAddr` host — edge terminates TLS, so un-spoofable; never trust `X-Forwarded-For`). **Truncated/hashed at the edge** (/24, /48) before ship — raw IP never leaves the edge; country resolved web-side. Analytics tier (`capture.client_ip`). |
| `user_agent`   | string  | no  | `r.UserAgent()`. Bot/device classification done web-side. |
| `referer`      | string  | no  | `r.Referer()`. |
| `started_at`   | string  | yes | RFC 3339 (matches existing `period_*` convention). |
| `ended_at`     | string  | yes | RFC 3339. For WS, the end of the upgraded session. |

Batch wrapper: `{ "events": RequestEvent[] }`.

Principle: **send raw signals, enrich on the other side.** IP→country,
UA→bot/device, host→name — all web-app jobs, keep the edge dumb.

## 4. `~/dynamism/beamd` (Go edge) changes

New package `internal/reqlog`. The file sink is **always on** (OSS gets local
request logs for free); the shipper is hosted-only.

### 4.1 Sink interface + event struct (`internal/reqlog/reqlog.go`)

```go
package reqlog

// RequestEvent is the wire contract. JSON field names are guarded against
// internal/beamdapi.RequestEvent by a conformance test (see §6). Types may
// differ from the generated type (int64 vs int) — the test checks names only.
type RequestEvent struct {
	RequestID    string `json:"request_id"`               // edge-minted uuidv7, per-emit → DB id (PK)
	ConnectionID string `json:"connection_id,omitempty"`  // shared across a connection's heartbeats
	Slug         string `json:"slug,omitempty"`           // optional: empty on no_route
	Host        string  `json:"host"`
	Method      string  `json:"method"`
	Path        string  `json:"path,omitempty"`   // optional: omitted when capture.path off
	Status      int     `json:"status"`
	Outcome     string  `json:"outcome"`
	BytesIn     int64   `json:"bytes_in"`
	BytesOut    int64   `json:"bytes_out"`
	TTFBMs      *int64  `json:"ttfb_ms,omitempty"`
	IsWebSocket bool    `json:"is_websocket"`
	ClientIP    string  `json:"client_ip,omitempty"`
	UserAgent   string  `json:"user_agent,omitempty"`
	Referer     string  `json:"referer,omitempty"`
	StartedAt   string  `json:"started_at"`
	EndedAt     string  `json:"ended_at"`
}

// Sink receives completed request events. Record must be non-blocking and
// drop-on-backpressure (fire-and-forget); it bumps a dropped counter so loss
// is observable, never silent.
type Sink interface {
	Record(ev RequestEvent)
}
```

### 4.2 File sink (`internal/reqlog/file.go`)

- Single writer goroutine draining a buffered channel → appends one JSON line
  per event to `<DataDir>/requests.log`. One writer = no interleaving.
- `Record` does a non-blocking channel send; on a full channel, **drop +
  `requestsDroppedTotal.Add(1)`**. (Fire-and-forget; never block the proxy.)
- **fsync policy:** batch fsync on a short timer (e.g. 250ms) or every N lines —
  durability vs. throughput knob. Default: fsync every 250ms.
- **Rotation:** rotate at a size cap (e.g. 128MiB) to `requests.log.1`; the
  shipper must follow rotation (track inode/path). Use `lumberjack`-style
  rotation or hand-roll. OSS without a shipper: cap total retained files.

This mirrors the existing `trafficStore` sink pattern (`internal/edge/traffic.go`)
— pluggable recorder, fan-out on completion, persisted under `DataDir`.

### 4.3 Hosted shipper (`internal/reqlog/shipper.go`)

- Tails `requests.log` with a maintained tail library (**`nxadm/tail`** — live following +
  rotation *while running*) and persists an **(inode, byte-offset)** cursor in
  `<DataDir>/requests.cursor` (atomic tmp+rename, like `usage-state.json`). **Ship only
  complete, newline-terminated lines** — hold a trailing partial until its newline arrives,
  so a flush mid-write never ships a truncated JSON object. *Caveat:* the library covers
  live following; **restart-resume is our glue** — on startup, find the file matching the
  cursor's inode (it may have rotated while we were down), seek to the offset, then follow
  forward. Verify the library's reopen-by-offset semantics rather than assuming it's free.
- Accumulates lines into batches; flush on **N events (e.g. 500) or T seconds
  (e.g. 1s)**, whichever first. (This is batching of *raw rows*, not delta
  aggregation — no watermark math.)
- Bulk-POST `{events:[…]}` to `cfg.RequestReporter.WebhookURL` with
  `Authorization: Bearer <secret>`. On 2xx → advance + fsync the cursor.
  On non-2xx/network error → **do not advance**; retry the same window next
  tick (this is what makes it lossless across control-plane outages — same
  self-healing property as the old watermark, but the file is the buffer).
- Rotation is handled by the tail library (inode change → continue on the new file);
  the cursor stores the inode so a restart resumes on the right file. Retry duplicates
  are absorbed by `request_id` idempotency (§5.2); truncation can't happen because only
  complete lines ship.

### 4.4 Edge instrumentation (`internal/edge/edge.go` `handler`, ~613)

Today `handler` already has `method, path(r.URL.Path), host(port-stripped),
status, bytes_out(rr.bytes), start, slug` and just `slog`s them (`edge.go:665`).
Capture instead. Additions:

- **TTFB:** add `firstByteAt time.Time` to `responseRecorder` (`metrics.go:84`),
  set on the first `WriteHeader`/`Write`. `ttfb = firstByteAt.Sub(start)`.
- **bytes_in:** wrap `r.Body` in a counting reader (alongside the existing
  `http.MaxBytesReader` at `edge.go:635`), read the count at completion. Use
  `r.ContentLength` as a fallback when no body is read.
- **outcome:** set from the branch taken — `no_route` when `route == nil`
  (`edge.go:653`), `size_limit` on 413, `client_closed`/`backend_error`/
  `timeout` from the proxy error, else `ok`.
- **client_ip:** host part of `r.RemoteAddr` (the TCP peer — edge terminates TLS, so
  this is the real visitor; **never** read `X-Forwarded-For`, which the untrusted
  tunneled app could spoof). **Truncate/hash here** (/24, /48) before it enters the
  event — raw IP never ships. Country is resolved web-side from the truncated prefix.
  **Deployment caveat:** this assumes the edge is the internet-facing TLS terminator. If
  it ever sits behind an L4 LB, `RemoteAddr` is the LB — use PROXY protocol; behind an L7
  proxy, trust a forwarded header from *that hop only*.
- **WebSocket / SSE / long-lived (time-bucketed, not per-message):** wrap the byte
  source in an **`atomic.Int64`** counter — the hijacked `net.Conn` for WS (post-`Hijack()`
  bytes bypass `responseRecorder`), the streaming `responseRecorder` for SSE. It's read by
  the heartbeat goroutine while the proxy goroutine writes, so it **must be atomic** for
  *both* WS and SSE (today's `rr.bytes` is a plain int64 — a race; §4.5). Emit on a
  **`heartbeat_seconds` timer (default 60)**: each heartbeat is a self-contained
  `RequestEvent` with that window's **delta** bytes, **its own fresh `request_id`**
  (per-emit uuidv7) and a **shared `connection_id`** tying the windows together,
  `outcome:in_progress`, and `started_at` = the **window start**.
  > **Invariant:** `started_at` is the **window start** (not the connection start), so each
  > window's bytes bucket into the correct day/period — the only thing that matters at a
  > day boundary for a long connection. Per-emit `request_id`s mean retries dedupe
  > per-window on the `id` PK with no collision (the old shared-id composite-key hazard is
  > gone). **Test: N windows → N rows.**

  The **final event on close** covers `[last-window-end, close]` with its own `request_id`
  + `started_at` (`outcome:ok`) — so it neither double-counts nor gaps the last window.
  `SUM(rows)` stays correct; volume ~1/min/conn. `is_websocket=true` for WS; `ttfb_ms`
  omitted.
- Emit via `e.reqSink.Record(ev)` at the end of `handler` (and at hijacked-conn close for
  WS). Mint a uuidv7 `request_id` **per emit** (one per request; a fresh one per heartbeat
  window), plus one `connection_id` per long-lived connection.

### 4.5 Metrics (`internal/edge/metrics.go`)

Add `requestsDroppedTotal atomic.Int64`; expose at `/metrics` as
`beam_requests_dropped_total` (sink backpressure). Loss is observable. **Also make any
byte counter the heartbeat path reads `atomic.Int64`** — `responseRecorder.bytes` is a
plain int64 today, and SSE/WS heartbeats read it from a separate goroutine (§4.4).

### 4.6 Config (`internal/config/server.go`)

```yaml
request_log:
  enabled: true                 # file sink (default on)
  path: ""                      # default <data_dir>/requests.log
  max_size_mb: 128
  fsync_ms: 250
  heartbeat_seconds: 60         # long-conn (WS/SSE) accounting interval (§4.4)
  ip_mode: truncate             # truncate (/24,/48) | hash | off — applied AT THE EDGE
  capture:                      # analytics tier — disable any to ship billing-only
    path: true                  #   (path also gets token-shape redaction, §3)
    client_ip: true             #   (always truncated/hashed per ip_mode)
    user_agent: true
    referer: true
request_reporter:               # hosted-only shipper; empty = off (OSS)
  webhook_url: "https://app.example.com/api/internal/requests"
  secret_env: BEAMD_REQUESTS_SECRET
  batch_size: 500
  flush_ms: 1000
  cursor_file: ""               # default <data_dir>/requests.cursor
```

Mirror `UsageReporterConfig` (`server.go:69`) and its env-override pattern.

### 4.7 Wiring (`cmd/beamd/main.go`)

- Construct the file sink and inject into `edge.New(...)` (new `reqSink` field on
  `Edge`, set before `Serve`, read lock-free on the hot path — same contract as
  `AddTrafficSink`, `edge.go:119`).
- Add `startRequestShipper(cfg, dataDir)` mirroring `startUsageReporter`
  (`main.go:192`): no-op when `RequestReporter.WebhookURL == ""`; otherwise
  `go shipper.Run(ctx)`; return a cancel for graceful drain.

### 4.8 Deletions

- `internal/usage/reporter.go` — the delta/watermark reporter. Remove (or reduce
  to nothing). `startUsageReporter`, `UsageReporterConfig`, `usage-state.json`,
  and `Edge.UsageSnapshot()`/`bytesOutBySlug()` go away **once** the web app
  reads from `request_event` (§5). Keep `trafficStore` only if `/metrics`
  per-(slug,name) bytes are still wanted locally; otherwise remove too.

## 5. `~/dynamism/beamd-web` (control plane) changes

### 5.1 Wire schemas (`src/server/public-api/contracts.ts`)

Add Zod schemas + register the endpoint. **This file is the OpenAPI source of
truth** (§6). Keep OpenAPI 3.0.3-friendly: use `.optional()` (→ omitempty
pointer in Go), never 3.1 nullable.

```ts
export const RequestEventSchema = z
  .object({
    request_id: z.string(),               // edge-minted uuidv7, per-emit → DB id (PK)
    connection_id: z.string().optional(), // groups a long connection's heartbeats
    slug: z.string().optional(),          // empty on no_route (no session); host is the key
    host: z.string(),
    method: z.string(),
    path: z.string().optional(),          // analytics tier (capture.path)
    status: z.number().int(),
    outcome: z.enum([
      "ok", "in_progress", "no_route", "backend_error", "timeout", "size_limit",
      "client_closed",
    ]),
    bytes_in: z.number().int().nonnegative(),
    bytes_out: z.number().int().nonnegative(),
    ttfb_ms: z.number().int().nonnegative().optional(),
    is_websocket: z.boolean(),
    client_ip: z.string().optional(),
    user_agent: z.string().optional(),
    referer: z.string().optional(),
    started_at: z.string(),
    ended_at: z.string(),
  })
  .openapi("RequestEvent")

export const RequestBatchSchema = z
  .object({ events: z.array(RequestEventSchema) })
  .openapi("RequestBatch")

export const RequestAcceptedSchema = z
  .object({ ok: z.boolean(), accepted: z.number().int() })
  .openapi("RequestAccepted")
```

Register in `registerContractPaths` (next to `ingestUsage`), tag `Internal`,
`security: [{ edgeSecret: [] }]`:

```ts
registry.registerPath({
  method: "post",
  path: "/api/internal/requests",
  operationId: "ingestRequests",
  summary: "Ingest a batch of per-request events",
  tags: ["Internal"],
  security: [{ edgeSecret: [] }],
  request: { body: jsonBody(RequestBatchSchema) },
  responses: {
    200: { description: "Accepted", ...jsonBody(RequestAcceptedSchema) },
    401: { description: "Bad shared secret" },
  },
})
```

### 5.2 Route handler (`src/app/api/internal/requests/route.ts`)

Mirror `…/usage/route.ts`:

1. `hasValidSharedSecret(req, env.BEAMD_REQUESTS_SECRET)` (`server/beamd/shared-secret.ts`); 401 on fail.
2. `RequestBatchSchema.safeParse`; 400 on fail. Empty → `{ok:true, accepted:0}`.
3. Attribute via **`slug` → organizationId** (alias-aware `claimed_slug → org`, url-model
   R5). The session slug is present on every *routed* request (custom domains included:
   the owning session carries the scope's slug), so this resolves the billable 99% with one
   indexed join and **no host parse on the hot path**. `host` is stored only for **identity**
   (the `tunnel` registry, per-URL grouping, which-alias-was-hit). For `no_route` events
   (no slug) do a **best-effort `host` → org** enrichment so "someone hit your retired URL"
   reaches the right org — but those carry no billable usage, so it's off the billing path.
   (This `no_route` enrichment is the *only* place the §7 host→slug parse runs; attribution
   for routed traffic stays parse-free.) Unknown → null (still recorded). One batched lookup
   over the distinct slugs (+ hosts for `no_route`) → map.
4. **Bulk insert** `request_event` rows (`db.insert(...).values(rows)`), idempotent on the
   **`id` PK** (`onConflictDoNothing`), mapping wire `request_id` → column `id`. Each emit —
   every request *and* every heartbeat window — carries its own edge-minted uuidv7, so
   retries/replays dedupe on the single-column PK; heartbeats correlate via `connection_id`.
5. Upsert the `tunnel` registry from distinct hosts (`firstSeenAt`/`lastSeenAt`),
   exactly like the existing usage route does for per-host events.
6. `200 {ok:true, accepted: rows.length}` fast (beamd retries the window on
   non-2xx, so failing closed is safe).

### 5.3 Schema (`src/server/db/schema.ts`)

New `request_event` (camelCase props, snake_case DB via the existing `casing`
config; reuse the `createdAt()` helper):

```ts
export const requestEvent = pgTable(
  "request_event",
  {
    // Edge-minted uuidv7, per emit (each request AND each heartbeat window). It's the
    // idempotency key (dedupe-on-retry via onConflictDoNothing), so the PRODUCER owns it:
    // NOT the pk() helper / DEFAULT uuidv7() — the edge supplies it on the wire (field
    // `request_id`). This is the one table whose id is edge-generated, not DB-generated.
    id: uuid().primaryKey(),                  // = wire `request_id`; no DB default
    connectionId: uuid(),                     // groups a long connection's heartbeats; null for one-shots
    organizationId: uuid().references(() => organization.id, { onDelete: "set null" }),
    slug: text(),                  // NULL on no_route (no session); §3
    host: text().notNull(),
    method: text().notNull(),
    path: text(),                  // NULL when capture.path off ("" is never a real path)
    status: integer().notNull(),
    outcome: text().notNull(),
    bytesIn: bigint({ mode: "number" }).notNull(),
    bytesOut: bigint({ mode: "number" }).notNull(),
    ttfbMs: integer(),
    isWebsocket: boolean().notNull().default(false),
    clientIp: text(),
    userAgent: text(),
    referer: text(),
    startedAt: timestamp({ withTimezone: true, mode: "date" }).notNull(),
    endedAt: timestamp({ withTimezone: true, mode: "date" }).notNull(),
    createdAt: createdAt(),
  },
  (t) => [
    index("request_event_org_started_idx").on(t.organizationId, t.startedAt),
    index("request_event_host_started_idx").on(t.host, t.startedAt),
    index("request_event_conn_idx").on(t.connectionId),
    // Rollup scans a day's rows by started_at; rows arrive ~in order → BRIN is tiny + ideal.
    index("request_event_started_brin").using("brin", t.startedAt),
  ],
)
```

Derived rollup `usage_daily` (replaces the delta `usage_event` for
billing/dashboards; long retention so raw rows can be aged out):

```ts
export const usageDaily = pgTable(
  "usage_daily",
  {
    id: pk(),                                  // uuidv7
    organizationId: uuid().references(() => organization.id, { onDelete: "set null" }),
    slug: text().notNull(),
    host: text().notNull(),        // one row per (org, host, day); org total = SUM over hosts
    day: timestamp({ withTimezone: true, mode: "date" }).notNull(),
    requests: bigint({ mode: "number" }).notNull(),
    bytesIn: bigint({ mode: "number" }).notNull(),
    bytesOut: bigint({ mode: "number" }).notNull(),
  },
  (t) => [
    // host is NOT NULL so the upsert target is reliable (no NULL-distinctness trap);
    // there's no separate org-level row — org/day total is SUM over its host rows.
    uniqueIndex("usage_daily_org_host_day_idx").on(t.organizationId, t.host, t.day),
    index("usage_daily_org_day_idx").on(t.organizationId, t.day),
  ],
)
```

A nightly job populates `usage_daily` from **complete days** of `request_event`
(≤ yesterday UTC; today is read live, §5.4), rolling up only **attributed** rows
(non-null `organizationId`/`slug` — `no_route` events carry no billable usage).
**Retention: keep everything for now** — no deletion. `usage_daily` stays the fast read
path so dashboards/billing don't scan the growing raw table.

> **Partitioning (decided): deferred — and when we do it, by `id`.** Ship a **plain,
> unpartitioned** table now (single-column uuidv7 `id` PK, `started_at` a normal column) —
> at zero traffic there's nothing to partition. **If/when volume needs it, `PARTITION BY
> RANGE (id)`:** because `id` is uuidv7 (time-ordered), id-ranges *are* time-ranges, and —
> the reason we picked `id` over `started_at` — partitioning by `id` keeps the
> **single-column PK unchanged** (Postgres requires the partition key inside the PK; `id`
> already *is* the PK → no composite, no PK migration). **The one cost:** to get partition
> *pruning*, time-range queries (rollup, retention, dashboard) must filter by **id-range
> bounds** via a `uuidv7_at(ts)` helper (`WHERE id >= uuidv7_at(t1) AND id < uuidv7_at(t2)`)
> — filtering a plain `started_at` won't prune across partitions. drizzle-kit can't express
> `PARTITION BY`, so that's a hand-written migration + an "ensure next month's partition"
> cron *at that time*. **Tripwire:** partition **before real traffic** — converting a large
> table is a rewrite; add a storage/row-count dashboard metric so we see it coming. We keep
> all data for now (*"don't delete yet; partitioning keeps archiving a cheap `DETACH`/`DROP`
> later"* — Postgres isn't the long-term home for high-volume raw; columnar/cold when volume
> demands).

### 5.4 Read API (`src/server/trpc/routers/usage.ts`)

Repoint the existing procedures off the delta `usage_event` and onto the new
tables, and enrich:

- `summary` → `usage_daily` for **complete days (≤ yesterday UTC)** + **today's partial
  read live from `request_event`**. Pin this seam so the two never overlap (rollup covers
  ≤ yesterday; raw covers today) — otherwise you double-count or gap the boundary.
  `SUM(requests)`, `SUM(bytes_in)`, `SUM(bytes_out)`, total = in+out, per org over N days.
- `series` → `usage_daily` grouped by `day`.
- `byTunnel` → group by `host`: requests + bytes in/out/total per URL. (This is
  the `api-<org>.beamd.sh → 1000 req, 200MB in, 500MB out, 700MB total` view.)
- `recent` → newest `request_event` rows (method, path, status, outcome, ttfb)
  for an activity/debug view.
- `tunnels` → unchanged (`tunnel` registry).
- Org total and grand total are just sums — never stored.

Drop the delta `usage_event` table + `/api/internal/usage` route + its contract
once the edge stops sending the old shape.

## 6. APIs & codegen — getting the shapes right

**Source of truth = beamd-web Zod schemas.** Flow, in order:

1. **Edit Zod** in `beamd-web/src/server/public-api/contracts.ts` (§5.1) and
   register the path. Document is assembled by `registry.ts` →
   `buildOpenApiDocument` as **OpenAPI 3.0.3** (deliberate: oapi-codegen doesn't
   support 3.1 `type:[...,"null"]`; 3.0 round-trips through both generators).
2. **Export the spec** (beamd-web dev server running):
   ```bash
   pnpm api:export        # curl /api/v1/openapi.json → openapi/beamd-api.json
   pnpm api:gen           # openapi-ts → src/lib/api-client/types.gen.ts (TS client)
   ```
3. **Sync to Go** (`~/dynamism/beamd`):
   ```bash
   cp ../beamd-web/openapi/beamd-api.json internal/beamdapi/openapi.json
   make api-gen           # oapi-codegen (models:true) → internal/beamdapi/types.gen.go
   ```
4. **Hand-written struct + conformance.** `internal/reqlog.RequestEvent` (§4.1)
   mirrors `beamdapi.RequestEvent`. Add `internal/reqlog/conformance_test.go`
   (copy `internal/usage/conformance_test.go`):
   ```go
   {"RequestEvent", RequestEvent{}, beamdapi.RequestEvent{}},
   {"RequestBatch", requestBatch{}, beamdapi.RequestBatch{}},
   ```
   `beamdapi.JSONFields` (`internal/beamdapi/fields.go`) compares the **sorted
   JSON field-name set**, `,omitempty` stripped. So:
   - **Field names must match exactly** across Zod / generated / hand-written.
   - **Types need not match** — generated `bytes_in` is `int`, our struct uses
     `int64`; `ttfb_ms` generated `*int`, ours `*int64`. Fine: names only.
   - **`optional()` ⇒ omitempty (guard blind spot).** A `.optional()` Zod field
     generates a Go pointer with `,omitempty`; the hand-written field must also be
     `,omitempty` (here: `slug`, `path`). **`JSONFields` strips `,omitempty` before
     comparing, so it can NOT catch an optionality mismatch** — a non-omitempty `slug`
     emits `"slug":""` (looks like an empty value) instead of omitting it, and the test
     passes anyway. Optionality + types are **manual care**, not guarded.
5. **CI guards:** `make api-check` (`check-drift.sh`) fails if `types.gen.go` is
   stale vs `openapi.json`; the per-package conformance tests fail if a
   hand-written struct drifts from the generated type. Both must be green.

Field/type mapping reference:

| Zod | OpenAPI 3.0.3 | generated Go (`beamdapi`) | hand-written (`reqlog`) |
|---|---|---|---|
| `z.string()` | `string` | `string` | `string` |
| `z.string().optional()` | `string` (not required) | `*string` omitempty | `string` omitempty |
| `z.number().int()` | `integer` | `int` | `int` |
| `z.number().int().nonnegative()` | `integer, minimum:0` | `int` | `int64` (bytes) |
| `z.number().int().optional()` | `integer` (not required) | `*int` omitempty | `*int64` omitempty |
| `z.boolean()` | `boolean` | `bool` | `bool` |
| `z.enum([...])` | `string, enum:[...]` | `string` alias + consts | `string` |

Gotchas: keep timestamps as **RFC 3339 strings** (matches `period_*`; trivial
`new Date()` web-side). Don't introduce 3.1-only constructs. `z.enum` emits a Go
string alias under `models:true` — harmless for conformance (names only), and
the web side gets real validation.

## 7. Migration / rollout

1. Land beamd-web first: schema (`request_event`, `usage_daily`), contract,
   `/api/internal/requests` route. Deploy. Endpoint live, nothing sends yet.
2. Sync the spec to Go, regenerate, add `internal/reqlog` (file sink + shipper),
   wire the file sink on (OSS benefit immediately), shipper behind config.
3. Turn on `request_reporter` on the hosted edge(s). Confirm `request_event`
   filling, `byTunnel` populating, `tunnel` registry growing.
4. Repoint the tRPC usage router to the new tables; verify dashboard parity.
5. Delete the old path: edge `internal/usage` reporter + `UsageSnapshot`,
   `/api/internal/usage` route, delta `usage_event` table, `UsageRequest`
   contract. Run drift + conformance; commit.

Later (only if billing disputes demand it): re-add a tiny durable per-slug
counter as an independent billing backstop. Additive — the request log stays the
analytics source.

## 8. Decisions

**Decided this revision:** `request_event` PK = **single-column edge-minted uuidv7 `id`**
(= wire `request_id`, no DB default — producer owns it for dedup); **partitioning deferred**,
and **if/when we partition, by `id`** so the PK never changes (§5.3); idempotency via
`onConflictDoNothing` on the **`id` PK**; long-conn heartbeats are **per-emit `id` + shared
`connection_id`** (no composite key) (§4.4); `client_ip` **truncated/hashed at the edge**
(§4.4); **no retention deletion** — keep all data (§5.3); `path` + analytics fields are
**capture-configurable** (§4.6); `slug` optional, **billing attribution slug-first** (host
for identity; host→org only for `no_route`, §3/§5.2).

**Still open:**
- **fsync cadence** — 250ms default; tighten for stronger durability.
- **Path redaction depth** — token-shape redactor only (v1) vs route-templating
  (`/users/:id`) later for cardinality.
- **Billing-backstop trigger** — the drop-rate alert threshold (§9) at which the
  durable per-slug counter (§7) becomes mandatory.

## 9. UI/UX & security

**UX — the request log is a product surface, not just billing.**
- **Live request inspector (ngrok-style).** Stream new `request_event`s to a live
  dashboard tail — watch requests hit your tunnel in real time (method, path, status,
  ttfb, outcome). ngrok's `localhost:4040` inspector is a beloved feature and we
  already have the data: high-value, low-extra-cost dev UX.
- **Per-URL + geo + outcome.** `byTunnel` (requests + bytes in/out per host), a country
  map from the truncated IP prefix, and an `outcome` breakdown ("12 backend_error, 3
  timeout") turn beamd into a debugging tool, not just a pipe.
- **`no_route` is a feature.** Events with `outcome:no_route` are hits on a dead/typo'd/
  **tombstoned** host — surface them ("requests are hitting `api-old-acme`, which you
  renamed") and tie into the url-model 410 / "this tunnel moved" page.

**Security — the new surface is a *writable, billing-affecting* endpoint.**
- **`/api/internal/requests` can move money.** Unlike the read-only verify-token /
  scope-hostnames lookups, a leaked shared secret here lets an attacker **forge usage
  events** — inflate a victim's bill, hide their own usage, or poison analytics. So:
  **per-edge secrets** (a compromised droplet can't speak for the fleet), **attribute
  each batch to the emitting edge** and reject events for slugs that edge doesn't serve,
  and **sanity-bound** batches (size / rate caps) so a forged flood is contained. This
  is the most important new control in this spec.
- **IP from the TCP peer, never a header.** `r.RemoteAddr` (edge terminates TLS) is the
  real visitor; never trust `X-Forwarded-For` — the tunneled app is untrusted and could
  spoof it. Truncate/hash at the edge (§4.4).
- **`request_event` is a sensitive store** (paths, IP prefixes, UAs of customers'
  visitors). Scope reads **strictly to the owning org**; the edge-side IP minimization +
  capture config keep the breach blast radius small.

## 10. Checklists

**`~/dynamism/beamd`**
- [ ] `internal/reqlog`: `RequestEvent` (`,omitempty` on optional `slug`/`path`), `Sink`,
      file sink, shipper, conformance test
- [ ] **Test:** heartbeats — each window a **fresh per-emit `request_id`** + **shared
      `connection_id`**, `started_at` = window start → N windows → N rows (correct day
      bucketing); final event covers `[last-window-end, close]`
- [ ] `responseRecorder`: `firstByteAt` (TTFB); counting `r.Body` reader (bytes_in)
- [ ] `handler`: outcome enum (incl. `in_progress`), **edge-truncated** client_ip,
      **path redaction** (token shapes), **WS/SSE time-bucketed heartbeats** +
      emit-on-close, per-emit `request_id` (uuidv7) + `connection_id`; honor `capture` config
- [ ] `Edge.reqSink` field + lock-free hot-path read; metrics `beam_requests_dropped_total`
- [ ] config `request_log` + `request_reporter`; `startRequestShipper` in `main.go`
- [ ] sync spec → `internal/beamdapi/openapi.json` → `make api-gen`
- [ ] delete `internal/usage` reporter + `UsageSnapshot`/`bytesOutBySlug` after cutover

**`~/dynamism/beamd-web`**
- [ ] `contracts.ts`: `RequestEvent`/`RequestBatch`/`RequestAccepted` + `ingestRequests` path
- [ ] `app/api/internal/requests/route.ts`: **per-edge** shared secret + **edge-attribution**
      (reject slugs the edge doesn't serve) + batch sanity-bounds; **slug-first
      (`claimed_slug → org`)** attribution, host for identity (+ host→org for `no_route`);
      bulk insert idempotent on the **`id` PK** (map wire `request_id` → `id`); tunnel upsert
- [ ] `schema.ts`: `request_event` (single-col **edge-minted uuidv7 `id` PK**, no DB default;
      `connection_id`; **plain table — partition deferred, by `id`**) + `usage_daily` (host
      NOT NULL); drop delta
      `usage_event` after cutover
- [ ] rollup job → `usage_daily` (no retention deletion — keep all). *(Partition-ensure cron
      lands later, with the deferred `PARTITION BY RANGE (id)` migration — not now.)*
- [ ] `trpc/routers/usage.ts`: repoint summary/series/byTunnel/recent; add requests + in/out/total
- [ ] `pnpm api:export && pnpm api:gen`; remove `/api/internal/usage` + `UsageRequest` after cutover
```
