# PRD — Beamd (working name): self-hostable instant-URL tunnel for multi-app dev

> Working name "Beamd" — rename freely. This document is the build spec. It is written to be executed directly by Claude Code. Read the whole thing before writing code. Where a decision is already made, do not relitigate it; where something is marked OPEN, make a reasonable call and note it.

---

## 1. Goal

Give a developer one command that turns any locally-running app into a stable, public HTTPS URL, with **a distinct subdomain per app, under a per-developer wildcard zone, on our domain** — and no per-app setup.

Target structure:

```
<app>.<dev>.beam.example.com
e.g.  api.turing.beam.example.com
      web.turing.beam.example.com
      3001.turing.beam.example.com   (default when no name given)
```

The developer (or an AI agent acting for them) runs a server on a port, runs one command, and synchronously gets back a URL that already works. The client may run on an always-on remote box **or a flaky laptop** — reconnection must be transparent and URLs must be stable across network blips.

The server is open source and self-hostable. A hosted version is the eventual business; the OSS server must stand alone.

## 2. Why it's valuable

The painful loop today: developers (increasingly, AI coding agents) run many short-lived apps on a dev box and need to test them over a real URL — for webhooks, mobile testing, sharing, or agent self-verification. Existing tools make you register tunnels one at a time, route multiple apps under awkward path prefixes, or enroll a machine into a VPN/identity mesh. None give a developer a clean *subdomain-per-app namespace under their own brand* with zero per-app ceremony.

The wedge is the AI-agent workflow: agent writes code → runs `npm run dev` on a port → calls one command → gets `https://api.turing.beam.example.com` → tests it. The platforms that solve this (Replit, v0, Cursor background agents, E2B/Daytona) solve it *internally and proprietarily*. There is no unbundled, self-hostable, bring-your-own-box version of that preview-URL primitive. That is what we build.

## 3. Why it's unique (and where it is NOT)

Be honest about this — it shapes scope.

- **vs Tailscale Funnel:** Funnel exposes one machine on `*.ts.net` with path-based multiplexing and requires tailnet enrollment. We give a *subdomain per app* under *our domain* with one binary + a token, no mesh. Subdomain-per-app also avoids the class of bugs path-routing causes (root-relative assets, absolute redirects, cookie scope, websocket paths).
- **vs ngrok / Cloudflare Tunnel:** Both are built around the human registering tunnels and are clumsy for many ephemeral, agent-spawned apps. Cloudflare needs per-hostname YAML; ngrok's wildcard/multi-tunnel UX is heavy.
- **vs Microsoft Dev Tunnels:** Closest conceptual neighbor, but not self-hostable and not agent-shaped.
- **Where we are NOT differentiated:** single app, occasional use, no branding need — Tailscale Funnel already does that for free. Do not try to win that user. The product only earns its existence on the multi-app / per-subdomain / our-domain / agent-ergonomic axis. The MVP must make *that specific loop* undeniable.

## 4. Non-goals (MVP)

Explicitly out of scope for v1. Do not build these; do not architect around them beyond leaving seams.

- Arbitrary TCP/UDP tunneling. **HTTP/HTTPS + WebSocket only** for v1.
- Customer-owned custom domains (`*.app.theircompany.com`). Phase 2 — leave the cert layer pluggable.
- Web dashboard / request-inspection UI. Phase 2.
- Multi-region / anycast edge. Later.
- Ambient port auto-discovery (auto-expose every listening port). Phase 2 — design the daemon API so it can be added without rework.
- Billing, teams, quotas beyond a basic per-token concurrency cap.

## 5. Decided technical choices (do not relitigate)

- **Language:** Go (1.22+). Single static binary for the client is a hard requirement for adoption.
- **Multiplexing:** `github.com/hashicorp/yamux` — one client↔server connection, many logical streams.
- **Edge HTTP proxy:** stdlib `net/http` + `net/http/httputil.ReverseProxy` with a custom `Transport.DialContext` that returns a yamux stream instead of a TCP socket.
- **Certificates:** `github.com/caddyserver/certmagic` for on-demand issuance + storage + renewal. Wildcard certs require ACME **DNS-01**, so a DNS provider integration is mandatory. The provider sits behind the `libdns` interface; the OSS binary compiles in a handful of common providers (Cloudflare, Route53, DigitalOcean, Hetzner, GCloud DNS, Gandi to start — actual list lives in the README and is expected to grow). Operator picks via `dns_provider:` config. Cloudflare is the reference/test target because its libdns module is the most mature — anything broken there is broken everywhere. Adding a new provider is one import + one `switch` case; downstream operators can fork to add private providers without touching the rest of the code.
- **Control protocol:** newline-delimited JSON messages over a dedicated yamux stream (stream 0). Simple, debuggable, sufficient.
- **Control transport:** the client↔server tunnel terminates on the edge's public **:443** listener, demuxed by ALPN (`beam/1`). No separate control port. This keeps clients reachable from locked-down networks (corp/hotel/CI runners) where only :443 egress is permitted. The edge inspects ALPN on the incoming TLS handshake and routes `beam/1` to the yamux handler; `h2`/`http/1.1` go to the public reverse-proxy path. Both paths share the same per-slug wildcard cert selection.
- **Auth:** opaque bearer token per developer. Token maps to a developer **slug** server-side. The slug, not the client, determines the wildcard zone — a client can never register outside its own `*.<slug>.domain`.

### 5.1 The cert/DNS model — read carefully, this is the subtle part

TLS wildcard certs match **exactly one label**: `*.turing.beam.example.com` covers `api.turing...` but not deeper. This is fine for our scheme because the structure is exactly two dynamic levels: `<app>.<slug>`.

Therefore:

- Issue **one** wildcard cert `*.<slug>.beam.example.com` **per developer**, lazily on that developer's first tunnel, via certmagic + DNS-01. Every app that developer ever exposes **reuses that one cert**. No per-app cert work, nothing on the hot path.
- DNS has the same one-label rule. `*.beam.example.com` will NOT resolve `api.turing.beam.example.com`. The operator must run authoritative DNS for the apex (or use a provider with a true nested catch-all), so any depth resolves to the edge IP. Document this as an operator requirement. For MVP, the acceptance path is: operator points `*.<slug>.beam.example.com` and `<slug>.beam.example.com` at the edge via the DNS provider API at developer-onboarding time (a `provision-dev` admin command), OR runs a wildcard authoritative zone. Implement the provider-API path; document the authoritative-DNS alternative.

## 6. Architecture

```
[ local app :3001 ]
        ▲
        │ loopback
[ beam client/daemon ]  ──(:443, ALPN "beam/1", yamux)──▶  [ beamd edge server ]
   - local control API                                   - :443 public ingress
   - CLI wraps it                                         - TLS term (certmagic)
   - reconnect + re-register                              - host-based routing table
                                                          - control-stream handler
                                                          - per-dev cert mgmt
        ▲
        │ HTTPS
[ public visitor / webhook / agent test ]
```

**Hot path (one public request):** request hits edge :443 → TLS terminated using the per-dev wildcard cert (selected by SNI) → read `Host` header → look up session in routing table → open a new yamux stream to that client → proxy request through it → client receives stream, dials `127.0.0.1:<port>`, copies bytes both ways → response returns over the same stream. No allocation-heavy work, no cert work on this path.

## 7. Repository layout

```
/cmd
  /beam          # client CLI + daemon (single binary, subcommands)
  /beamd         # edge server binary
/internal
  /proto            # control message types + (de)serialization
  /mux              # yamux setup/helpers (client + server side)
  /edge             # ingress: TLS, routing table, reverse proxy, control handler
  /certs            # certmagic config, DNS-01 provider wiring, per-slug cert mgr
  /client           # control connection, reconnect/backoff, local control API
  /auth             # token -> slug resolution (interface + file/env impl for MVP)
  /config           # server + client config loading/validation
  /naming           # port/name -> RFC1123 label, validation, collision rules
/test/e2e           # black-box: spin server + client + dummy app, assert URL works
README.md
```

Keep `internal/proto` dependency-free. Everything else can depend on it.

## 8. Control protocol (yamux stream 0, NDJSON)

Each message: one JSON object, newline-terminated. `type` discriminator.

Client → Server:
- `hello` `{ "type":"hello", "token":"<bearer>", "client_version":"x" }`
- `register` `{ "type":"register", "name":"api", "port":3001 }` — name optional; if absent server derives from port.
- `unregister` `{ "type":"unregister", "name":"api" }`
- `heartbeat` `{ "type":"heartbeat" }` (every 20s)

Server → Client:
- `hello_ok` `{ "type":"hello_ok", "slug":"turing", "base_domain":"beam.example.com" }`
- `registered` `{ "type":"registered", "name":"api", "url":"https://api.turing.beam.example.com" }`
- `error` `{ "type":"error", "code":"...", "message":"..." }` (codes: `bad_token`, `name_taken`, `invalid_name`, `over_limit`, `internal`)

Rules:
- `hello` must be first; reject the connection otherwise.
- On reconnect the client MUST replay `register` for every currently-active mapping. The server treats re-registration of an identical (slug, name)→session as idempotent.
- Subdomain is deterministic from `name` (or port), so the URL is stable across reconnects by construction.

## 9. Naming rules (`internal/naming`)

- Effective label = explicit `name` if given, else `strconv.Itoa(port)`.
- Lowercase; validate against RFC 1123 label: `^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`. Reject otherwise with `invalid_name`.
- Final hostname = `<label>.<slug>.<base_domain>`.
- Collision: within one slug, a `name` maps to exactly one active session. Second registration of a live name from a different session → `name_taken`. Re-registration from the same session (reconnect) → OK.

## 10. Client daemon + CLI

The client runs a background daemon exposing an HTTP API over a **unix domain socket** (default `~/.beam/daemon.sock`, mode `0600`; named pipe `\\.\pipe\beam-daemon-<user>` on Windows). The CLI is a thin wrapper so an agent can use either the CLI, raw HTTP-over-socket, or the MCP server below. File-system permissions enforce single-user access — no in-band auth needed.

Local API:
- `POST /expose` `{ "port":3001, "name":"api" }` → `200 { "url":"https://api.turing.beam.example.com" }` — **blocks until the URL is live (registered + control conn healthy), then returns.** This synchronous return is the core UX; do not make it fire-and-forget.
- `POST /unexpose` `{ "name":"api" }` → `200`
- `GET /list` → `[ { "name":"api","port":3001,"url":"...","healthy":true } ]`
- `GET /healthz`

**MCP server.** The daemon also exposes an MCP server (stdio transport, mountable into any MCP-speaking client) wrapping the same operations as typed tools:

- `expose_port(port: int, name?: string) -> { url: string }` — synchronous, same semantics as `POST /expose`.
- `unexpose(name: string) -> { ok: bool }`
- `list_tunnels() -> [ { name, port, url, healthy } ]`

This is the primary integration surface for AI agents — they get a discoverable, typed tool list instead of constructing CLI invocations or HTTP calls. The MCP tools are thin wrappers over the local API; both stay in sync by construction. Discoverability via the MCP tool schema is the point: the agent learns "I have an `expose_port` tool" the same way it learns about file_edit or bash.

CLI:
- `beam login --server beam.example.com --token <t>` (writes `~/.beam/config`)
- `beam expose 3001 [--as api]` → prints URL on success (exit 0), error on stderr (exit nonzero). One line of stdout = the URL, nothing else, so agents can capture it.
- `beam list`
- `beam unexpose api`
- The first `expose` auto-starts the daemon if not running.

Reconnect: exponential backoff (e.g., 0.5s → max 30s, jittered). While disconnected, `/list` marks entries `healthy:false`; on reconnect, replay registrations, flip back to healthy. Never drop a mapping due to a transient disconnect.

## 11. Server config

`beamd` config (file + env override):

```
base_domain:        beam.example.com
edge_ipv4:          1.2.3.4            # A target written by `provision-dev`
edge_ipv6:          2001:db8::1        # AAAA target (optional)
listen_https:       :443               # public ingress AND client control (ALPN-demuxed)
acme_email:         ops@example.com
acme_ca:            (default LE prod; allow staging for tests)
dns_provider:       cloudflare              # any compiled-in libdns provider; see README for list
dns_provider_creds: <from env/secret>      # provider-specific (e.g. CLOUDFLARE_API_TOKEN with Zone:DNS:Edit)
token_store:        file:/etc/beamd/tokens.json   # {token: slug} for MVP
max_tunnels_per_token: 25
```

Admin command: `beamd provision-dev --slug turing` → ensures DNS records (`*.turing.<base>` and `turing.<base>` → edge IP) exist via the provider, and pre-warms the cert. Idempotent.

## 12. Security & abuse (MVP-level only)

- Token → slug binding is the authorization boundary. A client can only register under its own slug. Enforce server-side; never trust client-sent slug.
- Per-token concurrent tunnel cap (`max_tunnels_per_token`). Exceed → `over_limit`.
- Daemon HTTP API binds to a unix socket (mode `0600`) under the user's home dir; FS perms enforce single-user access. Same model for the MCP stdio transport (inherits the daemon process's user). On Windows, named pipe with equivalent ACL.
- Reasonable request body and header limits at the edge.
- Bandwidth metering hooks: wrap the proxied copy in a counter per (slug, tunnel) and log it. Do not enforce quotas in v1, but the counter must exist (bandwidth egress is the real cost driver later; instrument now).
- Graceful shutdown: drain in-flight requests, send `error{code:"shutdown"}` to clients so they reconnect elsewhere.

## 13. Observability

- Structured logs (slog). Log: connection lifecycle, register/unregister, per-request method/host/status/bytes/duration, cert issuance events.
- `/healthz` on edge and client daemon.
- Minimal Prometheus metrics: active tunnels, active control conns, requests total (by status), bytes proxied (by slug), cert issuance count/errors. `/metrics` on edge.

## 14. Milestones & acceptance criteria

Build in this order. Each milestone has a concrete, testable "done."

**M0 — Skeleton.** Repo layout, both binaries compile and run, config loads. *Done:* `beamd` and `beam` start and print version/health.

**M1 — End-to-end single tunnel, hardcoded.** One client, one app, HTTP only, one hardcoded hostname, ACME staging or self-signed. No mux yet (single connection = single tunnel is fine here). *Done:* `curl https://hardcoded.host` returns content served by a local `:3001` dummy app, through the system.

**M2 — yamux multiplexing.** Single client↔server connection carries N concurrent app tunnels. *Done:* two dummy apps on two ports, both reachable concurrently over one control connection, verified by concurrent requests.

**M3 — Dynamic host routing + naming.** Routing table, control protocol (`register`/`registered`), name/port → subdomain, collision rules. *Done:* `register` for `api`@3001 and `web`@3002 yields working `api.<slug>...` and `web.<slug>...`; invalid names and collisions return correct error codes.

**M4 — Real certs, per-dev wildcard via DNS-01.** certmagic + DNS provider; one wildcard cert per slug, reused across that slug's apps; `provision-dev` admin command. *Done:* fresh slug, first `expose` triggers issuance of `*.<slug>.<base>`, second app under same slug serves over TLS with **no new issuance** (assert issuance count == 1).

**M5 — Client daemon, local API, MCP server, CLI, reconnect.** Synchronous `/expose`, MCP stdio server exposing `expose_port`/`unexpose`/`list_tunnels`, CLI wrapping, auto-start daemon, exponential-backoff reconnect with registration replay. *Done:* `beam expose 3001 --as api` prints a working URL and exits 0; an MCP client invoking `expose_port(3001, "api")` returns the same URL for the same inputs; killing the edge for 10s then restoring it restores all tunnels with unchanged URLs and no client restart.

**M6 — Hardening.** Token/slug auth enforcement, per-token cap, bandwidth counters, metrics, structured logs, graceful shutdown. *Done:* auth boundary cannot be bypassed by a forged slug; metrics and byte counters populate; clean shutdown drains and triggers client reconnect.

## 15. Definition of done (v1)

A developer self-hosts `beamd` against their domain and DNS provider, runs `beamd provision-dev --slug turing` once, then on their laptop (which can drop network) runs `beam expose 3001 --as api` and immediately gets `https://api.turing.beam.example.com` serving their local app over valid TLS. They can `expose` several more apps instantly with no extra setup, each on its own subdomain, all over one connection, all surviving a network blip. An AI agent can do the identical thing by calling `expose_port` on the daemon's MCP server (or the local HTTP API) and reading the returned URL.

## 16. Reference implementations to study (do not copy licenses blindly; study architecture)

- `bore` (Rust) — minimal mental model of control + data path.
- `frp` (Go) — production-grade Go architecture for host routing & multiplexing.
- `sish` (Go) — SSH-based tunneling; useful ideas on zero-extra-client UX.
- `certmagic` docs — the on-demand DNS-01 wildcard piece; this is the part most likely to be done wrong, study it before M4.

## 17. Decisions

**Resolved:**

- **DNS provider abstraction:** the `libdns` interface, with multiple providers compiled into the OSS binary. Cloudflare is the reference/test target because its libdns module is the most mature, but the binary ships with several (Cloudflare, Route53, DigitalOcean, Hetzner, GCloud DNS, Gandi to start; README has the live list). Operators pick via `dns_provider:` config; adding a new provider is a single PR.
- **Daemon transport:** unix socket with mode `0600` at `~/.beam/daemon.sock` (named pipe with equivalent ACL on Windows). FS permissions enforce single-user access; no in-band auth.
- **Control transport:** TLS on :443 with ALPN demux (`beam/1`), not a separate port — see §5. Eliminates the "client behind locked-down network" failure mode.

- **Token issuance: both flows ship in v1.** *Copy-paste* (`beam login --server <url> --token <t>`) is the default for self-hosted OSS — operator edits `tokens.json`, hands the developer the token over Slack/email, developer pastes. *Device-code* (`beam login --server <url>` → CLI prints URL + short code → developer confirms in a browser → CLI polls and writes the token) ships alongside, optional. The device-code wire endpoints (`/v1/device/code`, `/v1/device/token`) live in beamd; the *confirmer backend* (who can approve which slug and how they sign in) is pluggable — OSS ships a basic operator-approves-from-terminal confirmer, hosted swaps in OIDC/magic-link.

**Deferred:**

- **Token store backend beyond MVP `file:` impl.** Interface only for now; Postgres/etc. plug in behind it later.
- **Hosted auth backend (OIDC, magic link, etc.).** Not part of the OSS surface — the device-code confirmer is pluggable, hosted product provides its own implementation.