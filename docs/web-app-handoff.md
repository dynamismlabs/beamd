# Hosted web-app build brief (handoff)

> **For the engineer/agent building the control plane.** Beamd's edge + CLI are
> stateless and call into your web app; this is everything your app must expose
> for hosted mode to work. The canonical, detailed spec is
> [`hosted-mode.md`](hosted-mode.md); the client model is
> [`identity-and-accounts.md`](identity-and-accounts.md). This doc is the
> focused "what to build" cut. Reference hosted product stack: Next.js +
> better-auth + Drizzle + Postgres, but the contract is framework-agnostic.

## Architecture: control plane vs edges (this drives everything)

- **Control plane = your app** (one domain, e.g. `app.beamd.sh`). It serves the
  dashboard, device-code login, `verify-token`, usage ingestion, and owns the
  Postgres source of truth. The published CLI **bakes this host in** (a bare
  `beamd login` targets it).
- **Edges = beamd `serve` instances on *separate tunnel domains*.** For
  security + reputation you run **at least two**: a **paid** edge (e.g.
  `beamd.app`) and a **free** edge (e.g. `beamd-free.sh`) — *different
  registrable domains* so free-tier abuse can't get the paid domain
  blocklisted, and a tunneled app is never same-site with your dashboard (no
  cookie/session theft). Each edge validates credentials by calling **your**
  `verify-token` and reports usage to **your** usage endpoint. Beamd holds no
  user data — it caches token lookups ~60s and otherwise asks you.
- **The CLI bakes in only the control-plane host.** *You* tell the CLI which
  edge a user gets (per tier) in the login response — the edge is **data you
  return, never a CLI constant**, so changing tier→edge needs no CLI rebuild.

**Decide three domains:**
- **control plane** — baked into the CLI as `DefaultHost` (the login target).
- **paid edge** — where paid tunnels live.
- **free edge** — a separate registrable domain for the free tier.

## Two credential types (one bearer, two shapes)

| | Issued by | Authorizes | Used by |
|---|---|---|---|
| **User session** | device-code login | the user's **whole scope set** | humans (the CLI) |
| **Workspace API key** | dashboard "Create API key" | **one** workspace scope | CI / agents (in a `--config` file) |

Both are bearer tokens your `verify-token` resolves; they differ only in the
response shape (set vs single slug).

## Endpoints to implement

### 1. `GET /.well-known/beam-auth` (on the control plane)
The first thing the CLI fetches. Return the device-code URLs:
```json
{ "device_code_url":  "https://app.beamd.sh/api/device/code",
  "token_url":        "https://app.beamd.sh/api/device/token",
  "verification_uri": "https://app.beamd.sh/device" }
```
(Empty/404 means "no device-code offered" — that's the OSS case, N/A for you.)

### 2. `POST /api/device/code`
Req: `{}`. Res (RFC 8628-shaped):
```json
{ "device_code":"<32 random bytes, hex>", "user_code":"WXYZ-7K9P",
  "verification_uri":"https://app.beamd.sh/device", "expires_in":600, "interval":5 }
```
Persist a `device_codes` row (**hash** the device_code; `user_code` may be plaintext, case-insensitive lookup, short TTL).

### 3. `POST /api/device/token` (polled by the CLI) — ⭐ the key one
Req: `{"device_code":"…"}`.
- Pending: `{"error":"authorization_pending"}` (also `slow_down` / `access_denied` / `expired_token`, per RFC 8628 §3.5).
- **On approval:**
```json
{ "access_token": "<user-session token>",
  "edge":   "beamd.app",
  "scopes": [ {"slug":"trey","role":"owner"}, {"slug":"acme","role":"member"} ] }
```
- `access_token` = a **user session** (a better-auth session the CLI replays), **not** a single-workspace token.
- `edge` = the user's tunnel edge **per tier** (paid vs free domain). The CLI keys the account by this and connects there.
- `scopes` = the user's orgs (personal + teams). The CLI caches them and uses the **first** as the standing default.
- Once returned, mark the device_code consumed (a second poll → `expired_token`).

### 4. `GET /device` (the only user-facing page)
User signs in (better-auth — Google/GitHub/magic-link/your call), enters the
`user_code`, approves. On confirm: **issue a user session**, mark the row
approved, and attach the assigned `edge` + `scopes`. If the user is brand new,
**claim a personal slug and provision its DNS + cert before returning** —
otherwise their first `open` hangs on DNS resolution.

### 5. `POST /api/internal/verify-token` (called by each **edge**, hot path)
Auth header: `Authorization: Bearer <BEAMD_AUTH_VERIFY_SECRET>` (same secret on
the edge and here). Req: `{"token":"<bearer>"}`. Keep it cheap (one indexed
query on a hashed token column).
- **user session** → `{"kind":"session","user":"trey@…","scopes":[{"slug":"…","role":"…"}]}`
- **API key** → `{"kind":"key","slug":"acme"}` (a bare `{"slug":"acme"}` is also accepted)
- **reject** (unknown/revoked) → `{"slug":""}` or `404`/`401`

The edge caches the result ~60s and authorizes the *requested* scope (the CLI
sends it) against the set; a scope the session can't act in is rejected at
connect. Anything non-2xx = transient → the edge denies and does not cache.

### 6. `POST /api/internal/usage` (called by each edge, ~60s)
Per-slug byte/active-tunnel deltas for billing. Shape + verification in
[`hosted-mode.md`](hosted-mode.md) §2.5; insert into `usage_events`.

## Postgres schema
`users`, `workspaces`, `teams`, `memberships`, `api_tokens` (workspace-scoped,
plus `created_by_user_id` for attribution only), `device_codes` (with
`issued_session_id`), `droplets`, `usage_events`. Full Drizzle DDL in
[`hosted-mode.md`](hosted-mode.md) §8. Better-auth manages its own session
tables on top of `users`; the session that `verify-token` validates is one of
those. A user's scope set = their personal workspace + the workspaces of teams
they're a member of (drives the `scopes` array in #3 and #5).

## How each edge connects back to you (the edge's `beamd.yaml`)
```yaml
base_domain: beamd.app          # the paid edge (run a second serve on beamd-free.sh for free)
token_store: "https://app.beamd.sh/api/internal/verify-token"
usage_reporter:
  webhook_url: "https://app.beamd.sh/api/internal/usage"
  interval_seconds: 60
# set BEAMD_AUTH_VERIFY_SECRET (and the usage secret) in the edge's env
```
Each edge also needs its tunnel-domain DNS (`A`/`AAAA` + wildcard) and a
wildcard cert; provisioning details in [`setup.md`](setup.md) /
[`deploy-spec.md`](deploy-spec.md).

## What the CLI already does (the client side, for context)
- `beamd login` (no flags) → your `/.well-known/beam-auth` → device-code →
  stores an account **keyed by the returned `edge`**, caches `scopes`,
  default = first scope. `--server` is the self-host opt-out.
- On tunnel connect the CLI sends `{token, scope}` in the hello; the edge calls
  `verify-token` and rejects a scope the session can't use.
- `--config` automation (a workspace API key) bypasses all of this and is
  unchanged.

## Suggested build order
1. **Schema + better-auth** (users, workspaces, teams, memberships, sessions).
2. **`/.well-known` + device-code** (#1–#4) → a bare `beamd login` works end to end.
3. **`verify-token`** (#5) → tunnels actually authorize (and the scope set gates orgs).
4. **`usage`** (#6) → billing.
5. **Dashboard "Create API key"** (workspace-scoped, named, shown once) → CI/agents.

## Publish the hosted CLI (when ready)
Bake the control-plane host into the published `@beamd/cli`:
```
BEAMD_DEFAULT_HOST=app.beamd.sh make publish-npm VERSION=x.y.z
```

## Open item on the CLI side
`beamd orgs --refresh` (re-fetch the scope set without re-login) is not built
yet — it needs a small CLI-facing "list my scopes" endpoint (e.g.
`GET /api/scopes` with the session bearer). Not required for launch; flag if you
want it and we'll spec + wire it.
