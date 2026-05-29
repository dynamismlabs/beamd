# Hosted mode — building the web app side

Beamd has hooks for "hosted mode": a web app issues and revokes
tokens, owns the customer dashboard, runs the device-code login flow,
and receives usage events for billing. Beamd itself stays stateless
— it just validates tokens against your web app on each connection.

This doc specifies **what your web app has to expose** for those hooks
to work, so you can build it in any stack you like (the reference
hosted product is Next.js + better-auth + Drizzle + Postgres, but the
contract is framework-agnostic).

Read [`prd.md`](../prd.md) first if you haven't — this doc assumes
you understand the slug model, the wildcard-cert-per-slug rule, and
the OSS happy path from [`setup.md`](setup.md).

---

## 1. Architecture

```
                                  ┌──────────────────────────────┐
[ developer's CLI ] ── :443 ──▶  │         beamd             │
   `beam expose 3001`         │  (one or more droplets)      │
                                  │                              │
                                  │  - terminates TLS            │
                                  │  - routes by Host header     │
                                  │  - proxies through yamux     │
                                  └──────────────────────────────┘
                                          │      ▲    │
                       verify-token POST  │      │    │ usage POST
                       device-code POSTs  │      │    │ (every 60s)
                                          ▼      │    ▼
                                  ┌──────────────────────────────┐
[ developer's browser ] ────────▶│         your web app         │
   sign-up / dashboard           │  (Next.js + better-auth +    │
   device-code approval          │   Drizzle + Postgres)        │
                                  │                              │
                                  │  - users, tokens, slugs      │
                                  │  - device-code state machine │
                                  │  - usage rollups + billing   │
                                  │  - DNS + cert provisioning   │
                                  │    on slug claim             │
                                  └──────────────────────────────┘
                                                 │
                                       Cloudflare│ API (write A records
                                                 ▼ per slug at signup)
                                  ┌──────────────────────────────┐
                                  │           DNS                │
                                  └──────────────────────────────┘
```

Authoritative source of state is **your web app's Postgres**. Beamd
caches token lookups for ~60s and otherwise holds no user data.

---

## 2. Endpoints your web app must expose

Four HTTP endpoints, all called by beamd. None are user-facing.

| Endpoint | Caller | Purpose |
|---|---|---|
| `POST /api/internal/verify-token` | beamd, per session | Resolve a bearer token to a slug |
| `POST /api/device/code` | beam CLI, once per login | Issue a device + user code |
| `POST /api/device/token` | beam CLI, polling | Return the token once the user approves |
| `POST /api/internal/usage` | beamd, every 60s | Receive per-slug byte/tunnel deltas for billing |

Plus one user-facing page:

| Page | Caller | Purpose |
|---|---|---|
| `GET /device` | developer's browser | Enter the user code + sign in + approve |

Beamd is configured to call these via `auth_discovery:` and
`usage_reporter:` in `beamd.yaml`. See
[`example/beamd.yaml`](../example/beamd.yaml) for the full
shape.

---

### 2.1 `POST /api/internal/verify-token`

Wire contract is defined in [`internal/auth/http_store.go`](../internal/auth/http_store.go).
This is the **hot path** — called by beamd whenever a client
connects (or its 60s cache misses). Keep it cheap: a single indexed
query on a hashed token column.

**Request:**

```
POST /api/internal/verify-token
Authorization: Bearer <shared secret>
Content-Type: application/json

{"token": "<the beam bearer token>"}
```

**Response (valid token):**

```
200 OK
{"slug": "turing"}
```

**Response (unknown or revoked token):**

```
200 OK
{"slug": ""}
```

(Or `404` — beamd treats both as "reject".)

**Response (bad shared secret or transient error):**

```
401 / 500 / network error
```

Beamd treats anything non-2xx as a transient failure: the result is
**not** cached, and validation fails closed (deny). So make sure your
endpoint is genuinely healthy when it's healthy — don't 500 on benign
errors.

**Shared secret.** Set `BEAMD_AUTH_VERIFY_SECRET=<long random>` on
beamd and the same value on your web app. Reject calls without
matching `Authorization: Bearer ...`.

**Cache implications.** Beamd caches positive results for 60s,
negatives for 5s. A revoked token may keep working for up to ~60s
after revocation. Tune `defaultHTTPStoreTTL` if you need tighter
revocation latency.

**Drizzle sketch:**

```ts
export async function verifyToken(rawToken: string) {
  const hashed = sha256(rawToken);
  const row = await db.query.apiTokens.findFirst({
    where: and(eq(apiTokens.tokenHash, hashed), isNull(apiTokens.revokedAt)),
    with: { workspace: true },
  });
  return row?.workspace.slug ?? "";
}
```

---

### 2.2 `POST /api/device/code`

Wire contract from [`internal/devicecode/login.go`](../internal/devicecode/login.go),
which is RFC 8628-shaped.

**Request:** empty JSON body `{}`. (The CLI doesn't yet send a
`client_id`; you may add one if you support multiple CLI clients.)

**Response:**

```
200 OK
{
  "device_code":       "<32 random bytes, hex>",
  "user_code":         "ABCD-1234",
  "verification_uri":  "https://app.example.com/device",
  "expires_in":        600,
  "interval":          5
}
```

**State to persist:** create a `device_codes` row with
`(device_code, user_code, expires_at, status: 'pending', approved_user_id: null, slug: null, issued_token_id: null)`.

**User-code format.** Short, human-typeable. RFC 8628 suggests 8 chars
with a hyphen. `ABCD-1234` is fine. Make sure it's case-insensitive
on lookup.

**Don't** store the device_code in plaintext — hash it like a token.
The user_code can be plaintext since it's high-entropy in the moment
and expires in 10 minutes.

---

### 2.3 `POST /api/device/token`

Polled by the CLI on `interval`-second cadence until the user
approves (or expires).

**Request:**

```
POST /api/device/token
Content-Type: application/json

{"device_code": "<the device_code from /api/device/code>"}
```

**Response (still waiting):**

```
200 OK
{"error": "authorization_pending"}
```

**Response (approved):**

```
200 OK
{"access_token": "<the bearer token>"}
```

**Response (denied / expired):**

```
200 OK
{"error": "access_denied"}        # user clicked deny
{"error": "expired_token"}        # past expires_at
{"error": "slow_down"}            # client is polling too fast
```

Error codes mirror RFC 8628 §3.5. The CLI handles each of these
explicitly — see `Login()` in `internal/devicecode/login.go:122-138`.

**Important:** once `access_token` is returned, mark the device_code
row consumed. A second poll with the same device_code should return
`expired_token`, not the token again.

---

### 2.4 `GET /device` (the approval page)

This is the only user-facing piece. The CLI prints:

```
Open this URL in your browser:
  https://app.example.com/device

And enter this code:
  ABCD-1234
```

UX:

1. User lands on `/device`. If they're not signed in, route them
   through better-auth (Google/GitHub/magic-link — your call).
2. After auth, show a form: "Enter the code from your CLI" → user
   types `ABCD-1234`.
3. Look up the `device_codes` row by `user_code`. If missing,
   expired, or already-consumed, show an error.
4. Show a confirm screen: "Approve **beam** to act as your
   workspace **turing**? (Click confirm.)" Include device fingerprint
   if you have it, IP, geolocation hint — same shape as GitHub's
   device-code flow.
5. On confirm:
   - Find the user's workspace + a valid API token (mint one if they
     don't have one — see §3 for token format).
   - Update the `device_codes` row:
     `status: 'approved'`, `approved_user_id`, `slug`, `issued_token_id`.
   - The CLI's next poll picks up the token.
6. On deny: `status: 'denied'`. The CLI's next poll returns
   `access_denied`.

**Auto-claim slug at this point if they don't have one yet.** If the
user has no workspace, this is the moment to take a slug from them
(small text field on the confirm screen) and run the provisioning
flow from §4 before issuing the token. Otherwise the CLI receives a
token mapped to a slug whose DNS doesn't exist yet, and their first
`expose` will hang on DNS resolution.

---

### 2.5 `POST /api/internal/usage`

Receives per-slug deltas from beamd on a configurable interval
(default 60s). Shape from
[`internal/usage/reporter.go`](../internal/usage/reporter.go).

**Request:**

```
POST /api/internal/usage
Authorization: Bearer <shared secret>
Content-Type: application/json

{
  "events": [
    {
      "slug":           "turing",
      "bytes":          12345678,
      "active_tunnels": 3,
      "period_start":   "2025-06-01T12:00:00Z",
      "period_end":     "2025-06-01T12:01:00Z"
    },
    { "slug": "hopper", "bytes": 0, "active_tunnels": 0, ... }
  ],
  "requests_total_delta": 412
}
```

- `bytes` is a **delta** over `(period_start, period_end]`, not a
  cumulative total. Insert it into a `usage_events` table directly;
  rollups are your job.
- `active_tunnels` is a sample at `period_end`, not a delta.
- Beamd persists last-reported state to disk, so deltas remain
  correct across beamd restarts.

**Response:** `200 OK` (body ignored).

**Idempotency.** Beamd does not retry. If your endpoint returns
non-2xx the delta is **lost**. If billing accuracy matters, accept
the request, write to a durable queue, and process asynchronously.

**Shared secret.** Same model as the verify endpoint:
`BEAMD_USAGE_SECRET` on beamd, matched on the receiver.

---

## 3. Token format and storage

**Format:** 64 random bytes, hex-encoded. That's 128 hex characters.

```ts
import { randomBytes } from "node:crypto";

function newToken(): string {
  return randomBytes(64).toString("hex");
}
```

128 chars is long but the CLI never has to retype it — copy-paste
once into `~/.beam/config`, then it lives there. 512 bits of
entropy means the search space is permanent-future-proof.

**Storage:** never store raw tokens. Store SHA-256 hashes, look up by
hash.

```ts
import { sha256 } from "node:crypto"; // or @noble/hashes/sha256

export const apiTokens = pgTable("api_tokens", {
  id:           uuid("id").primaryKey().defaultRandom(),
  workspaceId:  uuid("workspace_id").references(() => workspaces.id).notNull(),
  tokenHash:    text("token_hash").notNull().unique(),
  label:        text("label"),
  createdAt:    timestamp("created_at").defaultNow().notNull(),
  lastUsedAt:   timestamp("last_used_at"),
  revokedAt:    timestamp("revoked_at"),
}, (t) => ({
  hashIdx: uniqueIndex("api_tokens_hash_idx").on(t.tokenHash),
}));
```

**On creation:** show the user the raw token **exactly once**. After
the page reload it's gone — they re-rotate if they lose it. This is
the standard "personal access token" pattern (GitHub, Stripe, etc.).

**Revocation:** set `revoked_at`. Verify queries already filter on
`isNull(revoked_at)`. Effective propagation time: up to ~60s
(beamd's positive cache TTL).

**Rotation:** users hit "regenerate" in the dashboard → revoke old,
mint new. UI should warn that active CLIs will lose connection
within 60s and need `beam login` again.

---

## 4. Slug provisioning at signup

When a user claims workspace slug `acme` for the first time, **before**
issuing them a token:

### 4.1 Validate the slug

- Lowercase ASCII, RFC 1123 label rules: `^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`.
  This is enforced again server-side in beamd
  ([`internal/naming/`](../internal/naming/)) — match it in your web
  app so users see "invalid name" client-side, not after their first
  `expose`.
- Unique in your `workspaces` table. Take an advisory lock or use a
  unique constraint to prevent two simultaneous signups racing for
  the same slug.
- Maintain a blocklist for reserved names: `www`, `api`, `admin`,
  `mail`, `support`, anything in your marketing site path tree.

### 4.2 Write DNS

For each new slug, write two A records (and AAAA if you serve IPv6):

```
acme.beam.example.com       A  <edge ip>
*.acme.beam.example.com     A  <edge ip>
```

Both are required. The wildcard alone won't cover the apex
(see [PRD §5.1](../prd.md)).

The edge IP comes from your droplet assignment logic (§5 below). If
you only run one droplet, it's hard-coded.

**Cloudflare API:**

```ts
async function provisionDNS(slug: string, edgeIp: string) {
  const zone = process.env.CLOUDFLARE_ZONE_ID!;
  const token = process.env.CLOUDFLARE_API_TOKEN!;

  for (const name of [`${slug}.${base}`, `*.${slug}.${base}`]) {
    await fetch(`https://api.cloudflare.com/client/v4/zones/${zone}/dns_records`, {
      method: "POST",
      headers: {
        Authorization: `Bearer ${token}`,
        "Content-Type": "application/json",
      },
      body: JSON.stringify({
        type: "A",
        name,
        content: edgeIp,
        proxied: false,
        ttl: 60,
      }),
    });
  }
}
```

Idempotent: catch the "record already exists" error and treat as
success. Cloudflare returns code `81057` for duplicates.

### 4.3 Pre-warm the certificate (optional)

Beamd will lazily issue `*.acme.beam.example.com` on the
workspace's first connection. The first `expose` then takes ~10s
while ACME completes — visible to the user.

To eliminate that delay, hit a hosted-mode admin endpoint on
beamd that triggers issuance immediately. **That endpoint does
not exist today** — see §6 "Open work."

Workaround until then: have new users tolerate the first-expose
delay, or shell out to `beamd add-developer --slug acme` from
your signup handler if your web app and a beamd droplet share
a host (most won't).

### 4.4 Insert into Postgres

```ts
await db.insert(workspaces).values({
  slug,
  ownerId: user.id,
  edgeDropletId: assignedDroplet.id,
  provisionedAt: new Date(),
});
```

Only after all three (validate, DNS, insert) succeed do you mint the
token in §3.

---

## 5. Multi-droplet placement

The OSS deployment runs one beamd droplet. Hosted will eventually
need more, sharded by slug.

The sharding is **per-slug at provision time**: each new slug is
permanently assigned to one droplet, and that droplet's IP is what
goes into the slug's DNS records. The client connects to
`<slug>.beam.example.com:443` — which resolves to its droplet,
where the wildcard cert lives.

Placement strategy:

- **Round-robin** across droplets is fine for v1. Track
  `(droplet_id, slug_count, bytes_last_30d)` and pick the
  least-loaded.
- **Sticky.** Once assigned, never move a slug. Each droplet's
  certmagic storage holds that slug's wildcard cert; migrating means
  re-issuing.
- **Failover** is out of scope. If a droplet dies, its slugs are
  down until you bring it back up (or manually reassign + re-issue).

Add a `droplets` table in your DB: `(id, public_ip, region, status,
slug_count)`. Your provisioning code in §4 picks one row at signup.

For multi-region later, pick the droplet closest to the
developer's region. Don't build this until you have customers asking.

---

## 6. Open work — gaps in beamd that hosted mode wants closed

These are beamd-side changes that would make the hosted web app
simpler. None are blockers; all are nice-to-have.

- **Admin endpoint for slug provisioning.** Today `beamd
  add-developer --slug X` only works as a CLI on the beamd host.
  An HTTP endpoint (`POST /admin/provision-slug`, shared-secret
  auth) would let the web app trigger DNS + cert pre-warm remotely.
  ~30 lines.
- **Token-bound slug change.** No way to move a slug between
  workspaces (e.g. user typo'd at signup). Today you'd manually
  delete the workspace, re-provision. Low priority.
- **Multi-droplet certmagic storage.** If you ever want two droplets
  to serve the same slug for HA, certmagic needs shared storage
  (S3-compatible). The library supports it; beamd doesn't expose
  the config knob yet.
- **Tighter token revocation latency.** Currently 60s cache. A
  beamd-side "token revoked" pubsub (Redis?) would drop this to
  near-instant. Probably overkill until a customer asks.

---

## 7. Configuration on the beamd side

Set these in `beamd.yaml` on each droplet:

```yaml
token_store: "https://app.example.com/api/internal/verify-token"

auth_discovery:
  device_code_url:  https://app.example.com/api/device/code
  token_url:        https://app.example.com/api/device/token
  verification_uri: https://app.example.com/device

usage_reporter:
  webhook_url:      https://app.example.com/api/internal/usage
  secret_env:       BEAMD_USAGE_SECRET
  interval_seconds: 60
  state_file:       /var/lib/beamd/usage-state.json
```

And these as env vars:

```
BEAMD_AUTH_VERIFY_SECRET=<shared secret matching your web app>
BEAMD_USAGE_SECRET=<another shared secret>
BEAMD_DNS_PROVIDER_CREDS=<Cloudflare token, if beamd still
                            writes DNS; in pure hosted mode the web
                            app writes DNS and beamd doesn't need it>
```

The CLI fetches `/.well-known/beam-auth` on `beam login` (no
`--token`) to discover the device-code endpoints. That's handled by
[`internal/edge/edge.go:610`](../internal/edge/edge.go) — nothing
to wire on your end beyond setting the YAML.

---

## 8. Recommended Postgres schema

A minimum viable schema. Adjust to taste.

```ts
export const users = pgTable("users", {
  id:        uuid("id").primaryKey().defaultRandom(),
  email:     text("email").notNull().unique(),
  createdAt: timestamp("created_at").defaultNow().notNull(),
});

export const workspaces = pgTable("workspaces", {
  id:            uuid("id").primaryKey().defaultRandom(),
  slug:          text("slug").notNull().unique(),
  ownerId:       uuid("owner_id").references(() => users.id).notNull(),
  edgeDropletId: uuid("edge_droplet_id").references(() => droplets.id).notNull(),
  provisionedAt: timestamp("provisioned_at").defaultNow().notNull(),
});

export const apiTokens = pgTable("api_tokens", {
  id:          uuid("id").primaryKey().defaultRandom(),
  workspaceId: uuid("workspace_id").references(() => workspaces.id).notNull(),
  tokenHash:   text("token_hash").notNull().unique(),
  label:       text("label"),
  createdAt:   timestamp("created_at").defaultNow().notNull(),
  lastUsedAt:  timestamp("last_used_at"),
  revokedAt:   timestamp("revoked_at"),
});

export const deviceCodes = pgTable("device_codes", {
  id:              uuid("id").primaryKey().defaultRandom(),
  deviceCodeHash:  text("device_code_hash").notNull().unique(),
  userCode:        text("user_code").notNull().unique(),
  status:          text("status").notNull().default("pending"),
  approvedUserId:  uuid("approved_user_id").references(() => users.id),
  issuedTokenId:   uuid("issued_token_id").references(() => apiTokens.id),
  expiresAt:       timestamp("expires_at").notNull(),
  createdAt:       timestamp("created_at").defaultNow().notNull(),
});

export const droplets = pgTable("droplets", {
  id:        uuid("id").primaryKey().defaultRandom(),
  publicIp:  text("public_ip").notNull(),
  region:    text("region"),
  status:    text("status").notNull().default("active"),
  slugCount: integer("slug_count").notNull().default(0),
});

export const usageEvents = pgTable("usage_events", {
  id:            uuid("id").primaryKey().defaultRandom(),
  workspaceId:   uuid("workspace_id").references(() => workspaces.id).notNull(),
  bytes:         bigint("bytes", { mode: "number" }).notNull(),
  activeTunnels: integer("active_tunnels").notNull(),
  periodStart:   timestamp("period_start").notNull(),
  periodEnd:     timestamp("period_end").notNull(),
});
```

Better-auth manages its own session/account tables on top of
`users`; follow its docs.

---

## 9. End-to-end flow recap

Putting it all together — a brand-new user, from web sign-up to a
working tunnel:

1. User signs up on `app.example.com` via better-auth (email,
   Google, whatever).
2. They claim slug `acme`. Web app:
   - Validates slug (RFC 1123 + reserved list + uniqueness).
   - Picks a droplet (round-robin).
   - Writes Cloudflare A records.
   - Inserts `workspaces` row.
3. Web app mints a token, stores hash, shows raw token to user once.
4. User installs CLI, runs `beam login --server beam.example.com:443 --token <token>`. CLI writes config.
5. User runs `beam expose 3001 --as api`.
6. Daemon dials beamd at `:443`, ALPN `beam/1`, sends `hello`
   with the token.
7. Beamd POSTs `/api/internal/verify-token` → web app returns
   `{"slug":"acme"}`. Cached 60s.
8. Beamd registers `api.acme.beam.example.com` → routes to
   this session.
9. First request to that URL: beamd issues
   `*.acme.beam.example.com` from Let's Encrypt via DNS-01 (~10s,
   one-time). Subsequent connects are instant.
10. Every 60s, beamd POSTs `/api/internal/usage` with byte
    deltas. Web app inserts into `usage_events`.

If you want the device-code path instead of copy-paste tokens, swap
step 4 for `beam login --server beam.example.com:443` (no
token), which fetches `/.well-known/beam-auth`, walks the
device-code dance against `/api/device/code` + `/api/device/token`,
and writes the token after browser approval.
