# Security hardening spec

Status: **SEC-1…SEC-7 implemented** (2026-07) · Scope: internet-facing edge
surface · Basis: focused security review of the edge, cert-issuance, proxy, and
token paths.

This spec is a remediation runbook. Each item is self-contained — location, the
defect, the fix, and how to prove it's fixed. Nothing here changes the core
protocol or breaks existing tunnels; every fix is localized. SEC-1 through SEC-7
are implemented and verified (see [Implementation log](#implementation-log));
SEC-8 is a tracked deferred item.

The **core security invariants already hold** — see
[Verified sound](#verified-sound-do-not-re-audit). The gaps below were one
client-IP-spoofing HIGH plus a cluster of availability/infoleak mediums.

---

## Findings at a glance

| ID | Sev | Area | One-line | Status |
|----|-----|------|----------|--------|
| [SEC-1](#sec-1--high--edge-trusts-inbound-forwarding-headers) | HIGH | proxy | Edge trusts inbound `X-Forwarded-For`/`Forwarded`/`X-Real-IP` → client IP spoofing | ✅ done |
| [SEC-2](#sec-2--medium--metrics-is-unauthenticated) | MED | metrics | `/metrics` is unauthenticated; Host-gate is not access control | ✅ done (Opt A) |
| [SEC-3](#sec-3--medium--on-demand-cert-path-has-no-failure-cooldown) | MED | certs | On-demand issuance re-tries every handshake → ACME account budget burn | ✅ done |
| [SEC-4](#sec-4--medium--resolve-host-decision-is-not-cached) | MED | certs | Resolve-host gate called per-handshake → unauth amplification DoS | ✅ done |
| [SEC-5](#sec-5--low--authorizer-ignores-cert_mode) | LOW | certs | Authorizer ignores `cert_mode: delegated` → issues certs for delegated domains | ✅ done (Opt A) |
| [SEC-6](#sec-6--low--inline-secret-in-world-readable-config-accepted-silently) | LOW | config | Inline `dns_provider_creds` in a world-readable config accepted silently | ✅ done |
| [SEC-7](#sec-7--nit--pin-tls-minversion) | NIT | tls | `tls.Config.MinVersion` unset (relies on toolchain default) | ✅ done |
| [SEC-8](#sec-8--deferred--no-cap-on-concurrent-unauthenticated-connections) | MED | edge | No cap on concurrent unauthenticated handshakes/sessions (raw connection flood) | ⏸ deferred |

---

## SEC-1 — HIGH — Edge trusts inbound forwarding headers

**Location:** `internal/edge/edge.go` — `proxyFor()` (the `ReverseProxy.Director`).

**Defect.** The edge is the first and only trusted hop, but the Director
*appends* the observed peer IP to a client-supplied `X-Forwarded-For` instead of
replacing it, and never touches `X-Real-IP` or the RFC-7239 `Forwarded` header.

```go
// current — trusts the client's value
if prior := req.Header.Get("X-Forwarded-For"); prior != "" {
    req.Header.Set("X-Forwarded-For", prior+", "+ip)
}
```

**Why it matters.** A public client sending
`X-Forwarded-For: 9.9.9.9` (or `X-Real-IP` / `Forwarded: for=9.9.9.9`) makes the
tunneled backend see the forged IP. Any app that reads the **leftmost** XFF value
— the near-universal "client IP" convention (Express `trust proxy`, typical
Rails/Django/nginx) — or that prefers `X-Real-IP`/`Forwarded`, trusts it. This
defeats IP allowlists, rate limits, geo-gating, and audit logging **inside every
tunneled app**. It also folds in the related medium that a client can *delete*
the edge-set headers by naming them in `Connection:` (a documented `Director`
pitfall).

**Fix.** Move from the deprecated `Director` to `Rewrite` +
`ProxyRequest.SetXForwarded()`. The `Rewrite` path strips inbound forwarding
headers before the rewrite and re-adds them from the real connection.
`SetXForwarded` covers `X-Forwarded-*` only, so also `Del` the non-standard
variants explicitly.

```go
p = &httputil.ReverseProxy{
    Rewrite: func(pr *httputil.ProxyRequest) {
        pr.SetXForwarded() // drops inbound X-Forwarded-*; sets XFF/XFH/XFP from the real conn
        // SetXForwarded doesn't touch these — a client could still forge the IP through them.
        pr.Out.Header.Del("Forwarded")
        pr.Out.Header.Del("X-Real-Ip")
        // The edge always terminates TLS; make the scheme signal unconditional.
        pr.Out.Header.Set("X-Forwarded-Proto", "https")
        pr.Out.URL.Scheme = "http"
        pr.Out.URL.Host = host
        if _, ok := pr.Out.Header["User-Agent"]; !ok {
            pr.Out.Header.Set("User-Agent", "")
        }
    },
    Transport: /* unchanged */,
}
```

Notes:
- `Rewrite` and `Director` are mutually exclusive — remove the `Director`.
- Keep the existing `PreviewEmbed` response-header stripping (it hangs off
  `ModifyResponse`/the returned proxy, not the Director — leave it as is).
- `X-Forwarded-Host` from `SetXForwarded` is `pr.In.Host`, matching prior behavior.

**Acceptance criteria.**
- Add a header-echo path to the e2e dummy backend (reflect the
  `X-Forwarded-For`, `X-Real-IP`, and `Forwarded` it received).
- New e2e test: client sends forged `X-Forwarded-For: 9.9.9.9`,
  `X-Real-IP: 9.9.9.9`, `Forwarded: for=9.9.9.9`. Assert the backend sees
  `X-Forwarded-For` == only the edge's observed loopback IP (no `9.9.9.9`),
  and `X-Real-IP` / `Forwarded` absent.
- Second assertion: a `Connection: X-Forwarded-Proto` header does **not** strip
  the edge-set `X-Forwarded-Proto` reaching the backend.

**Effort:** small (≈20 lines + one e2e test). Closes the HIGH and the
`Connection`-strip medium together.

---

## SEC-2 — MEDIUM — `/metrics` is unauthenticated

**Location:** `internal/edge/edge.go` — `handler()` (the `/metrics` case) and
`handleMetrics()`.

**Defect.** `/metrics` is gated only by `isBaseHost(r.Host)`. The Host header is
client-controlled and decoupled from the TLS SNI, and there is no auth on the
endpoint. Anyone who reaches the base domain (`curl https://<base>/metrics`) gets
every tenant's slug, every tunnel name, and per-tenant/per-tunnel byte counts.

> The earlier Host-gate fix stopped the dump from *also* appearing on tunnel
> hostnames (the original finding). It is necessary but **not** access control —
> this item adds the missing authentication.

### ✅ DECIDED: Option A (bearer token)

Implemented as Option A. The two options were:

- **Option A — bearer token (recommended).** Add `metrics_token` to the server
  config (overridable by `BEAMD_METRICS_TOKEN`). `handleMetrics` requires
  `Authorization: Bearer <token>` (constant-time compare); missing/empty config
  token → `/metrics` returns 404 (endpoint effectively disabled, no accidental
  exposure). Keeps a single :443 listener; works with a scraper that can send a
  header. Lowest operational friction.
- **Option B — separate operator listener.** Serve `/metrics` (and optionally
  `/healthz`) on a second listener bound to `127.0.0.1:<port>` (or a configurable
  operator address), never on the public :443 handler. Strongest isolation;
  requires operators to reach it via SSH tunnel / private network and is a bigger
  change to `Serve()`.

Chose **A**: single :443 listener, lowest friction, and unset-token → 404 is the
fail-closed default. `handleMetrics` now returns 404 when no token is set (with
a startup warning), 401 on a missing/wrong bearer, and serves only on a
constant-time token match.

**Fix (Option A sketch).**
```go
case "/metrics":
    if e.isBaseHost(r.Host) && e.metricsAuthOK(r) {
        e.handleMetrics(w, r)
        return
    }
    // else fall through to normal routing (tunnel hosts keep their own /metrics)
```
`metricsAuthOK` returns false when no token is configured, and otherwise does
`subtle.ConstantTimeCompare` against the bearer token.

**Acceptance criteria.**
- With a token configured: `GET /metrics` on the base domain **without** the
  header → 404 (or 401); **with** the correct bearer → 200 and contains
  `beam_active_sessions`.
- Host-spoof check: SNI = a tunnel host, `Host: <base>`, no token → not served.
- With no token configured: `/metrics` is not served on any host.
- Tunnel-host `/metrics` still routes to the tenant app (existing e2e assertion
  stays green).

**Effort:** small (A) / medium (B).

---

## SEC-3 — MEDIUM — On-demand cert path has no failure cooldown

**Location:** `internal/certs/magic.go` — the on-demand branch of
`GetCertificate()` (`m.cmOnDemand.GetCertificate(hello)`), and the
`OnDemandDecision` wiring.

**Defect.** The DNS-01 wildcard path has `kickWildCooldown`/`lastFail`; the
on-demand custom-domain path has no equivalent. A verified-but-repeatedly-failing
customer domain (DNS intermittently points at the edge, challenge never truly
served) re-attempts ACME issuance on **every handshake**. Because on-demand and
the wildcard renewals share one ACME account+email, this can exhaust the
account-wide rate budget and **starve legitimate apex/wildcard renewals**.

**Fix (implemented).** A short cooldown in the `decisionGate` from SEC-4
(`internal/certs/decisiongate.go`), which owns its state and is consulted by
both the DecisionFunc and `GetCertificate`:
1. `decisionGate.lastFail map[string]time.Time` + `gateDefaultFailCooldown` (2
   min — short on purpose; see #4).
2. `Decide()` denies a name in cooldown **before** the resolve-host call.
3. `GetCertificate` calls `gate.recordFailure(name)` on any on-demand error and
   `gate.recordSuccess(name)` on success.
4. **`recordFailure` does not EXTEND an active cooldown** — otherwise continuous
   traffic to a failing domain bumps `lastFail` every handshake and the window
   never elapses. Not-extending means the domain re-attempts once per ~2-min
   window even under load (a long window would delay a freshly-verified domain's
   first cert — the reason it's short).
5. **`recordFailure` records ONLY names the gate AUTHORIZED** (verdict `err ==
   nil`). This is load-bearing, not cosmetic: `GetCertificate` calls
   `recordFailure` on *any* on-demand error including a plain **deny** of an
   unverified name, so recording denials would let an unauthenticated peer
   spraying distinct off-base SNIs grow `lastFail` without bound — a persistent
   version of the very DoS SEC-4 closes. Only control-plane-verified domains are
   ever authorized, so keying off the verdict cache keeps `lastFail`
   operator-bounded and attacker-proof. (An earlier "blanket" version recorded
   denials too and was caught in review — cooling a denied name buys nothing
   since it never reaches an ACME order and its lookups are already collapsed by
   the 30 s negative verdict cache.) This also removes a shadowing wart: a name
   flipping unverified→verified now starts issuing after the 30 s negative TTL,
   not after a needless 2-min cooldown.

**Acceptance criteria (met).** `TestDecisionGate_CooldownAfterFailure` (denies
without calling the underlying authorizer), `TestDecisionGate_CooldownDoesNotExtend`,
`TestDecisionGate_SuccessClearsCooldown`, `TestDecisionGate_DeniedNameNotCooledDown`,
and `TestDecisionGate_LastFailBoundedUnderDeniedSpray` in `decisiongate_test.go`,
plus the end-to-end `TestMagicManager_DeniedSNIsDoNotGrowCooldown` (200 sprayed
denied SNIs through the real `GetCertificate` → `lastFail` stays empty).

---

## SEC-4 — MEDIUM — Resolve-host decision is not cached

**Location:** `internal/certs/resolvehost.go` (`NewResolveHostAuthorizer`), wired
in `internal/certs/magic.go`.

**Defect.** The authorizer makes a fresh outbound `GET
/api/internal/resolve-host` on **every** handshake for a novel SNI, with no
memoization. certmagic v0.25.3 keeps no negative cache. An unauthenticated peer
sending many distinct off-base SNIs turns cheap TLS ClientHellos into one
authenticated backend call each (held up to the 5s client timeout) — an
amplification/exhaustion primitive against the control plane and edge goroutines.

**Fix.** Wrap the DecisionFunc in a small TTL cache keyed by name:
- **Positive** (verified) result cached ~60s → repeat handshakes for a real
  custom domain don't re-hit the control plane.
- **Negative** (unverified / non-2xx) result cached ~30s → a spray of the same
  bogus SNI collapses to one backend call per window.
- Bound the cache size (mirror the `httpStoreCacheMax` eviction pattern) so the
  cache itself isn't a spray target.
- This wrapper is the natural home for SEC-3's cooldown check too.

**Acceptance criteria.**
- Unit test: two handshakes (or two direct DecisionFunc calls) for the same
  unverified name within the TTL produce exactly **one** resolve-host call
  (assert via a stub authorizer hit counter).
- Cache is bounded: spraying `> max` distinct names keeps the map at/under the
  cap.

**Effort:** small–medium.

---

## SEC-5 — LOW — Authorizer ignores `cert_mode`

**Location:** `internal/certs/resolvehost.go`.

**Defect.** The authorizer treats any 2xx as "issue" and discards the response
body, so it ignores the `cert_mode` field. A domain the operator intended to
**delegate** (`cert_mode: "delegated"`, customer serves their own cert) still
gets an on-demand ACME cert minted by the edge — unexpected issuance, account
budget consumption, and a possible conflict with the externally managed cert.

**Fix (implemented) — ✅ DECIDED: Option A (only explicit `delegated` denies).**
Decode `beamdapi.ResolveHostResponse`; deny issuance only on an explicit
`cert_mode == "delegated"`. A **`nil`/absent** `cert_mode` or `"on_demand"`
allows. This is the drop-in choice: it fixes the reported bug (delegated domains
getting edge certs) with **zero control-plane coordination** — a control plane
that predates the field keeps issuing normally, so shipping the edge first
causes no custom-domain cert outage. (The stricter `nil → deny` fail-closed
alternative was rejected: it would silently stop every custom-domain cert unless
the web app emitted `cert_mode` in the same release.) The `*ResolveHostResponseCertMode`
pointer is nil-checked before deref; an undecodable 2xx logs and allows
(preserves prior behavior).

**Acceptance criteria (met).** `TestResolveHostAuthorizer_DelegatedDenies` and
`TestResolveHostAuthorizer_MissingCertModeAllows` in `resolvehost_test.go`.

---

## SEC-6 — LOW — Inline secret in world-readable config accepted silently

**Location:** `internal/config/server.go` — `LoadServer()`.

**Defect.** A server config may carry an inline `dns_provider_creds` (a
Cloudflare/DNS API token that controls the tunnel zone). `LoadServer` reads the
file with no permission check, so a `0644` config silently exposes a
DNS-zone-controlling credential to any local user or leaked backup. Contrast the
CLI-side secret files, which are all `0600`.

**Fix (implemented).** `LoadServer` warns loudly when `dns_provider_creds` is set
inline **and** the file mode is group/world-readable (`mode&0o077 != 0`). Chose
warn (not refuse) so an existing loose deploy still boots but is told to fix it;
the check runs against the YAML value *before* env overrides, so the
recommended `BEAMD_DNS_PROVIDER_CREDS` env form never trips it.

**Acceptance criteria (met).** `TestLoadServer_WarnsOnLooseInlineCreds` in
`config_test.go`: 0644+inline warns; 0600 silent; env-only creds silent.

---

## SEC-7 — NIT — Pin `tls.MinVersion`

**Location:** `internal/edge/edge.go` — `Serve()` (`tls.Config`).

**Defect / context.** `MinVersion` is unset. On the current Go toolchain the
server already refuses TLS < 1.2 by default, so this is not currently
exploitable — but relying on the default is fragile (a `GODEBUG` flip or older
toolchain regresses it), and linters (gosec G402) flag it.

**Fix (implemented).** `MinVersion: tls.VersionTLS12` on the edge `tls.Config`.

---

## SEC-8 — DEFERRED — No cap on concurrent unauthenticated connections

**Location:** `internal/edge/edge.go` — `Serve()` accept loop / `handle()`.

**Defect.** `preAuthTimeout` bounds how *long* any one unauthenticated handshake
or hello can take, but nothing caps the *number* of concurrent unauthenticated
TLS handshakes or half-open yamux sessions. A raw connection flood against :443
can still exhaust goroutines/FDs. (SEC-4 only closes per-SNI amplification into
the control plane, not listener-level flooding.)

**Why deferred.** For beamd's deployment model (single operator, one public IP),
the expected control is at the network layer — a cloud LB connection cap,
`fail2ban`, or firewall rate-limiting — not in-process. Building an in-process
accept-rate / max-concurrent-handshake limiter is reasonable later, but it's
availability hardening against a DoS that network infra already handles, so it's
tracked rather than blocking.

**If picked up.** A semaphore bounding concurrent `handle()` goroutines in the
pre-auth phase (released once a session authenticates or the conn closes), plus
an accept-rate limiter. `log()` what's shed so silent drops don't look like
"handled everything."

---

## Recommended order (as executed)

1. **SEC-1** + **SEC-7** — edge proxy + TLS, one pass.
2. **SEC-2** — metrics auth (Option A).
3. **SEC-4 + SEC-3 + SEC-5** — one `internal/certs` session: the `decisionGate`
   (cache), then the failure cooldown and the `cert_mode` check.
4. **SEC-6** — config hardening, independent.

Verification after each: `gofmt`, `go vet ./...`, `go build ./...`,
`go test -race -count=1 ./...` (incl. `./test/e2e/`).

---

## Implementation log

| ID | Files touched | Tests |
|----|---------------|-------|
| SEC-1 | `internal/edge/edge.go` (`proxyFor` → `Rewrite`); `test/e2e/helpers_test.go` (`/__hdrs` echo) | `TestProxy_ForgedForwardingHeadersScrubbed` |
| SEC-2 | `internal/config/server.go` (`MetricsToken` + env); `internal/edge/edge.go` (`handleMetrics` auth, `bearerTokenOK`, startup warn) | `TestMetrics_RequiresAuth` (+ existing metrics tests now authed) |
| SEC-3 | `internal/certs/decisiongate.go`, `magic.go` (`recordFailure`/`recordSuccess`) | `TestDecisionGate_Cooldown*` |
| SEC-4 | `internal/certs/decisiongate.go`, `magic.go` (gate wiring) | `TestDecisionGate_Caches*`, `_CacheStaysBounded` |
| SEC-5 | `internal/certs/resolvehost.go` (decode `cert_mode`) | `TestResolveHostAuthorizer_DelegatedDenies`, `_MissingCertModeAllows` |
| SEC-6 | `internal/config/server.go` (`LoadServer` perm warn) | `TestLoadServer_WarnsOnLooseInlineCreds` |
| SEC-7 | `internal/edge/edge.go` (`MinVersion`) | existing e2e TLS handshakes |

---

## Verified sound (do not re-audit)

These were traced during the review and are correct — don't spend time here:

- **Tenant isolation.** Routes keyed by the full canonical hostname; the
  `(name, slug) → host` mapping is injective; slug is derived server-side from
  the token, never from client input. No cross-tenant routing; non-canonical Host
  variants miss the map and 404 rather than colliding.
- **Cert-issuance gate fails closed.** Arbitrary attacker SNI cannot reach an
  ACME order — resolve-host denies (non-2xx) and a network error denies. The
  self-signed fallback conveys no authority.
- **Auth fails closed.** Hosted verify denies on every error/timeout/401; tokens
  are 256-bit `crypto/rand` looked up by hash map (no timing oracle); the reject
  message is non-enumerating.
- **No secret leakage.** Bearer token, verify secret, and DNS creds never reach
  logs, `/metrics`, or peer error messages.
- **Request smuggling / SSRF / response splitting.** Client dials only its own
  registered `127.0.0.1:<port>` by exact name match; stdlib re-serialization
  closes CL.TE/TE.CL desync and backend-driven response splitting.
- **Secret file perms.** All CLI-written secret files are `0600` / dirs `0700`;
  certmagic key material is `0600`/`0700`. (The one gap was SEC-6.)

**Scope caveat.** "Verified sound" covers *correctness* and the audited
availability vectors — NOT listener-level DoS. A raw connection flood against
:443 is out of scope here and tracked as [SEC-8](#sec-8--deferred--no-cap-on-concurrent-unauthenticated-connections).

---

## Re-review addendum (independent second pass)

A second full review (after SEC-1…SEC-8 landed) found three more issues, all now
fixed. Two are the same bug class as items already in this doc — worth noting
that "fixed one instance" did not mean "fixed the class."

### SEC-9 — HIGH — Unauthenticated ACME-amplification via `kickWild`

**Location:** `internal/certs/magic.go` `kickWild` + `certs.go:extractSlug`.

**Defect.** The SEC-3/4 fix bounded the on-demand *gate's* `lastFail`, but the
DNS-01 wildcard path had the identical flaw, worse. `GetCertificate` for an
under-base two-label SNI `<app>.<slug>.<base>` misses the eager `*.<base>`
wildcard → `kickWild`. `extractSlug` returns the **attacker-controlled middle
label** as the slug with no validation and **no known-slug check**, so
`kickWild` spawns a **real background ACME order** for `*.<slug>.<base>` and
grows `m.inflight`/`m.lastFail`. An unauthenticated peer spraying
`a.evil1.<base>`, `a.evil2.<base>`, … (ClientHello only — no handshake
completion needed) exhausts the operator's Let's Encrypt limits (50 certs/week
per registered domain) → blocks legitimate renewals → outage.

> This also corrects the earlier "Verified sound → cert-issuance gate fails
> closed" claim, which covered only the on-demand path and missed the DNS-01
> path. Both paths are now gated.

**Fix (implemented).** `MagicManager.SetHostAllowed(func(name) bool)`, wired by
the edge (`edge.New` type-asserts it) to `hostKnownForCert` — the apex or a host
with a live route. `kickWild` refuses issuance for anything else. Legitimate
tunnels have a route (registration precedes the handshake), so they still issue;
attacker SNIs have none → fallback, no ACME order. Also tightened: `m.inflight`
is `delete`d on completion and `m.lastFail` prunes expired entries, so both maps
are bounded even under legitimate churn. The dev/test `SelfSignedManager` lacks
the setter (cheap self-signed certs, no ACME), so the type-assert no-ops there.

**Acceptance (met).** `TestMagicManager_KickWildGatedByHostAllowed` (50 sprayed
unrouted SNIs → `inflight`/`lastFail`/issuance stay 0; known host allowed).
Pebble real-ACME integration still passes (gate doesn't break legit issuance).

### SEC-10 — MEDIUM — Misrouted `error` replies (asymmetric with the `registered` fix)

**Location:** `internal/client/client.go` `readControl`; `internal/proto/proto.go`;
`internal/edge/edge.go` `handleControlMsg`.

**Defect.** The SEC (reply-correlation) fix guarded the `registered` branch by
`Name` but left the `error` branch delivering to whatever `pending` existed, and
`proto.Error` had no `Name`. A late `error` from a timed-out register misroutes
onto the *next* register → wrong error returned + a **ghost tunnel** (live on the
edge, rolled out of `intended`, so replay won't restore it). Only the user-`open`
path; replay self-heals.

**Fix (implemented).** Added `Name` (omitempty) to `proto.Error`; the edge
populates it on register-scoped errors (`invalid_name`, and everything from
`register()`); `readControl` drops an error whose non-empty `Name` doesn't match
the in-flight register (empty `Name` = connection-scoped / older edge → delivered
as before). **Acceptance:** `TestClient_MismatchedErrorReplyDropped` (a
wrong-named error is dropped → register times out, not the wrong error).

### SEC-1 addendum — scrub list extended

The edge reviewer flagged that the curated scrub list omitted the bare
**`Client-IP`** header (PHP `HTTP_CLIENT_IP`, checked *before* `X-Forwarded-For`
by the common idiom). Added `Client-IP` + `Proxy-Client-Ip`, `Wl-Proxy-Client-Ip`,
`X-Proxyuser-Ip`, `X-Original-Forwarded-For`, `Forwarded-For`, `X-Forwarded`; the
e2e forged-header test asserts them scrubbed.
