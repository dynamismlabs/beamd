# Launch readiness — URL model + request events

> Status + the short path to production for the work in [`url-model.md`](url-model.md)
> and [`request-events-spec.md`](request-events-spec.md) (build order in
> [`implementation-plan.md`](implementation-plan.md)). **This doc is the single
> tracker for what's left** — the spec task lists (url-model §11, request-events
> §10) hold the granular items; this groups them by *whether/when they're worth
> doing*.

## Built + verified

The core is implemented and green across both repos (beamd-web: typecheck + lint +
**87 vitest tests** + build; beamd Go: build + vet + all packages incl. e2e + `-race`
on the hot paths):

- §7 hyphen-free slugs (org-last, injective) + the shared `orgBySlug`/`orgByHost`
  resolver; `tunnelHost(primaryDomain?)` / `primaryHostFor`; `scope-hostnames` +
  `resolve-host` endpoints + the OpenAPI/`beamdapi` contract.
- Slug **rename** — never-release, routed aliases, tombstone, self-guarding claim,
  `isPersonal` lock; dashboard rename + URLs panel.
- **Request-events pipeline end-to-end** — edge instrumentation (TTFB, bytes_in,
  outcome, edge-truncated IP, WS heartbeats) → file sink → shipper → `/api/internal/requests`
  → `request_event` + `usage_daily` rollup → the live inspector. Old delta pipeline
  removed.
- **Wildcard custom domains** — add/verify (anti-takeover, plan cap, fail-closed
  re-verify) → routed on the edge (multi-host registration) → On-Demand TLS gated
  by the `resolve-host` allowlist.

### Review hardening (2026-06-04, two independent passes)

A self-review (4 parallel reviewers) plus an independent Codex pass found and **fixed**
a set of correctness/security bugs — all landed, all gates re-run green:

- **Cert issuer was DNS-01-only** (would have broken every custom-domain cert): the
  single ACME issuer set `DNS01Solver`, which makes certmagic use DNS-01 *exclusively* —
  so the On-Demand path could never solve TLS-ALPN-01 for a domain whose DNS we don't
  control. Fixed: a second TLS-ALPN-01 issuer for on-demand, ordered after the DNS-01
  issuer (wildcards keep DNS-01; custom domains fall through to TLS-ALPN). Now
  covered by an automated Pebble integration test (`make test-acme`) that proves the
  failover issues a real cert via TLS-ALPN-01; see §3.
- **Production migration was stale** — `drizzle/0000_*.sql` still created the dropped
  `usage_event` and lacked `request_event`/`usage_daily`/`custom_domain`/`host_binding`/
  `aliased_at` (tests pass on `drizzle-kit push`, so it was invisible). Regenerated from
  schema + re-added the `set_updated_at()` trigger; **verified by applying to a throwaway
  PG18 DB** (16 tables, 15 triggers, `uuidv7()` defaults, trigger fires).
- **`?next=` open redirect / XSS** — auth pages pushed `router.query.next` unvalidated;
  added `safeNextPath()` (same-origin paths only) in login/signup/onboarding.
- **`createWorkspace` non-atomic** — org+member+slug-claim+droplet now wrapped in one
  `db.transaction` (a claim race can't orphan an org).
- **`requireMembership` fail-open** on an unrecognized role → now fail-closed.
- **Edge:** `ip_mode: off` shipped the *raw* IP (now drops it); `activeTunnels` gauge
  drift + spurious `over_limit` on a subset-reconnect (dead-tunnel reclaim moved ahead of
  the cap check); file-sink open made synchronous (bad path is now fatal at startup, not a
  silent drop) with write failures counted at `/metrics`; SIGTERM now drains the edge
  *before* flushing the request log.
- **Reads:** usage day-buckets pinned to UTC (match the rollup); `usage_daily` rollup
  grouped by `(org, host)` so two slugs on one host can't undercount.

## Launch checklist (the only things between here and production)

### 1. PSL submission — START NOW (long lead time, out-of-band)

Cookie isolation under the shared-base shape depends on the tunnel base being a
**public suffix**, so `x.beamd.run` and `app.beamd.run` become separate cookie jars.
PSL additions take **weeks-to-months** to ship in browser releases — the lead
time, not the work, is the bottleneck. File the PR today.

- **Where:** a PR to <https://github.com/publicsuffix/list> editing
  `public_suffix_list.dat`, in the **`// ===BEGIN PRIVATE DOMAINS===`** section.
- **Entry** (alphabetical within the section):
  ```
  // Beamd : https://beamd.run
  // Submitted by <name> <ops@beamd.ai>
  beamd.run
  ```
  Add the free-tier domain too if/when bought (e.g. `beamd-free.sh`).
- Until it lands in shipped browsers, the hyphen shape is **not** cookie-safe for
  auth traffic. Track the PR; flip nothing in code when it merges (the S6 guard
  below is the only code touchpoint).

### 2. Dashboard ≠ tunnel base (cookie isolation) — code guard DONE

The dashboard must be on a **different registrable domain** than `*.<tunnelBase>`,
so an untrusted tunneled app can't set/read the dashboard's session cookie.

- **Code (done):** `beamd-web/src/lib/env.ts` `assertCookieIsolation()` refuses to
  boot in production if `NEXT_PUBLIC_APP_URL`'s host is the tunnel base or nested
  under it. Once `beamd.run` is a confirmed public suffix (step 1), a shared parent
  like `app.beamd.run` is safe — set `COOKIE_ISOLATION_OK=true` to acknowledge.
- **Deployment (yours):** pick the domains. Either dashboard on a separate
  registrable domain (`app.beamd-app.com`), or rely on the PSL + the env opt-out.
  Also keep free vs paid tunnels on different registrable domains (reputation).

### 3. LE-staging validation of custom-domain certs — hands-on (~hours)

The On-Demand cert path is now **validated automatically** against a real ACME
server: **`make test-acme`** runs Let's Encrypt's Pebble + a mock DNS locally and
drives the real `NewMagicManager` through an On-Demand issuance for a custom
domain — proving the DNS-01 issuer fails (no control of the customer's DNS) and
**fails over to TLS-ALPN-01**, which issues a real cert over the edge's own
listener (and that an unauthorized host is refused → self-signed fallback). This is
the regression guard for the DNS-01-only issuer bug.

What `make test-acme` can't cover is the **real CA + real networking** (public DNS,
a real domain, Let's Encrypt's multi-perspective validation), so a one-time
LE-staging smoke test is still worth doing before relying on it. Runbook:

1. Point the edge at staging: `acme_ca: https://acme-staging-v02.api.letsencrypt.org/directory`
   (or `BEAMD_ACME_CA`) in `beamd.yaml`. Set the `token_store` to the hosted
   verify-token URL (so `resolve-host` is reachable) + `BEAMD_AUTH_VERIFY_SECRET`.
2. In the dashboard, add a custom domain you control (`test.example.com`), add the
   DNS records, click **Verify** → status `verified`.
3. Point the host at the edge: `*.test.example.com CNAME edge.beamd.run` (or A → edge IP).
4. `curl -v https://api.test.example.com/` → edge logs should show
   `certs: on-demand issuance authorized` then an LE-staging cert issued via
   TLS-ALPN-01, and the request routes to your tunnel. (Browsers flag the staging
   cert as untrusted — expected.)
5. **Negative test:** hit a host NOT added in the dashboard → `resolve-host` 404 →
   `on-demand issuance refused` in the logs → self-signed fallback, no cert minted.
6. Flip `acme_ca` back to production for real certs.

If a customer needs many subdomains under one cert, that's **Path A** (delegated
wildcard) — a follow-up (see backlog).

## Deferred backlog — DON'T do until the trigger fires

These are correctly deferred (most are in the spec task lists already). Do each
when its trigger arrives, not before.

| Item | Spec | Trigger — do it when… |
|---|---|---|
| **Edge-attribution + per-edge secrets (S3)** — `/requests` rejects slugs an edge doesn't serve | req-events §9, url-model S3 | you run **>1 edge/droplet** (single edge serves all slugs — attribution is a no-op until then) |
| **R6** — edge re-registers *already-open* tunnels on scope-hostnames TTL refresh | url-model §11 R6 | renames are common **and** tunnels stay open across them (today a live tunnel reconnects; the old URL keeps working) |
| **`request_event` partitioning + ensure-cron** (`PARTITION BY RANGE (id)`) | req-events §5.3 | the table approaches **~25M rows** / pre-GA (cheap migration while small; the id-PK keeps it painless) |
| **Billing backstop** (durable per-slug counter) | req-events §7 | you're billing **and** `beam_requests_dropped_total` alerts fire (it now covers channel backpressure **and** file-sink write/reopen failures, so it's a complete loss signal) |
| **S5** — team-slug reclaim admin/support-gated (not the founding claimer) | url-model S5 | you have **multi-member teams** whose members churn |
| **host_binding** single-host custom domains (`app.acme.com`→a specific tunnel) — schema + resolver built (verified-parent gated); needs an insert path (router/UI) + edge registration | url-model §8 | a customer wants apex / single-host binding instead of a wildcard |
| **Cert Path A** — delegated wildcard (`*.acme.com`, one cert) | url-model §8.2 | a customer wants **many** subdomains under one domain (Path B per-host covers the rest) |
| **CLI** — print edge-assigned primaryHost; refresh cached scopes after rename | url-model §11 CLI | polishing the CLI (edge already returns the right URL on register) |
| **Pending-domain expiry**; **U3** wizard auto-poll + cert pre-warm; **path token-shape redaction** | url-model §11, req-events §3/§10 | GA polish — none are correctness/security holes (query strings already stripped; path is a toggle-off-able analytics field) |

## What to NOT build (gold-plating)

Path token-shape redaction and pending-domain expiry are low-value heuristics —
skip unless a concrete privacy/ops requirement appears.
