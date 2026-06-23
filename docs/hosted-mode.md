# Hosted mode — building the web app side

Beamd has hooks for "hosted mode": a web app issues and revokes
credentials, owns the customer dashboard, runs the interactive
(device-code) login flow, mints workspace API keys, and receives per-request
events for billing. Beamd itself stays stateless — it just validates a
bearer credential against your web app on each connection.

This doc specifies **what your web app has to expose** for those hooks
to work, so you can build it in any stack you like (the reference
hosted product is Next.js + better-auth + Drizzle + Postgres, but the
contract is framework-agnostic).

> **Read [`identity-and-accounts.md`](identity-and-accounts.md) first** for the
> canonical identity model — the two credential kinds (interactive **user
> session** vs headless **workspace API key**), how the CLI stores accounts
> per-server, and how scope is resolved. This doc defines the server-side
> contract (verify-token, device-code, token format, schema) that backs it.

Read [`prd.md`](../prd.md) first if you haven't — this doc assumes
you understand the slug model, the wildcard-cert-per-slug rule, and
the OSS happy path from [`setup.md`](setup.md).

---

## 1. Architecture

```
   [ browser ] ── sign-up / dashboard / approve device-code ──┐
                                                              ▼
   [ CLI ] ── beamd login (device-code) ──────▶ ┌──────────────────────────────────┐
                                                │  CONTROL PLANE — your web app    │
                                                │  one domain, e.g. app.example.com │
                                                │   · /.well-known/beam-auth +     │
                                                │     device-code (CLI logs in here)│
                                                │   · verify-token   ◀── each edge  │
                                                │   · request events ◀── each edge  │
                                                │   · DNS + cert provisioning       │
                                                └─────┬────────────────────▲────────┘
                                       Cloudflare API │                     │
                                                      ▼          verify-token + requests
                                                 ┌─────────┐               │
                                                 │   DNS   │               │
                                                 └─────────┘               │
                                                                           │
   [ CLI ] ── beamd open · tunnel :443 (ALPN beam/1) ──┐                   │
   [ public visitor ] ── HTTPS :443 ───────────────────┼──▶ ┌──────────────┴───────────┐
                                                       └────│  EDGE(S) — `beamd serve` │
                                                            │  SEPARATE domain per tier:│
                                                            │   paid  edge.example.com  │
                                                            │   free  edge-free.example │
                                                            │  TLS · route Host · yamux │
                                                            └───────────────────────────┘
```

Authoritative source of state is **your web app's Postgres**. Beamd caches token
lookups for ~60s and otherwise holds no user data.

**Control plane vs edges.** The host the CLI *logs in against* — the **control
plane**, your dashboard/api domain, baked into the published CLI — is **not**
where tunnels *flow*. Login assigns each account an **edge**, and paid vs free
tunnels live on **separate registrable domains** (reputation isolation; a
tunneled app is never same-site with your dashboard). You run one `beamd serve`
per edge domain, each pointing back at the control plane for verify-token +
request events. See [`identity-and-accounts.md`](identity-and-accounts.md) and the build
brief in [`web-app-handoff.md`](web-app-handoff.md).

---

## Open design decisions (revisit before launch)

These shape every hosted URL, cert, and the onboarding flow, and they're a
**breaking migration once customers exist** (baked into every URL + cert), so
decide before the first paying customer. Below: the URL options, the one
constraint that drives cert cost, the provisioning wait, how they tier, and
custom domains.

### URL options

Every hosted tunnel is `<something>.<tunnel-domain>`. The design question is how
customers are partitioned so names don't collide, and what that costs in certs.
The "scope" below is a personal handle or a team slug (see org model).

**1. Shared flat pool — a reserved subdomain ("no-namespace" lane).**
`<name>.tunnel.beamd.run` (pick the word: `tunnel` / `go` / `t` / `url` …). One
`*.tunnel.beamd.run` wildcard for *everyone* in the pool; `tunnel` is reserved
(nobody can claim it as a scope). Names are one global pool → auto-hashed
(ngrok's `cabbage-tuesday`) or first-come reservations. For users who just want
a URL and don't care about owning names. **One cert, instant, infinite scale,
shortest** — but no per-user name ownership, and a shared pool to keep clean.

**1′. Same, on a separate domain.** `<name>.beamd-free.sh`. Identical to #1 but
on its own registrable domain → **reputation isolation** (free-tier abuse can't
get `beamd.run` blocklisted) and one label shorter. The natural home for a free
tier — buy a couple `beamd-free.<tld>` to reserve it.

**2. Per-account subdomain (dot-nested).** `<name>.<scope>.beamd.run` →
`api.acme.beamd.run`. A `*.<scope>.beamd.run` wildcard **per account**. Names are
clean, grouped, and *owned* — `api`/`web` are yours, collision-free across
accounts. Prettiest; true per-account namespace — but a **cert per account**
(provisioning wait + LE ceiling, below). This is beamd's current model.

**3. Scope-in-label (hyphen-flattened — the Vercel shape).**
`<name>-<scope>.beamd.run` → `api-acme.beamd.run`. **One shared `*.beamd.run`**
covers it. Names are *owned per scope* (`api-acme` ≠ `api-trey` — contained,
collision-free) but the scope is jammed into the label, so **every URL is
longer**. The containment of #2 with the one-cert/instant economics of #1 —
exactly how Vercel does previews (`proj-hash-scope.vercel.app`). Needs a short
hash to be fully collision-proof.

**4. Custom domain (premium).** `<name>.acme.com` — the customer points their
own domain at your edge. Per-hostname cert, issued on-demand (see below). Fully
theirs, no `beamd.run` in sight.

### The one constraint that decides cert cost

A wildcard is **exactly one label deep**: `*.beamd.run` covers `x.beamd.run`,
never `x.y.beamd.run`. So:

- One label under the cert base (#1, #1′, #3) → **one shared wildcard**, issued
  once, instant, unlimited accounts.
- Two labels (#2's `api.acme.beamd.run`) → a **wildcard per account**, with a
  provisioning step and a rate ceiling.

There is no way to get dot-nesting under a single cert — which is the whole
reason #3 exists: it's the only way to get per-account *containment* without
per-account *certs*. (Containment is what stops an agent ripping through names
from exhausting a global pool — under #3 the scope partitions them; under a
single flat pool (#1) you lean on hashes instead.)

### The provisioning wait (#2 only) — and how to erase it

Issuing `*.<scope>.beamd.run` via ACME DNS-01 took **~8 seconds** in our own
deploy (Cloudflare TXT write → LE validates → issued). One-time per account.

- **Don't issue lazily at first `open`** — that puts the 8s in the user's face.
- **Pre-warm at signup/upgrade.** `PreWarm` already exists (it's what
  `add-developer` calls); hosted needs the admin endpoint (§6, ~30 lines) to
  trigger it remotely. Fire it during a "setting up your namespace…" step → the
  first tunnel is instant.
- If #2 is a **paid** option, pre-warm only runs for paying users (low volume),
  so LE's **~50 new certs/week per registered domain** never bites; when it
  eventually might, you have revenue to shard tunnel domains or request an LE
  rate-limit increase (free, granted to real services).

### How they combine into tiers (leaning)

Not exclusive — mix them. A reasonable shape:

| Tier | URL | Mechanism |
|---|---|---|
| **Free** | `myapp-x7k2.beamd-free.sh` | shared flat pool (#1′), hashed names |
| **Standard** | `api-acme.beamd.run` | scope-in-label (#3) — contained, one cert, instant |
| **Pro (pretty)** | `api.acme.beamd.run` | per-account subdomain (#2), pre-warmed at upgrade |
| **Enterprise** | `api.acme.com` | custom domain (#4) |

Reframings from the design discussion:

- **Namespacing isn't the free/paid gate.** Everyone gets *containment* (the
  scope is always present — via the pool or the label); what's tiered is how
  *pretty* it renders — hyphen (#3) → dot-subdomain (#2) → custom domain (#4) —
  plus reputation (free domain vs the main one).
- **"No namespace" is its own lane** (#1/#1′) for people who just want a URL — a
  reserved subdomain (`*.tunnel.beamd.run`) or the free domain. Some *paid* users
  will prefer this (short, flat) over a namespace, so don't force one on them.
- **Agents can't exhaust a global pool** under #3 — the scope partitions them —
  which is the case for the scope being always-present, achieved without
  per-account certs.

**Still open:** whether Standard is #3 (hyphen, one cert, longer URL) or #2
(dot, per-account cert, prettier), and the exact reserved word/domain for the
no-namespace lane.

> **Downstream:** §4 (per-slug DNS provisioning), §5 (slug-sharded droplets),
> and the §9 recap assume the **per-account-subdomain (#2)** model. Under a
> single-wildcard lane (#1/#1′/#3) they collapse — one `*.<base>` cert + one
> `*.<base>` A record cover *all* tunnels (no per-account provisioning), and
> droplet sharding keys on a hash, not the slug.

### Custom domains — how, and how hard

Moderately easy; certmagic is built for it:

- Customer CNAMEs `tunnel.acme.com` → your edge; you verify ownership (a
  TXT/CNAME check) and add it to an allowlist.
- Enable certmagic **On-Demand TLS** with an authorization callback that checks
  that allowlist per handshake. It issues a per-hostname cert via
  **TLS-ALPN-01 / HTTP-01** — which only need the domain to *resolve to your
  edge*, **not** access to the customer's DNS (the thing that makes DNS-01
  impossible for domains you don't control).
- Per-hostname (not wildcard) → back under per-cert rate limits, but custom
  domains are premium/low-volume, so fine.
- beamd today is DNS-01 + per-slug wildcards; this is "enable OnDemand + an
  allowlist hook + an ownership-verify step." ~**1–2 days** — exactly what Caddy
  does for automatic HTTPS on arbitrary domains.

### Org / team model (Vercel-shaped)

- A **user** has a **personal scope** (a slug, e.g. `trey`) and can **join
  multiple teams**, each its own scope (e.g. `acme`).
- A tunnel belongs to a scope; the scope is what appears in the URL (`…-acme`
  flat, or `.acme.` nested). The user/agent never types it — it's resolved from
  the project `beamd.yaml`, a `--scope` flag, or their default (see
  [`identity-and-accounts.md`](identity-and-accounts.md)).
- **Scope is carried by the credential — two ways, not "per membership":**
  - a **user session** (interactive login) authorizes the user's **whole set**
    of scopes; `verify-token` returns that set, and the edge authorizes the
    *requested* scope against it. This is what makes "log in once, act across
    all your orgs" work without re-auth.
  - a **workspace API key** authorizes **exactly one** scope; `verify-token`
    returns that single slug. This is the headless/automation credential, and
    its narrow blast radius is the point.
- **Client side:** one login per *server* spans all your orgs; the org is a
  lightweight **selector** (`--scope` / `beamd.yaml` / `beamd default`), never a
  separate login. (The old per-org `beamd use` / `beamd profiles` model is
  replaced — see the canonical doc.)
- Schema (extends §8): add `teams` + `memberships` (user↔team + role) — these
  drive a session's scope set and gate *who may mint a key in a scope*. A
  tunnel/workspace belongs to a scope (a personal user **or** a team).
  `api_tokens` stays **workspace-scoped** (one slug) with a `created_by_user_id`
  for attribution only — the key's authority is the workspace, not the person.

### Auth-gated previews (pairs with the flat shape)

Optionally **require the viewer to be authenticated** — logged into the
dashboard, or holding a view token / signed URL — before the edge proxies a
preview. This matters most for the **shared-pool / hyphen lanes (#1/#1′/#3)**:
those tunnels are siblings in one scannable namespace, so a gate stops casual
leakage of in-progress agent previews (and lets a flat lane trade obscurity for
an actual access check).

Shape: per-tunnel visibility (`public | org | private`) lives in the control
plane and is returned to the edge; for non-public, the edge checks a session
cookie (set by the dashboard, scoped so the edge can validate it) or a
signed/expiring URL param before proxying — unauthenticated → 302 to login or
403. Builds on the token/signing work in
[`preview-auth-spec.md`](preview-auth-spec.md). (Off for OSS/self-host —
tunnels stay public by default, as today.)

### Domains & PSL

- **Buy a couple `beamd-free.<tld>` now** to host the free-tier pool (#1′) —
  reputation isolation (the ngrok-free.app split), before someone else grabs
  them.
- The website-domain vs tunnel-domain assignment across `beamd.{ai,sh,dev,io}`
  is **still open** (owner's call); everything above is agnostic to which
  becomes the tunnel base.
- Whichever domain hosts tunnels, submit it to the **Public Suffix List** so
  tunnels can't cookie-poison each other or the dashboard.

---

## 2. Endpoints your web app must expose

Four HTTP endpoints, all called by beamd. None are user-facing.

| Endpoint | Caller | Purpose |
|---|---|---|
| `POST /api/internal/verify-token` | beamd, per session | Resolve a bearer credential to its scope(s) — a **set** for a user session, a single slug for an API key |
| `POST /api/device/code` | beamd CLI, once per login | Issue a device + user code |
| `POST /api/device/token` | beamd CLI, polling | Return the **user session** once the user approves |
| `POST /api/internal/requests` | beamd, batched | Receive per-request event batches for billing + analytics |

Plus one user-facing page:

| Page | Caller | Purpose |
|---|---|---|
| `GET /device` | developer's browser | Enter the user code + sign in + approve |

Beamd is configured to call these via `auth_discovery:` and
`request_reporter:` in `beamd.yaml`. See
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

{"token": "<the beamd bearer token>"}
```

**Response — a user session** (interactive login; authorizes the user's whole
scope set):

```
200 OK
{
  "kind":   "session",
  "user":   "trey@example.com",
  "scopes": [
    {"slug": "trey", "role": "owner"},
    {"slug": "acme", "role": "member"}
  ]
}
```

The edge caches this and, when a tunnel registers naming a scope, authorizes
that scope against the set. A request for a scope not in the set is rejected at
connect (the user was removed, or never a member).

**Response — a workspace API key** (headless/automation; one scope):

```
200 OK
{"kind": "key", "slug": "acme"}
```

A bare `{"slug": "acme"}` with no `kind` is still accepted and treated as a
single-scope key, so the pre-session contract (and the OSS FileStore shape)
keeps working unchanged.

**Response (unknown or revoked credential):**

```
200 OK
{"slug": ""}        # empty slug, empty scopes, or 404 — all mean "reject"
```

> **Empty slug means reject, *not* flat.** Self-hosted beamd treats a
> `{token: ""}` file entry as a valid **flat** tunnel (`<name>.<base>`); the
> hosted HTTP store deliberately does the opposite — an empty slug is always a
> reject (`internal/auth/http_store.go`). So a valid hosted token must resolve
> to a **non-empty** scope: flat routing is self-host-only, and hosted's
> "no-namespace" lane (design notes above) still carries a non-empty reserved
> scope like `tunnel`.

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

  // 1. Workspace API key → one scope.
  const key = await db.query.apiTokens.findFirst({
    where: and(eq(apiTokens.tokenHash, hashed), isNull(apiTokens.revokedAt)),
    with: { workspace: true },
  });
  if (key) return { kind: "key", slug: key.workspace.slug };

  // 2. User session → the user's whole scope set (personal + every team).
  const session = await verifySessionToken(rawToken); // better-auth
  if (session) {
    const scopes = await scopesForUser(session.userId); // personal + memberships
    return { kind: "session", user: session.email, scopes };
  }

  return { slug: "" }; // reject
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
`(device_code, user_code, expires_at, status: 'pending', approved_user_id: null, issued_session_id: null)`.
(No per-row `slug` — interactive login issues a **user session** that spans
every scope, not a single-workspace token.)

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
{
  "access_token": "<the user-session token>",
  "edge":   "tunnels.beamd.app",
  "scopes": [
    {"slug": "trey", "role": "owner"},
    {"slug": "acme", "role": "member"}
  ]
}
```

The `access_token` is a **user session** (§3), not a single-workspace token —
it authorizes the user's whole scope set.

**`edge` and `scopes` are how the CLI learns where to point — this is the
control-plane vs edge split.** The host the CLI logged in against (the *control
plane* — your dashboard/api domain, baked into the published CLI) is **not**
necessarily where tunnels flow:

- **`edge`** — the tunnel edge this account uses, returned per **tier**. Free and
  paid tunnels live on **different registrable domains** on purpose: free-tier
  abuse can't get the paid domain blocklisted, and a malicious tunneled app is
  never same-site with your dashboard (no cookie/session exfiltration). The CLI
  keys the account by this `edge`, so `beamd open` connects there. Omit it and
  the CLI falls back to the login host (single-domain / self-host).
- **`scopes`** — the user's set; the CLI caches it for `beamd orgs` and picks the
  first as the standing default. (The edge still re-checks scope on every
  connect via verify-token; this cache is for display + selection only.)

So a hosted user runs a bare `beamd login` (no `--server`): the CLI hits the
baked-in control plane, and *this response* tells it the edge + orgs. `--server`
stays the self-host opt-out.

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
4. Show a confirm screen: "Approve **beamd** to sign in as **you** on this
   device? It will be able to act across your orgs. (Click confirm.)" Include
   device fingerprint if you have it, IP, geolocation hint — same shape as
   GitHub's device-code flow.
5. On confirm:
   - Issue a **user session** for the approved user (better-auth) — this is the
     bearer the CLI stores; it spans every scope they belong to. (API keys are
     not minted here; those are created explicitly in the dashboard, §3.)
   - Update the `device_codes` row:
     `status: 'approved'`, `approved_user_id`, `issued_session_id`.
   - The CLI's next poll picks up the session token.
6. On deny: `status: 'denied'`. The CLI's next poll returns
   `access_denied`.

**Auto-claim a personal scope here if they don't have one yet.** If the user
has no workspace, this is the moment to take a slug from them (small text field
on the confirm screen) and run the provisioning flow from §4 before issuing the
session. Otherwise the session's personal scope has no DNS yet, and their first
`open` hangs on DNS resolution.

---

### 2.5 `POST /api/internal/requests`

Receives **batches of per-request events** that the edge ships as it tails its
request log. Shape from
[`internal/reqlog/reqlog.go`](../internal/reqlog/reqlog.go); full contract in
[`request-events-spec.md`](request-events-spec.md).

**Request:**

```
POST /api/internal/requests
Authorization: Bearer <shared secret>
Content-Type: application/json

{
  "events": [
    {
      "request_id":   "0190f7c2-1234-7abc-89de-0123456789ab",
      "slug":         "turing",
      "host":         "api-turing.edge.example.com",
      "method":       "GET",
      "path":         "/v1/things",
      "status":       200,
      "outcome":      "ok",
      "bytes_in":     412,
      "bytes_out":    8123,
      "is_websocket": false,
      "started_at":   "2025-06-01T12:00:00.123Z",
      "ended_at":     "2025-06-01T12:00:00.187Z"
    }
  ]
}
```

- One self-contained event per completed request, plus per-window heartbeats for
  long-lived connections (`outcome: "in_progress"`). Attribute billing via
  `slug → organization`; `host` identifies the URL (custom domains included).
- `request_id` is an **edge-minted uuidv7** and the idempotency key: bulk-insert
  with `onConflictDoNothing` on it (map wire `request_id` → your `id` PK).
- Analytics fields (`path`, `client_ip`, `user_agent`, `referer`) are
  capture-configurable at the edge and may be absent; billing fields always ship.

**Response:** `200 {"ok": true, "accepted": <n>}`.

**Delivery is at-least-once, not fire-and-forget.** The edge advances its file
cursor **only on a 2xx**, so a non-2xx (or your endpoint being down) makes it
**retry the same window** on the next tick — nothing is lost across an outage,
but you **will** see replays. Dedupe on `request_id`. Keep the handler cheap and
idempotent; when in doubt, accept + enqueue.

**Shared secret.** Same model as the verify endpoint:
`BEAMD_REQUESTS_SECRET` on beamd, matched on the receiver.

---

## 3. Credentials: user sessions & API keys

Two credential kinds back the two `verify-token` responses (see
[`identity-and-accounts.md`](identity-and-accounts.md) for the why):

- **User session** — minted by the device-code flow (§2.3), it *is* a
  better-auth session/refresh token. It represents **the user** and authorizes
  their whole scope set. Better-auth owns its storage (§8); you don't put it in
  `api_tokens`. `verify-token` validates it and derives the scope set from the
  user's memberships.
- **Workspace API key** — the headless/automation credential below. It
  represents **a workspace** (one scope), is the "personal access token"
  pattern, and is what goes in a `--config` file or a CI secret.

The rest of this section is the **API key**.

**Format:** 64 random bytes, hex-encoded. That's 128 hex characters.

```ts
import { randomBytes } from "node:crypto";

function newToken(): string {
  return randomBytes(64).toString("hex");
}
```

128 chars is long but it's copy-pasted once — into a `--config` file or
`beamd login --token <key>`. 512 bits of entropy means the search space is
permanent-future-proof.

**Headless issuance.** API keys are created in the dashboard ("Create API key"
→ name + workspace), **not** via device-code — that's the whole point: CI and
agents have no browser. A workspace can have **multiple named keys** (`ci`,
`flow-prod`, `laptop`), each independently revocable. The `label` column is
what makes them nameable.

**Storage:** never store raw tokens. Store SHA-256 hashes, look up by
hash.

```ts
import { sha256 } from "node:crypto"; // or @noble/hashes/sha256

export const apiTokens = pgTable("api_tokens", {
  id:              uuid("id").primaryKey().defaultRandom(),
  workspaceId:     uuid("workspace_id").references(() => workspaces.id).notNull(),
  createdByUserId: uuid("created_by_user_id").references(() => users.id), // attribution only
  tokenHash:       text("token_hash").notNull().unique(),
  label:           text("label"),
  createdAt:       timestamp("created_at").defaultNow().notNull(),
  lastUsedAt:      timestamp("last_used_at"),
  revokedAt:       timestamp("revoked_at"),
}, (t) => ({
  hashIdx: uniqueIndex("api_tokens_hash_idx").on(t.tokenHash),
}));

// The key's *authority* is `workspaceId`; `createdByUserId` is for the audit
// trail and "revoke when they leave", never for what the key can do.
```

**On creation:** show the user the raw token **exactly once**. After
the page reload it's gone — they re-rotate if they lose it. This is
the standard "personal access token" pattern (GitHub, Stripe, etc.).

**Revocation:** set `revoked_at`. Verify queries already filter on
`isNull(revoked_at)`. Effective propagation time: up to ~60s
(beamd's positive cache TTL).

**Rotation:** revoke the old key, create a new one, update the consumer's
`--config` (or `beamd login --token <new>`) and run `beamd reload` so the
detached agent picks up the new credential. With multiple named keys you rotate
*one* (e.g. `ci`) without disturbing the others. A revoked key keeps working for
up to ~60s (the positive-cache TTL).

---

## 4. Slug provisioning at signup

When a user claims workspace slug `acme` for the first time, **before**
issuing them a token:

### 4.1 Validate the slug

- Lowercase ASCII, RFC 1123 label rules: `^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`.
  This is enforced again server-side in beamd
  ([`internal/naming/`](../internal/naming/)) — match it in your web
  app so users see "invalid name" client-side, not after their first
  `open`.
- Unique in your `workspaces` table. Take an advisory lock or use a
  unique constraint to prevent two simultaneous signups racing for
  the same slug.
- Maintain a blocklist for reserved names: `www`, `api`, `admin`,
  `mail`, `support`, anything in your marketing site path tree.

### 4.2 Write DNS

> **What you write depends on the URL shape** (see "Open design decisions"). The
> example below is the **per-scope nested shape** — a wildcard *per slug*. If you
> pick a flat or hyphen shape instead, you provision the edge domain's wildcard
> **once** (`*.edge.example.com`) and write **nothing per-slug**. Match this to
> your cert strategy ("the one constraint that decides cert cost").

For the nested shape, write two A records per new slug (and AAAA if you serve
IPv6) against that slug's assigned **edge** domain:

```
acme.edge.example.com       A  <edge ip>
*.acme.edge.example.com     A  <edge ip>
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

Beamd will lazily issue the slug's wildcard (e.g. `*.acme.edge.example.com`,
nested shape) on the workspace's first connection. The first `open` then takes
~10s while ACME completes — visible to the user.

To eliminate that delay, hit a hosted-mode admin endpoint on
beamd that triggers issuance immediately. **That endpoint does
not exist today** — see §6 "Open work."

Workaround until then: have new users tolerate the first-open
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

The sharding granularity follows your URL shape. With the **nested** shape it's
**per-slug** at provision time: each new slug is permanently assigned to one
droplet, that droplet's IP goes into the slug's DNS, and its per-slug wildcard
cert lives there. With a **flat/hyphen** shape it's **per edge domain**: one
shared wildcard per edge, routed by Host within it. Either way a client connects
to its edge on `:443`, which resolves to the droplet holding that cert.

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

request_reporter:
  webhook_url:      https://app.example.com/api/internal/requests
  secret_env:       BEAMD_REQUESTS_SECRET
  batch_size:       500
  flush_ms:         1000
  cursor_file:      /var/lib/beamd/requests.cursor
```

And these as env vars:

```
BEAMD_AUTH_VERIFY_SECRET=<shared secret matching your web app>
BEAMD_REQUESTS_SECRET=<another shared secret>
BEAMD_DNS_PROVIDER_CREDS=<Cloudflare token, if beamd still
                            writes DNS; in pure hosted mode the web
                            app writes DNS and beamd doesn't need it>
```

The CLI fetches `/.well-known/beam-auth` on `beamd login` (no `--token`) to
discover the device-code endpoints. **In the hosted multi-domain setup the
CLI's baked-in default is the control plane, so the control plane serves
`/.well-known/beam-auth` directly** (returning the `device_code_url` /
`token_url` / `verification_uri` above). The `auth_discovery:` block on an
*edge* (the YAML above) is only used when a CLI dials that edge **directly**
(self-host); in pure hosted mode it's optional. See
[`internal/edge`](../internal/edge/) for the edge-served path.

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
  id:              uuid("id").primaryKey().defaultRandom(),
  workspaceId:     uuid("workspace_id").references(() => workspaces.id).notNull(),
  createdByUserId: uuid("created_by_user_id").references(() => users.id), // attribution only
  tokenHash:       text("token_hash").notNull().unique(),
  label:           text("label"),
  createdAt:       timestamp("created_at").defaultNow().notNull(),
  lastUsedAt:      timestamp("last_used_at"),
  revokedAt:       timestamp("revoked_at"),
});

// Org model: a workspace's scope is either personal (owner is a user) or a
// team. `memberships` is what a user session's scope set is built from, and
// what gates who may mint a key in a team's workspace.
export const teams = pgTable("teams", {
  id:          uuid("id").primaryKey().defaultRandom(),
  workspaceId: uuid("workspace_id").references(() => workspaces.id).notNull(),
  name:        text("name").notNull(),
});

export const memberships = pgTable("memberships", {
  id:      uuid("id").primaryKey().defaultRandom(),
  userId:  uuid("user_id").references(() => users.id).notNull(),
  teamId:  uuid("team_id").references(() => teams.id).notNull(),
  role:    text("role").notNull().default("member"), // owner | admin | member
}, (t) => ({ uniq: uniqueIndex("memberships_user_team_idx").on(t.userId, t.teamId) }));

export const deviceCodes = pgTable("device_codes", {
  id:              uuid("id").primaryKey().defaultRandom(),
  deviceCodeHash:  text("device_code_hash").notNull().unique(),
  userCode:        text("user_code").notNull().unique(),
  status:          text("status").notNull().default("pending"),
  approvedUserId:  uuid("approved_user_id").references(() => users.id),
  issuedSessionId: text("issued_session_id"), // the better-auth session handed to the CLI
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

// Raw per-request events shipped by each edge (§2.5). `id` is the edge-minted
// uuidv7 (wire `request_id`) — the idempotency key, so it has NO DB default.
// Billing/dashboards read a derived `usage_daily` rollup, not these raw rows.
// Canonical DDL (indexes + the rollup table) in
// [`request-events-spec.md`](request-events-spec.md) §5.3.
export const requestEvent = pgTable("request_event", {
  id:             uuid("id").primaryKey(),    // = wire request_id; no DB default
  connectionId:   uuid("connection_id"),      // groups a long connection's heartbeats
  organizationId: uuid("organization_id").references(() => workspaces.id),
  slug:           text("slug"),               // NULL on no_route (no session)
  host:           text("host").notNull(),
  method:         text("method").notNull(),
  path:           text("path"),               // NULL when capture.path is off
  status:         integer("status").notNull(),
  outcome:        text("outcome").notNull(),
  bytesIn:        bigint("bytes_in",  { mode: "number" }).notNull(),
  bytesOut:       bigint("bytes_out", { mode: "number" }).notNull(),
  ttfbMs:         integer("ttfb_ms"),
  isWebsocket:    boolean("is_websocket").notNull().default(false),
  clientIp:       text("client_ip"),
  userAgent:      text("user_agent"),
  referer:        text("referer"),
  startedAt:      timestamp("started_at").notNull(),
  endedAt:        timestamp("ended_at").notNull(),
  createdAt:      timestamp("created_at").defaultNow().notNull(),
});
```

Better-auth manages its own session/account tables on top of `users`; follow
its docs. **The user-session credential that `verify-token` validates is one of
those better-auth sessions** — interactive login is just the device-code flow
(§2.2–2.4) handing the CLI a session it can replay. API keys (above) are
separate and workspace-scoped; sessions are user-scoped.

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
3. User installs the CLI and runs `beamd login` (no flags) → it hits the
   baked-in **control plane**, does browser/device-code, and the approval
   response assigns the account's **edge** (paid vs free domain) + scope set.
   The CLI stores a **user session** keyed by that edge; `acme` becomes the
   default scope. (Self-host instead passes `--server <edge> --token`.)
4. User runs `beamd open 3001 --as api` — it lands in scope `acme` (their
   default; `--scope` or a project `beamd.yaml` would pick another).
5. Client dials beamd at `:443`, ALPN `beam/1`, sends `hello` with the
   session token and the requested scope `acme`.
6. Beamd POSTs `/api/internal/verify-token` → web app returns the session's
   **scope set**; beamd checks `acme` is in it. Cached 60s.
7. Beamd registers the tunnel host (nested-shape example:
   `api.acme.edge.example.com`) → routes to this session.
8. First request to that URL: beamd issues the slug's wildcard (e.g.
   `*.acme.edge.example.com`) from Let's Encrypt via DNS-01 (~10s, one-time).
   Subsequent connects are instant.
9. As requests flow, beamd ships event batches to `/api/internal/requests`. Web
   app bulk-inserts `request_event` rows (idempotent on `request_id`) and rolls
   them up into `usage_daily` for billing.

**Automation / CI / agents** swap steps 3–4: create a workspace **API key** in
the dashboard and pass it via `beamd open --config <path>` (an `{server, token}`
file). No browser, no scope selection — the key *is* the scope, so verify-token
returns a single `{"slug":"acme"}` instead of a scope set.
