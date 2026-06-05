# URL & hostname model — spec

> How a hosted scope's public URL is chosen, changed, and routed. **One source of
> truth** for the slug, the URL shape, slug rename, shape migration, and custom
> domains — all of which are the same idea: *a scope answers on a **set** of
> hostnames, exactly one of which is **primary** (what we render).*
>
> Builds on [`hosted-mode.md`](hosted-mode.md) (§"URL options") and
> [`identity-and-accounts.md`](identity-and-accounts.md) (scope model). Control
> plane = `beamd-web` (Next.js + Drizzle + Postgres); edge + CLI = this repo
> (`beamd`, Go). Tasks tagged **[CP] / [EDGE] / [CLI]**.
>
> *(Supersedes the former `custom-domains-spec.md` — custom domains are now §8
> here.)*

---

## 1. The model

Three layers the slug currently conflates:

| Layer | What | Mutable? |
|---|---|---|
| **Identity** | the slug — owned forever (`claimed_slug`), the routing key | never; only *added to* |
| **Hostname set** | every Host a scope answers on | grows/shrinks freely |
| **Primary** | the one host rendered in dashboard / CLI / REST | swappable anytime |

A scope's **hostname set** is the union of:

- **default-shape host(s)** — derived from each slug it owns, in the configured
  shape (`<name>-<slug>.<base>` hyphen, `<name>.<slug>.<base>` subdomain).
- **retained alias slugs** — previous slugs after a rename. **Owned forever**
  (never re-bound to another owner); **routed for a grace window, then tombstoned**
  (owned-but-dark) — §5.
- **verified custom domains** — the customer's own domain, §8.

Exactly one host is `primary`. **One resolver renders it; one resolver routes the
set.** Slug rename (§5), shape migration (§6), and custom domains (§8) then become
the *same operation*: add a host to the set, optionally flip primary, and **never
*release* the old one** — ownership is forever (anti-takeover), while *routing* an
alias lasts a grace window then tombstones (§5). Distinguishing "owned forever"
from "routed forever" is what keeps the set bounded.

## 2. What exists today

- `claimed_slug` (PK = slug, `organizationId`, `claimedByUserId`) — the
  forever-ledger. Already a **many-slugs → one-org** relation: nothing stops an
  org owning several slugs.
- `organization.slug` (unique) — the current / primary slug.
- URLs are **derived, not stored** — `src/lib/tunnel-url.ts` renders
  `(name, slug, shape, base)`.
- `tunnel` — usage registry of hosts *seen* (not a reservation); populated only
  when the edge reports per-host (not yet).
- Routing / auth resolvers (`scopesForUser`, `tunnelScopesForUser`) return **only**
  `organization.slug`. ← the gap.
- No `custom_domain` table; no alias routing.

**Key realization:** the only thing missing for slug aliases is *reading more than
`organization.slug`*. The data model already holds multiple owned slugs per org.
→ **slug rename needs no new table.**

## 3. Scopes answer on a set, not a single slug [CP]

Add one helper and widen the edge-facing resolver; leave the dashboard resolver
alone.

```ts
// All slugs an org currently answers on: primary (organization.slug) first,
// then retained aliases (claimed_slug rows still bound to this org).
scopeSlugs(orgId: string): Promise<string[]>
```

- **`scopesForUser` (dashboard)** — UNCHANGED. One entry per membership, primary
  slug only. The workspace switcher must not show aliases as separate workspaces.
- **`tunnelScopesForUser` / the verify-token scope set (edge auth)** — expand each
  membership to one `{slug, role}` per `scopeSlugs(org)`, so a tunnel naming
  *either* the new or an old slug authorizes. (MVP: one slug each → identical to
  today.)

## 4. The edge contract: `scope-hostnames` + `resolve-host` [CP][EDGE]

Two cheap, cached, shared-secret lookups (like verify-token / usage).

**`scope-hostnames`** — the single "what does this scope answer on" lookup,
fetched on a scope's first tunnel open (cache ~5 min):

```
GET /api/internal/scope-hostnames?slug=acmeinc
→ {
    primarySlug: "acmeinc",
    slugs:       ["acmeinc", "acme"],    // primary + retained aliases (§5)
    shape:       "hyphen",
    domains:     [ { domain: "acme.com", primary: true, certMode: "delegated" } ],
    primaryHost: "acme.com"              // what to render / land on by default
  }
```

On open, the edge registers every tunnel under **all** its hosts: `<name>-<slug>`
for each slug in `slugs`, plus `<name>.<domain>` for each wildcard domain (and any
single-host binding, §8). Inbound Host matching any → the open tunnel. This single
mechanism is what makes **old URLs keep working after a rename** (the old slug is
in `slugs`) *and* serves custom domains.

**`resolve-host`** — backs the certmagic On-Demand authorization (§8) and inbound
Host→scope routing for *cold* custom hosts (cache ~60s):

```
GET /api/internal/resolve-host?host=app.acme.com
→ { slug: "acme", certMode: "on_demand", tunnel: "web" }   |   404 (refuse)
```

- For a wildcard domain (`*.acme.com`, path A) the label *is* the tunnel name, so
  `tunnel` is omitted. For a single bound host (path B) `tunnel` names the bound
  tunnel.
- Only **verified** hosts resolve; everything else 404s, so On-Demand can't be
  abused to mint certs for arbitrary hosts.

(MVP with no rename / no domains: `slugs = [primarySlug]`, `domains = []`, no
custom hosts → behaves exactly like today.) Add both to the OpenAPI contract
(`src/server/public-api/contracts.ts`) so `internal/beamdapi` regenerates the Go
types.

## 5. Slug rename [CP]

Allowed, with one hard invariant: **a renamed-away slug is never released.**

Mutation `organizations.rename(orgId, newSlug)`:

1. `slugStatus(newSlug, ownerUserId)` must be available; owner-only.
2. `provisionDNS(newSlug)` (no-op unless `subdomain` shape).
3. Insert `claimed_slug[newSlug]` bound to the org; **keep `claimed_slug[oldSlug]`
   bound** (the alias — routed until tombstoned; see lifecycle below).
4. Set `organization.slug = newSlug`.

Result: `scopeSlugs(org) = [newSlug, oldSlug]`; the edge registers tunnels under
both; verify-token authorizes both; usage under either slug resolves to the org via the
shared **alias-aware `claimed_slug → org`** resolver (the request-events
`/api/internal/requests` handler attributes **slug-first** —
[`request-events-spec.md`](request-events-spec.md) §5.2,
[`implementation-plan.md`](implementation-plan.md) "shared seam").

- **Alias lifecycle — ownership is permanent, routing is not.** On rename the old
  slug stays bound (`claimed_slug[oldSlug].organizationId = org`) and routes as an
  alias — serve-identical, or **301 → primary**. After a **grace window** (default
  90 days) **tombstone** it: null `organizationId` (keep `claimedByUserId`) so the
  slug leaves the routed set — the old URL stops resolving (**404 by default**, since
  the edge simply has no route; serve **410 Gone** only if `resolve-host` returns a
  `tombstoned` marker so the edge can tell "retired" from "never existed", §13) —
  while the slug stays unclaimable by others and reclaimable by the owner. That's the
  same null-binding state a deleted org leaves, and it bounds `scopeSlugs` + the
  verify-token set.
- **Claim safety (self-guarding upsert):** the `claimed_slug` insert must assert
  reclaim rights in its *own* conflict clause — `onConflictDoUpdate(… where
  claimedByUserId = ownerUserId OR organizationId IS NULL)`, 0 rows → `SlugTakenError`.
  Today only the `organization.slug` unique insert + the `slugStatus` check gate
  `createWorkspace`; make the upsert *locally* safe so a future reorder can't open a
  TOCTOU takeover. (Flagged at the call site in `provisioning.ts`.)
- **Personal usernames** stay immutable for now (identity-grade; the onboarding
  "email support to change" note). Same mechanism if/when enabled.
- **Security:** never-release = no app-layer subdomain takeover — an attacker
  can't claim a freed `acme` and inherit its stale links/webhooks. The ledger
  already guarantees ownership; this spec adds *routing* for bound aliases and
  forbids unbinding on rename.

## 6. Shape migration (hyphen → subdomain) — kept as a future option [EDGE]

We ship **hyphen only** and **do not build** dual-serve migration now. But
subdomain stays an open future, so nothing here forecloses it. **Migration-safety
invariants — keep these true:**

- Render stays shape-parametric — `buildTunnelHost(name, slug, shape, base)`, never
  hardcode the hyphen form.
- The hostname-set model (§1) already expresses a future shape as "more hosts in
  the set" — adding subdomain hosts is additive, not a rewrite.
- `NEXT_PUBLIC_URL_SHAPE` stays wired end-to-end (CP render + edge), and the edge
  agrees on the active shape (it routes by Host + issues certs).
- Any slug rule we adopt is a **subset** a future subdomain shape can *relax*, never
  the reverse — the §7 hyphen-free-slug rule qualifies (subdomain could later allow
  hyphenated slugs; existing hyphen-free ones keep working).
- Org-last (§7) is chosen so the eventual migration is **order-preserving** — see §7.

**When we do it (later) it's a dual-serve, not a flip** — live URLs aren't
interchangeable (`api-acme.beamd.sh` ≠ `api.acme.beamd.sh`):

1. Issue per-slug `*.<slug>.<base>` certs (subdomain) while keeping the shared
   `*.<base>` (hyphen).
2. Construct + route **both** shapes for a scope's slugs.
3. Render the new shape as `primaryHost`; keep routing the old shape indefinitely.

**Likely never needed:** custom domains (§8) own "prettier / owned URL," so
hyphen-default + custom domains may make subdomain a door we keep but never walk.

## 7. Hyphen-shape rule — org-last, hyphen-free slug (locked) [CP][EDGE]

`<name>-<slug>` is **not collision-free** if *both* sides may contain hyphens
(RFC 1123 allows them): `(name="app-web", slug="acme")` and
`(name="app", slug="web-acme")` both render `app-web-acme.<base>` — a cross-tenant
host collision. The fix is to make the `(name, slug) → host` map **injective** by
keeping exactly one side hyphen-free.

**Decision (locked): forbid hyphens in the org slug; keep them in tunnel names;
render org-last `<name>-<slug>`** (e.g. `pr-123-api-acme.beamd.sh` — name
`pr-123-api`, slug `acme`). A hyphen-free slug makes the **last** hyphen the
unambiguous name/slug boundary → injective, collision-free, and tunnel names stay
expressive (`pr-123-api`, `feat-x-web` — the per-worktree / per-branch workflow
this product is for).

- **Why constrain the slug, not the name:** names are where richness lives
  (compound, ephemeral); the org slug is a short, stable token. Cost: org slugs
  can't contain hyphens (`acme-inc` → `acmeinc`). Anyone who wants a hyphenated or
  branded host upgrades to a **custom domain** (§8), which lifts the constraint
  entirely (`pr-123.acme.com`).
- **Why org-last (not org-first):** both orders are equally collision-safe, but
  org-last matches the subdomain shape's structure (`<name>.<slug>.<base>`, where
  the per-org wildcard `*.<slug>.<base>` *forces* the slug to be the second label).
  So a future hyphen→subdomain migration (§6) is just "promote the last hyphen to a
  dot" — order-preserving. It's also the industry convention for this flatten
  (Vercel `proj-hash-scope`, cited in `hosted-mode.md` §"URL options"). The render
  order is already `<name>-<slug>` — no change.
- **Timing:** no users today, so no migration risk — but lock the slug rule
  **before the first hosted hyphen URL is served** (you can't retroactively forbid
  hyphens without breaking issued URLs).

**Implementation:** split slug validation from name validation (they currently
share one RFC-1123 regex). Slug: `^[a-z0-9]{1,63}$` (no hyphens) in
`provisioning.ts` (`SLUG_RE`) **and** the edge (`internal/naming`); also update
`deriveSlug` / `ensureUniqueSlug` to emit hyphen-free collision suffixes
(`acme7f`, not `acme-7f`). Name: keep RFC 1123 (hyphens allowed). To recover
`(name, slug)` from a host, split on the **last** hyphen — though injectivity (not
parsing) is what guarantees no cross-tenant collision.

**One format, two renderers — guard the drift.** The host string is built in *two*
places: `tunnel-url.ts` (web, for display) and the edge `internal/naming` (Go, for
routing + certs). They already disagree — `naming.go:Hostname` only builds
subdomain/flat (`<name>.<slug>.<base>`), so the hyphen shape is **net-new edge
code** that must match the TS render exactly (org-last, hyphen-free). Pin them with
**shared golden vectors** (a checked-in `(name, slug, shape) → host` table both
repos test), or make `scope-hostnames.primaryHost` the single authority the web
*displays* and the edge *constructs* from one rule. Silent drift = a dashboard URL
that doesn't route.

## 8. Custom domains [CP][EDGE]

A paid scope brings its own domain so tunnels serve on it (`api.acme.com`) instead
of the shared base. ~1–2 days of edge work — certmagic already does the hard part.

### 8.1 How it sits on the model

A custom domain is a **per-scope override of the URL shape** — nothing about the
slug/ownership model changes; it's just another (prettier, customer-owned) entry
in the scope's hostname set (§1).

- A scope can attach **many** domains (capped by plan); a **domain belongs to
  exactly one scope** (globally unique) — same anti-takeover guarantee as
  `claimed_slug`.
- A verified **wildcard** (`*.acme.com`) maps to a scope, so `<name>.acme.com` →
  tunnel `<name>`. A verified **single host** (`app.acme.com` or apex `acme.com`)
  binds to **one** tunnel (`host_binding`, §10). A tunnel is reachable on all of
  its scope's verified hosts **plus** the default base host.
- One domain per scope is flagged **primary** → `primaryHost` (§4); fallback is
  the default base host.
- **Precedence — specific > wildcard.** If a host is covered by both a wildcard
  `*.acme.com` and a single `host_binding` (`app.acme.com → web`), the **binding
  wins**. A host already under a verified *delegated* wildcard needs no On-Demand
  issuance — `resolve-host` returns `certMode: delegated` for it; only a standalone
  bound host is `on_demand`.

### 8.2 Cert strategy — two paths, B-first

Our base domains use ACME **DNS-01 wildcards** (`internal/certs/magic.go`:
`*.<slug>.<base>`), which works because we control `beamd.sh`'s DNS. We do **not**
control a customer's DNS, so:

| Path | How | Customer adds | Result |
|---|---|---|---|
| **B. On-Demand per-host** *(ship first — the 90%)* | certmagic **On-Demand TLS** + a `DecisionFunc` calling `resolve-host` (§4). Per-host certs via **HTTP-01 / TLS-ALPN-01**, which only need the host to *resolve to the edge*. | routing record + ownership TXT | **One cert per hostname**, lazily issued, under LE per-cert limits (fine — premium/low-volume). Covers single host / apex. |
| **A. Delegated wildcard** *(upgrade — many subdomains)* | Customer delegates the ACME challenge: `_acme-challenge.acme.com CNAME _acme-challenge.<token>.beamd.sh`. The edge solves DNS-01 by writing the TXT in **our** zone; LE follows the CNAME. | routing + ownership TXT + **one** `_acme-challenge` CNAME | **One** `*.acme.com` cert, auto-renews. The only path for wildcards. |

**Why B first:** least customer DNS, no delegation concept to explain, matches
Vercel / Netlify / Render, and the single-host case (`app.acme.com`) is the common
one. A is strictly needed only for `*.acme.com` (wildcards **require** DNS-01 — no
HTTP-01/TLS-ALPN-01 wildcard exists). *(Reverses an earlier "A first" draft: A
reuses our DNS-01 machinery, but the delegation CNAME is more to explain and the
payoff — many subdomains under one cert — is the advanced case.)*

### 8.3 DNS the customer adds

**Path B (on-demand single host):**
```
app.acme.com              CNAME  edge.beamd.sh                       # routing (apex → A/ALIAS)
_beamd-verify.acme.com    TXT    beamd-domain-verify=<verifyToken>   # ownership (§8.4)
```

**Path A (delegated wildcard)** adds the ACME delegation:
```
*.acme.com                CNAME  edge.beamd.sh
_acme-challenge.acme.com  CNAME  _acme-challenge.<token>.beamd.sh
_beamd-verify.acme.com    TXT    beamd-domain-verify=<verifyToken>
```
*(Apex `acme.com` can't CNAME — use an A/ALIAS record to the edge IP for routing.)*
`edge.beamd.sh` is the single stable routing target customers point at,
independent of which droplet serves the scope.

### 8.4 Ownership verification [CP]

Before a domain is active, prove the org controls it:

1. On "add domain", generate a `verifyToken`; show the `_beamd-verify.<domain>` TXT
   to add.
2. Verify = DNS-lookup that TXT and match. Allow re-checks; expire pending domains
   after N days.
3. Only `verified` domains are returned by `scope-hostnames` / `resolve-host` (§4)
   — until then the edge refuses to route or issue for the host.
4. **Re-verify on a schedule (daily) and before every cert issue/renew; fail
   closed.** A verified domain can lapse — the customer drops it, someone else buys
   it and points it at the edge. On a failed re-check, immediately stop returning
   the host so no stale cert is minted and no new owner is routed to the old scope.
   The re-verify cadence *is* the dangling-domain security parameter.

## 9. Tracking full tunnel URLs / backfill (note)

A tunnel URL is a **pure function** of `(name, slug, shape, base)`, so the *format*
is always backfillable by running `buildTunnelHost` over known `(name, slug)`
pairs — and `tunnel.host` (unique-indexed) already exists to hold it. What is
**not** recoverable is tunnel *names never captured*: names are ephemeral
(`beamd open --as api`). Per-host capture is **realized by the per-request pipeline**
([`request-events-spec.md`](request-events-spec.md)): every `request_event` carries
`host`, and the `tunnel` registry is upserted from it — this supersedes the old
`usage_event` delta prep. **Caveat:** deriving-on-read is **lossy
across rename / shape changes** (you'd re-derive *today's* URL, not the one served
then) — so for audit / billing, store the literal host when seen (which
`tunnel.host` / `usage_event.host` do).

## 10. Schema delta [CP]

| Change | Why |
|---|---|
| **none** for slug aliases | reuse `claimed_slug` (bound rows = the scope's slug set, §5) |
| **new `custom_domain`** | §8 |
| **new `host_binding`** (single host → tunnel) | path B / single-host needs it (the label isn't the tunnel name) |
| *(optional)* `claimed_slug.alias_redirect boolean` | serve-identical vs 301-to-primary, if §5 alias policy wants it |

```ts
export const customDomain = pgTable("custom_domain", {
  id:             pk(),                                  // uuidv7 (DEFAULT uuidv7(), PG18)
  organizationId: uuid().notNull().references(() => organization.id, { onDelete: "cascade" }),
  domain:         text().notNull().unique(),            // "acme.com" — globally unique (one owner)
  status:         text().notNull().default("pending"),  // pending | verified | failed
  certMode:       text().notNull().default("on_demand"),// on_demand | delegated
  isPrimary:      boolean().notNull().default(false),   // the render default for the scope
  verifyToken:    text().notNull(),                     // _beamd-verify TXT value
  verifiedAt:     timestamp({ withTimezone: true, mode: "date" }),
  createdAt:      createdAt(),
}, (t) => [
  uniqueIndex("custom_domain_domain_idx").on(t.domain),
  index("custom_domain_org_idx").on(t.organizationId),
])

// Single host (`app.acme.com`/apex) → one tunnel. Wildcard domains don't need a
// row (the label is the tunnel name); this is only for the bound-host case.
export const hostBinding = pgTable("host_binding", {
  id:             pk(),
  organizationId: uuid().notNull().references(() => organization.id, { onDelete: "cascade" }),
  host:           text().notNull().unique(),            // "app.acme.com"
  tunnelName:     text().notNull(),                     // "web"
  createdAt:      createdAt(),
}, (t) => [uniqueIndex("host_binding_host_idx").on(t.host)])
```

- At most one `isPrimary` per org (enforce in the mutation, or a partial unique
  index). `domain` unique = the anti-takeover guarantee, like `claimed_slug`.

## 11. Tasks

**[CP] (`beamd-web`)**

- [ ] `scopeSlugs(orgId)`; widen `tunnelScopesForUser` / verify-token set to all
      bound slugs (dashboard `scopesForUser` unchanged). §3
- [ ] `GET /api/internal/scope-hostnames` + `GET /api/internal/resolve-host`
      (shared secret); add both to the OpenAPI contract → Go regen. §4
- [ ] Extend `src/lib/tunnel-url.ts`: `tunnelHost(name, slug, primaryDomain?)`,
      `primaryHostFor(scope)`; feed primary into `organizations.get` + REST
      `exampleUrl`. §1, §4
- [ ] `organizations.rename` mutation (never-release) + dashboard UI; usage handler
      `claimed_slug` fallback. §5
- [ ] **Slug rule** (§7): split slug vs name validation — `SLUG_RE` →
      `^[a-z0-9]{1,63}$` (no hyphens); update `deriveSlug` / `ensureUniqueSlug` to
      emit hyphen-free collision suffixes. Names keep RFC 1123.
- [ ] **Custom domains**: `custom_domain` + `host_binding` tables; tRPC `domains`
      router (`add` → plan-cap + `verifyToken` + DNS records to add; `list`,
      `verify`, `setPrimary`, `remove`); Domains tab UI (records w/ copy, Verify
      button, status, set-primary, remove); plan-cap policy from config;
      lifecycle (expire unverified, periodic re-verify). §8
- [ ] Tests: rename keeps old slug routable + unclaimable by others;
      `scope-hostnames` / `resolve-host` shapes; alias usage resolves to org;
      domain uniqueness/anti-takeover (2nd org can't add a verified domain);
      verification flow; plan cap; §7 slug rule (hyphenated slug rejected,
      auto-slug emits no hyphens).

**[EDGE] (`beamd`)**

- [ ] Fetch `scope-hostnames` on first open; register tunnels under all slug-hosts
      + domain-hosts + single-host bindings; cache. §4
- [ ] Hostname construction in `internal/naming` (one shape-parametric function,
      org-last `<name>-<slug>` — net-new; `naming.go` only does subdomain/flat
      today); split name vs slug validation per §7; match `tunnel-url.ts` via the
      shared golden vectors (§7 drift guard).
- [ ] **Cert path B**: certmagic On-Demand TLS + `DecisionFunc` → `resolve-host`
      (allow only verified hosts). Inbound Host→scope via the open-tunnel map /
      `resolve-host`. §8.2
- [ ] **Cert path A**: manage `*.<domain>` via DNS-01, writing the TXT to the
      delegation target in *our* zone (`_acme-challenge.<token>.beamd.sh`). Verify
      certmagic/libdns CNAME-delegation against LE-staging. §8.2
- [ ] `beamd.yaml`: delegation-zone base, `scope-hostnames`/`resolve-host` URLs +
      secret.

**[CLI] (`beamd`)**

- [ ] Print the edge-assigned `primaryHost` (incl. the custom-domain URL); after a
      rename, refresh cached scopes at next login / `beamd orgs`.
- [ ] *(optional)* `--domain <d>` on `open`/`run` + `.beamd` to pick which of the
      scope's hosts to land on (default: primary).

**Hardening & UX (tracked — fold in as phases land)**

- [ ] **R2** Shared golden vectors `(name, slug, shape) → host` tested in *both*
      repos (web `tunnel-url.ts` + edge `internal/naming`) — the only drift guard on
      the host format.
- [ ] **R3** Hyphen-free slug touches every `SLUG_RE` copy — `provisioning.ts`,
      `onboarding.tsx` (client mirror), edge `internal/naming` — plus `deriveSlug` /
      `ensureUniqueSlug` and the email-derived default username. Keep in sync
      (client/server divergence bit onboarding once).
- [ ] **R4** Validate combined `<name>-<slug>` ≤ 63 chars — at name-time in CP/CLI
      (UX) **and on the edge at `hello_ok`** (the backstop: it's the routing/cert
      authority and only learns the slug at registration, so reject
      `len(name)+1+len(slug) > 63` there or cert issuance fails opaquely). Actionable
      message: "name too long for this workspace URL — shorten it or use a custom
      domain".
- [ ] **R5** `scope-hostnames` / usage / verify resolve the org by **any bound
      slug** (`claimed_slug → org`), never just `organization.slug`.
- [ ] **R6** Edge re-fetches `scope-hostnames` on TTL and **re-registers open
      tunnels** under the new set, so a rename reaches already-open tunnels.
- [ ] **S3** Per-edge internal secrets (or mTLS) as droplets shard — contain a
      compromised edge's blast radius (verify-token / scope-hostnames / resolve-host).
- [ ] **S5** Deleted **team** slugs: reclaim is admin/support-gated, not the single
      `claimedByUserId` (an ex-member must not self-reclaim a team namespace).
- [ ] **S6** Assert dashboard origin ≠ tunnel base at config load (cookie isolation,
      independent of PSL).
- [ ] **isPrimary** one-per-org via a **partial unique index** (DB-enforced), not
      app-only.
- [ ] **U1** Live-slugify the onboarding / team-create input (strip hyphens as
      typed, show a URL preview) — transform, don't reject.
- [ ] **U2** Dashboard "URLs" panel: primary (+ toggle), alias hosts (with expiry),
      custom domains (with status).
- [ ] **U3** Custom-domain wizard: auto-poll Verify, set DNS-propagation
      expectations, **pre-warm the cert on verify** (the first visitor isn't
      lazy-issued).
- [ ] **U4** Cache-bust / short TTL after verify & rename so "I just did X" is
      immediate.

## 12. Phased path

1. **Resolver foundation** [CP] — `scopeSlugs` + widen the edge-facing set +
   `tunnelHost(primaryDomain?)` + `scope-hostnames`/`resolve-host` endpoints +
   lock the §7 slug rule (hyphen-free slug, org-last) in `provisioning` + naming.
   No schema; unblocks everything. (MVP behaves exactly like today.)
2. **Slug rename** [CP] — mutation (never-release) + dashboard. Old URLs alias via
   the foundation; verify end-to-end without the edge.
3. **Custom domains — path B** [CP][EDGE] — tables + `domains` router + Domains tab
   (verifiable end-to-end before the edge: a customer can add + verify), then edge
   On-Demand per-host gated by `resolve-host`.
4. **Custom domains — path A** [EDGE] — delegated-wildcard DNS-01 for `*.acme.com`,
   validated against LE-staging.
5. **Shape migration** [EDGE] — dual-serve, only if/when actually needed (§6).
   **Polish** — alias 301 policy, CLI `--domain`, per-host metrics (reuses the
   per-tunnel `host` usage field already in the contract).

## 13. Decisions

**Locked** (see body):

- Hyphen shape, **org-last** render, **hyphen-free org slug** (§7).
- Subdomain kept as a **future option, not built now** (§6); migration-safety
  invariants hold.
- Custom domains **B (on-demand single-host) first**, A (delegated wildcard) for
  `*.acme.com` (§8.2).
- **Never-release** on rename/delete (§5).

**Still open:**

- §5 alias policy: serve-identical vs 301-to-primary; grace-window length; and the
  post-tombstone response — **404** (zero-effort; the host is just gone from the set)
  vs **410 Gone** (needs `resolve-host` to return a `tombstoned` marker so the edge
  can distinguish retired from never-existed). Don't over-build it.
- Personal-username rename: enable or keep immutable.
- Plan caps: how many custom domains per tier.
- Apex support priority (`acme.com` with no subdomain — needs A/ALIAS + a
  single-host binding; v1 could be subdomain-only).

## 14. Security recap

- **Never-release** (§5) — no app-layer subdomain takeover on rename / delete;
  routing only for *bound* `claimed_slug` rows.
- **Hyphen collision** (§7) — **resolved**: a hyphen-free org slug makes
  `(name, slug) → host` injective, so no two tenants can collide on a host.
  Org-last does put the *attacker-chosen* name as the leading label (the owned slug
  doesn't lead) — a minor brand-impersonation surface on `*.beamd.sh`, acceptable
  for dev/preview hosts (low brand-trust); custom domains (§8) are the answer for
  brand-trusted URLs.
- **Cookie isolation + PSL lead time** — tunnels on a *different registrable
  domain* than the dashboard (and free ≠ paid), and submit tunnel base(s) to the
  **Public Suffix List** so sibling tunnels can't cookie-poison each other
  (load-bearing under any shared-base shape — hyphen/flat/subdomain). **Submit
  `beamd.sh` + the free domain to the PSL now, out of band** — additions take
  weeks-to-months to propagate into shipped browser releases, so the lead time, not
  the code, is the bottleneck; until it lands, the hyphen shape isn't safe for
  auth-cookie traffic. See `hosted-mode.md` §"Domains & PSL".
- **Custom domains** (§8) — globally-unique `domain` + ownership TXT before active
  (no cross-org takeover, no attaching domains you don't own); `resolve-host`
  allowlist gates On-Demand issuance (can't mint certs for arbitrary hosts);
  **daily re-verify + fail-closed + re-verify before every issue/renew** so a lapsed
  domain can't mint a stale cert or route a new owner to the old scope (§8.4); keep
  custom domains on the **paid edge** (shared reputation — see `hosted-mode.md` edge
  split).
