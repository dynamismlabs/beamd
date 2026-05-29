# Spec — Preview URL auth (signed-URL + edge cookie)

> **Status:** spec only, not implemented. Gated behind seeing the basic
> localhost+beam preview flow work end-to-end first.

## Problem

beamd tunnel URLs (`<name>.<slug>.<base>`) are public — anyone with the
link hits the (often half-built, possibly sensitive) app behind it. We
want to gate them so only a holder of a freshly-minted link can view,
while keeping the UX seamless for **browser tab** *and* **iframe inside
an authenticated app** (Flow), with sub-resource loads and HMR intact.

Constraint: beamd terminates TLS itself (gray-cloud DNS), so Cloudflare
Access/WAF can't sit in front — auth must live in the edge.

## Non-goals

- Per-user accounts / multi-tenant identity on beamd (that's the hosted
  provisioning layer, separate).
- Rate limiting / WAF / abuse protection.
- Protecting against a malicious app *behind* the tunnel.

## Design: signed URL → edge-set cookie (stateless, HMAC)

A shared secret lives on the edge and on whatever mints links (Flow).
HMAC-SHA256, no server-side session store.

### Config (`beamd.yaml`)
```yaml
preview_auth:
  enabled: false                       # off = today's public behavior
  secret_env: BEAMD_PREVIEW_AUTH_SECRET # HMAC key, from env
  cookie_name: __beam
  default_ttl: 12h                     # link + cookie lifetime
```

### Link format (minted by Flow, server-side)
```
https://<name>.<slug>.<base>/<path>?__beam=<b64url(payload)>.<b64url(hmac)>
payload = { "exp": <unix>, "host": "<name>.<slug>.<base>" }
hmac    = HMAC_SHA256(secret, b64url(payload))
```
`host` binding stops a link for app A being replayed against app B.

### Edge middleware (runs before `proxyFor`, tunnel hosts only)
`/healthz`, `/metrics`, `/.well-known/*` are always exempt.

1. **Valid `__beam` cookie** → allow, proxy as normal.
2. **No cookie, valid `__beam` query param** (sig ok, not expired, host
   matches) → set signed cookie, then **302 to the param-stripped URL**
   (clean address bar) → subsequent + sub-resource requests carry it.
3. **Neither** → `401` with a tiny "link expired or invalid" HTML page.

Cookie: `HttpOnly; Secure; SameSite=None; Path=/`, host-scoped,
`Max-Age≈ttl`, value = `b64url({exp}) . hmac`. `SameSite=None` is required
for any cross-site use; it's safe because issuance is gated by the signed
link and the cookie is host-scoped.

### ⚠️ Known issue: third-party cookies in an iframe (Safari/iOS)

`SameSite=None` is **necessary but not sufficient.** Safari/iOS (WebKit)
blocks third-party cookies entirely, and Chrome is phasing them out. When
Flow embeds the preview in an **iframe**, the preview origin
(`*.<base>`) is third-party to Flow's origin, so the `__beam` cookie set
inside the iframe will be **blocked on Safari/iOS** — breaking the
gated-preview-in-iframe flow on phones (the primary "review anywhere"
case). Mitigations, pick one (decision required):

1. **Gated previews open top-level (new tab), not iframed.** Top-level =
   first-party → cookie works everywhere. Only iframe *ungated* previews.
   Simplest; slight UX change.
2. **Host previews under Flow's own registrable domain** (e.g. base_domain
   `*.preview.flowapp.com`, Flow at `app.flowapp.com`). Same site
   (eTLD+1 = `flowapp.com`) → the cookie is *first-party*, no blocking,
   `SameSite=Lax` even works. Cleanest for the hosted product; couples the
   preview domain to Flow's domain.
3. **`Partitioned` (CHIPS).** Add the `Partitioned` attribute — restores
   the cookie in a partitioned third-party context on Chrome; Safari
   support is incomplete, so this alone doesn't fix iOS.

Recommendation: **#1 for now** (open gated previews top-level), **#2** if
beamd becomes the hosted product under your domain. Don't rely on the
in-iframe cookie on Safari.

### WebSocket / HMR
The upgrade request carries cookies, so the same cookie check applies —
HMR works once the cookie is set on first load.

### Revocation
Rotate the secret (invalidates every outstanding link/cookie), or add a
`v` epoch to the payload and bump it.

## Flow integration
- Flow stores the shared secret, mints `?__beam=…` links with a short
  `exp`, embeds them in the iframe / shares them.
- On expiry, Flow re-mints (cheap, stateless).

## Related (separate small edge feature)
Iframe embedding also needs the edge to strip `X-Frame-Options` and
relax `Content-Security-Policy: frame-ancestors` for tunnel hosts when
`preview_embed: true` — otherwise frame-busting apps won't embed. Track
separately from auth.

## Threat model
- **Link leak:** valid until `exp`; mitigate with short TTL + host
  binding. Accept residual risk.
- **No defense** against the app itself being hostile, or against a
  viewer who has a valid live link sharing it. Out of scope.

## Implementation sketch (when greenlit)
- `internal/preview/authlink.go`: `Sign(secret, host, ttl) string`,
  `Verify(secret, host, token) (ok, exp)`.
- Wrap `Edge.handler` with the middleware above before the host→proxy
  dispatch in `internal/edge/edge.go`.
- ~80–120 LOC total, no new deps (crypto/hmac, crypto/sha256).
