# Implementation plan — URL model + request events

> **The single driver** for building two related-but-distinct subsystems. The design
> lives in two cohesive specs; this is the build order, the shared seam, and the
> cross-spec rules so neither side reinvents the other.
>
> - [`url-model.md`](url-model.md) — identity / routing / certs (slug, rename, shape,
>   custom domains, the hostname set).
> - [`request-events-spec.md`](request-events-spec.md) — telemetry / billing (per-request
>   event log replacing the delta usage pipeline).
>
> They are **siblings, not parent/child** — kept as two specs on purpose (different
> cohesion). They meet at exactly one seam.

## The shared seam — build it once

Both subsystems must turn an inbound request into a scope. Define **one** resolver and
use it everywhere:

```
orgBySlug(slug) → org   — ALIAS-AWARE via claimed_slug (NOT organization.slug). Cheap:
                          one indexed join. This IS url-model R5.
orgByHost(host) → org   — exact; custom_domain / host_binding / default-shape parse.
```

Same two building blocks, **used in opposite orders** by the two subsystems:

- **Event attribution (request-events ingest, §5.2):** **slug-first.** The session slug is
  present on every routed request (custom domains included), so `orgBySlug` resolves the
  billable 99% with no host parse on the hot path. `host` is kept for *identity* only;
  `orgByHost` is a best-effort enrichment for `no_route` (no slug) — off the billing path.
- **Inbound routing / certs (url-model edge, §4):** **host-first** — the edge only has the
  Host at that point (`scope-hostnames` / `resolve-host`).
- The ALIAS-AWARE `orgBySlug` is the shared piece both must use; land it **first** —
  everything depends on it.

## Build order

Each phase is independently shippable; later phases degrade to today's behavior if a
prior one isn't in yet.

1. **Shared resolver + url-model §7 slug rule** [CP]
   - `orgBySlug` (alias-aware) + `orgByHost` + `scopeSlugs` (resolveScope =
     slug-if-provided-else-host).
   - §7: split slug vs name validation (`SLUG_RE → ^[a-z0-9]{1,63}$`, hyphen-free),
     de-hyphen `deriveSlug`/`ensureUniqueSlug`, client mirror in `onboarding.tsx`.
   - No schema. MVP behaves like today. *(url-model Phase 1.)*

2. **url-model foundation** [CP] — `tunnelHost(primaryDomain?)`, `primaryHostFor`,
   `scope-hostnames` + `resolve-host` endpoints → OpenAPI → `beamdapi` regen.

3. **Request-events pipeline** [CP→EDGE] — *(request-events §7 rollout.)*
   - CP: `request_event` (single-col **edge-minted uuidv7 `id` PK**; partition deferred, by
     `id` when needed) + `usage_daily`, `/api/internal/requests` (per-edge secret, edge-attribution,
     **slug-first** attribution / host for identity), rollup job. Deploy — endpoint live,
     nothing sends yet.
   - EDGE: `internal/reqlog` (file sink always-on; shipper hosted-only), edge
     instrumentation (truncated IP, path redaction, WS/SSE heartbeats, outcome).
   - Repoint tRPC usage router to the new tables; verify parity; **then** delete the
     delta `usage_event` + `/api/internal/usage`.

4. **Slug rename** [CP] — url-model §5 (never-release, alias lifecycle, self-guarding
   upsert). Old URLs alias via the foundation.

5. **Custom domains** [CP→EDGE] — url-model §8: path B (on-demand single-host) first,
   then path A (delegated wildcard).

6. **Polish / hardening** — both specs' backlogs (url-model §11 R/S/U list;
   request-events live inspector, billing backstop, partition cron).

## Cross-spec rules (don't trip over each other)

- **One OpenAPI regen at a time.** url-model adds `scope-hostnames` + `resolve-host`;
  request-events adds `ingestRequests`. Both edit `contracts.ts` → regen `beamdapi`.
  **Sequence the contract commits** (land + regen one, then the next) — do not run two
  regens in flight or you fight the generated-file diff. Order: phase 2's endpoints,
  then phase 3's.
- **`tunnel` registry has one writer of record** — the request-events `/requests` route
  upserts it (`firstSeenAt`/`lastSeenAt`). url-model only *reads* it. Don't add a second
  upsert path.
- **Per-host capture is request-events, not `usage_event`.** url-model §9's "per-host
  capture" is realized here; the old `usage_event.host` prep is superseded.
- **The §5 "usage handler claimed_slug fallback"** (url-model) lands in the `/requests`
  route's `resolveScope`, not the deleted usage route.

## Security crosscheck (both specs)

- **Per-edge internal secrets / mTLS.** Both add shared-secret edge↔CP endpoints; as
  droplets shard, per-edge secrets contain a compromised droplet (url-model S3). Extra
  weight on `/requests`: it's **writable + billing-affecting**, so also edge-attribution
  + batch sanity-bounds (request-events §9).
- **PSL submission** is the long pole and out-of-band — keep it moving in parallel
  (url-model §14). Independent of all code above.
- **Sensitive stores, org-scoped reads.** `request_event` (visitor paths/IPs/UAs) and the
  hostname tables are org-scoped; edge-side IP truncation + capture config minimize
  breach blast radius.
