# Beamd — Implementation Tasks

Working checklist mapping PRD §14 milestones to discrete tasks. Check items off
as completed. Each item should be small enough to land in one commit. This is
the source of truth for milestone history and top-level work; a linked
task-specific specification may declare its own canonical sub-checklist.
`docs/transport-performance-spec.md` is the canonical sub-checklist for the
transport-performance item. The PRD remains the source of truth for the
product being built.

> **Naming note (post-M6 refactor).** The milestones below were originally built as **two binaries** — `cmd/beam` (client) and `cmd/beamd` (edge) — using the verbs `expose`/`unexpose` and a background **daemon**. They were later **merged into the single `beamd` binary** (same binary for `serve` and `open`): the verbs were renamed `open`/`close`, the background worker became the **agent**, and client state moved `~/.beam/` → `~/.beamd/` (socket `agent.sock`, config `config`). The checklist text uses the **current** names; the original two-binary milestone *structure* (the `cmd/beam` headings, "both binaries") is kept as a historical record.

---

## Deferred work — still open going into v1

The first cluster (real ACME issuance + cert persistence) blocked "real MVP" status and is now resolved in code. What remains is non-blocking — Cloudflare-on-macOS/Linux works end-to-end without any of these.

### Reclaimed

- [x] **`certs.MagicManager`** — certmagic + ACME DNS-01 via libdns. Same `Manager` interface as `SelfSignedManager`; beamd picks one based on `acme_ca` (blank/URL → real ACME, `off`/`self_signed` → self-signed). **Wired but not exercised against a real ACME server in CI** — operator should validate locally against LE staging (`acme_ca: https://acme-staging-v02.api.letsencrypt.org/directory`) before pointing production traffic. Unit tests cover construction + fallback-SNI behavior.
- [x] **Local FS cert storage** — `certmagic.FileStorage` at `<data_dir>/certs/`. `data_dir` is a server-config field (default `/var/lib/beamd`).
- [x] **README operator walkthrough** — full Cloudflare + Let's Encrypt setup story, config reference, developer quickstart, MCP server doc.
- [x] **Dockerfile** — multi-stage build, distroless final image, runs as non-root, exposes :443.
- [x] **GoReleaser config** — cross-platform binaries (linux/darwin × amd64/arm64) + container image pushed to GHCR on tag.
- [x] **WebSocket pass-through end-to-end test** — `TestM6_WebSocketPassThrough` connects a `gorilla/websocket` client through the tunnel, round-trips two messages, verifies the duplex copy doesn't hang up between frames.
- [x] **Configurable request body size limit** — `MaxRequestBodyBytes` server-config field (default 32 MiB), wrapped via `http.MaxBytesReader` on every public request. `TestM6_RequestBodySizeLimit` confirms a 1 KiB-capped edge accepts a 100-byte body and rejects a 4 KiB body.

### Still deferred (non-blocking for OSS v1)

- [ ] **Tunnel performance hardening (A1 shipped; Part B implemented
  default-off).** A1 and the G1 measurement/decision gate are complete. The
  two qualification-discovered TCP/yamux corrections passed their targeted
  staging rechecks. The following matrix cleared both defects and completed 36
  of 48 blocks before exposing an undersized high-RTT/loss harness deadline;
  its profile-aware correction let the exact targeted recheck advance through
  direct and beamd QUIC plus direct TCP, where the beamd-TCP stage exposed a
  60-second heartbeat timeout despite active data transfer. The narrow
  data-activity liveness correction passed that exact targeted recheck, and the
  next matrix completed 39 of 48 blocks with 796 clean records before its
  high-RTT/loss upload/TCP mixed case exposed the shared five-second
  caller-visible yamux open bound. TCP now has a distinct 60-second bound below
  yamux's 75-second internal timer, and the harness has an exact mixed-target
  path. Its first targeted staging recheck completed all eight interactive
  records cleanly but found three background TCP streams still reaching the
  agent's five-second name-prefix deadline while the session stayed live.
  Prefix setup now remains five seconds on QUIC and is 60 seconds on yamux at
  both peers; backend dial remains five seconds. Immutable candidate `bfc94f0`
  passed the exact targeted staging gate with all interactive and live bulk
  evidence clean, and its fail-closed chain started a fresh 48-block matrix
  from block one. That matrix is now the active gate.
  Permanent product-aware
  defaults keep self-hosted/token on TCP with edge QUIC disabled, while
  hosted/session resolves to `auto` and the hosted edge enables QUIC only after
  B4 qualification and the production-link pilot. The single canonical
  specification and executable checklist is
  [`docs/transport-performance-spec.md`](docs/transport-performance-spec.md).
  Work its Section 16 top to bottom; do not duplicate or independently track
  the subtasks here.
- [ ] **Additional libdns providers compiled in** — Route53, DigitalOcean, Hetzner, GCloud DNS, Gandi. Today only `cloudflare` + `stub` are wired in `internal/dns/dns.go`'s `Open()`. Each is one import + one `case`. Operators on other DNS hosts can vendor it themselves until we land more.
- [x] **Device-code login flow — CLI side** — `beamd login` without `--token` now does the device-code dance against whatever web app the operator advertises via `auth_discovery` in beamd.yaml. Discovery endpoint at `/.well-known/beam-auth`. `internal/devicecode` package implements the polling. The *server* side (the `/api/device/code` + `/api/device/token` endpoints + the browser-based approval page) lives in the hosted web app, not in this repo.
- [x] **`auth.HTTPStore`** — hosted beamd's `auth.Store` impl. POSTs to a verify endpoint with shared-secret auth, caches 60s positive / 5s negative. `token_store: http(s)://...` in beamd.yaml, secret via `BEAMD_AUTH_VERIFY_SECRET` env var.
- [ ] **Windows agent transport** — Unix socket only today. Named-pipe equivalent (with ACL) per PRD §17.
- [ ] **`token_store: file:<path>` YAML quoting** — Already documented in README + example config. Keep pinned until we've watched a few operators not trip over it.

---

## M0 — Skeleton ✅

Goal: both binaries build, both load config, both print version, server serves `/healthz`. No tunnel logic yet.

### Foundation
- [x] `go.mod` initialized at `github.com/dynamismlabs/beamd` (rename if needed) on Go 1.22
- [x] Repo directory layout per PRD §7 created (M0 subset: `cmd/`, `internal/config/`, `example/` — other `internal/*` dirs land with their milestones)
- [x] `LICENSE` — Apache 2.0
- [x] `.gitignore` — standard Go ignores + local dev artifacts
- [x] `README.md` — install/build/run stub
- [x] `Makefile` — `build`, `test`, `run-server`, `clean` targets
- [x] `example/beamd.yaml` — sample server config

### Server binary (`cmd/beamd`)
- [x] `main.go` with subcommands: `serve` (default), `provision-dev`, `issue-token`, `version`
- [x] `--config <path>` flag on `serve`; default `/etc/beamd/beamd.yaml`
- [x] `--version` prints semver from build-time `var Version`
- [x] slog initialized with JSON handler
- [x] On `serve`: loads config, logs "ready" with parsed values, serves `/healthz`

### Client binary (`cmd/beam`)
- [x] `main.go` with subcommands: `login`, `open`, `list`, `close`, `agent`, `mcp`, `version`
- [x] `--version` prints semver
- [x] slog initialized
- [x] All M5 subcommands stubbed to print "not implemented (M5)" and exit nonzero

### Config (`internal/config`)
- [x] `server.go` — Server struct + YAML loader + env override (`BEAMD_*`)
- [x] Server fields: `base_domain`, `edge_ipv4`, `edge_ipv6`, `listen_https`, `acme_email`, `acme_ca`, `dns_provider`, `dns_provider_creds`, `token_store`, `max_tunnels_per_token`
- [x] `client.go` — Client struct + YAML loader; tolerates missing file
- [x] Client fields: `server`, `token`, `agent_socket`
- [x] Validation returns clear errors for missing required server fields
- [x] Unit tests: valid load, invalid (missing required) fails, env override, default agent socket path

### Health
- [x] `beamd serve` exposes `GET /healthz` returning `{"status":"ok","version":"..."}`

### Build/Test
- [x] `make build` produces `bin/beamd` and `bin/beam`
- [x] `make test` runs `go test ./...` and passes
- [x] Both binaries pass `--version`

**M0 done when:** `make build && bin/beamd serve --config example/beamd.yaml` logs `ready` with parsed config, `curl localhost:8443/healthz` returns ok JSON, and `make test` is green.

**Verified 2026-05-17:** built clean, tests green, `{"status":"ok","version":"dev"}` returned from `/healthz`.

---

## M1 — End-to-end single tunnel, hardcoded ✅

Goal: hardcoded one-app-on-one-port tunnel, HTTP-only, single TLS connection between client and server, no mux. ACME staging or self-signed cert.

### Edge
- [x] TLS listener on `listen_https` with self-signed cert for `hardcoded.host` (generated at startup, valid 24h, includes 127.0.0.1 in SANs)
- [x] ALPN demux: `beam/1` → client control conn; anything else → public HTTP path (this is PRD §5 landing earlier than originally planned, since we needed it to distinguish the two conn flavors)
- [x] Hardcoded routing: any incoming Host → the one connected client
- [x] Public request → write raw HTTP to the client conn, read response, return to public visitor. Serialized via `reqMu` (M1 only; yamux removes the constraint in M2). Decision: did NOT use `httputil.ReverseProxy` for the beam-side hop because `http.Server` exits early on non-default ALPNs and we need the same bidirectional conn for many requests.
- [x] Backend conn obtained from a server-side state struct (single client allowed for now)

### Client
- [x] Dial server on `listen_https`, complete TLS handshake (with `InsecureSkipVerify` for the M1 self-signed cert; real verification in M4)
- [x] Read HTTP requests off the conn with a raw `bufio.Reader` + `http.ReadRequest`. Did NOT use `http.Server` here: it sees the `beam/1` ALPN, returns from `c.serve` without running the handler, closes the conn — found this the hard way during M1 (see debug notes in m1.go).
- [x] For each request: dial backend on `127.0.0.1:<hardcoded port>`, forward request, read response, write back to edge.

### Test infra
- [x] `test/dummy-app/main.go` — HTTP server echoing method + path
- [x] `TestTunnel_SingleRegisteredAppServesPublicURL` (in `test/e2e/e2e_test.go`) — spins server + client + dummy app in-process, verifies two sequential requests and `/healthz` bypass

**M1 done when:** `go test ./test/e2e -run M1` passes — full path edge→client→backend works.

**Verified 2026-05-17:** `TestM1_SingleTunnel` passes; two sequential `/foo`+`/bar` requests succeed over one TLS conn; `/healthz` returns ok without going through the client.

---

## M2 — yamux multiplexing ✅

Goal: one client↔server TLS connection carries N concurrent app tunnels.

### Mux (`internal/mux`)
- [x] Wrap yamux client + server setup (20s keepalive, 30s write timeout, slog-routed logging)
- [x] Server: accept yamux session per inbound TLS conn
- [x] Client: open yamux session over single TLS conn to server

### Edge integration
- [x] Routing table indexed by host → route name (hardcoded for M2 via `AddRoute`; control protocol populates it in M3)
- [x] For each public request, open new yamux stream and proxy through it
- [x] Custom `Transport.DialContext` returns yamux stream conn (with `<name>\n` framing prefix, `DisableKeepAlives` so each request gets a fresh stream)
- [x] Per-host `ReverseProxy` cached lazily; reads `e.session` dynamically so reconnects (M5) pick up the new session for free

### Client integration
- [x] Accept inbound yamux streams; read `<name>\n` prefix, look up port in backend map
- [x] Dial backend per stream; bidirectional `io.Copy`, with both-sides-close on either copy returning
- [x] Two (or N) hardcoded backend ports supported via `client.Run(ctx, addr, backends)`

### Tests
- [x] E2E: two dummy apps on two ports, two hostnames, sequential + cross-host no-leak (`TestM2_TwoTunnels`)
- [x] Load: 100 concurrent requests across both backends, no goroutine leak (after closing test-client idle conns)
- [x] Bonus: unknown host returns 404 (`TestM2_UnroutedHost`)

**M2 done when:** Two hardcoded host→backend pairs work concurrently over one TLS conn.

**Verified 2026-05-17:** `TestM2_TwoTunnels` runs 100 concurrent requests over one yamux session with no interference and no goroutine leak. M1 single-tunnel test still passes against the new yamux-based edge/client.

---

## M3 — Dynamic host routing + naming + control protocol ✅

Goal: control protocol on stream 0, dynamic registration, RFC 1123 naming, collision rules.

### Control protocol (`internal/proto`)
- [x] Message types per PRD §8: `hello`, `hello_ok`, `register`, `registered`, `unregister`, `heartbeat`, `error`
- [x] NDJSON encode/decode — `Write` and `Read` helpers; one JSON object per line; `type` discriminator
- [x] `proto_version` field on `hello`/`hello_ok` for future-proofing (constant `ProtoVersion = 1`)
- [x] Unknown message types → `error{code:"unknown_message"}`
- [x] Round-trip unit tests

### Naming (`internal/naming`)
- [x] RFC 1123 label validation: `^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`
- [x] Port → label fallback (`strconv.Itoa`)
- [x] Hostname assembly: `<label>.<slug>.<base_domain>`
- [x] Unit tests for valid/invalid labels

### Auth / token store (`internal/auth`)
- [x] `Store` interface + `MemoryStore` + `FileStore`
- [x] `Open(spec)` parses `file:<path>` and `memory:` forms
- [x] Unit tests

### Edge control handler
- [x] On yamux session establish, accept the first stream (the control stream), expect `hello` first; reject otherwise
- [x] Resolve token → slug via injected `auth.Store`
- [x] Send `hello_ok` with slug + base_domain
- [x] Handle `register`: validate (or derive from port), check collisions, add to routing table, send `registered`
- [x] Handle `unregister`: remove from routing table; idempotent
- [x] Handle session liveness: heartbeats and successful data-stream I/O reset the activity timer; the watchdog drops an inactive session after the configurable timeout (default 60s, test override via `SetHeartbeatTimeout`)
- [x] Collision: same (slug, name) different session → `name_taken`; same session → idempotent OK
- [x] Drop-on-disconnect: `dropSession` removes routes when yamux session ends

### Client control
- [x] `client.Connect(ctx, addr, token, opts...)` — opens TLS, yamux, control stream, sends `hello`, awaits `hello_ok`
- [x] `c.Register(name, port)` / `c.Unregister(name)` — serialized control RPCs
- [x] Heartbeat goroutine every `HeartbeatInterval` (default 20s; test override via `Options`)
- [x] Background `acceptStreams` goroutine handles data streams (yamux + `<name>\n` prefix lookup)

### Tests
- [x] `TestM3_RegisterTwoTunnels` — api@p1 + web@p2, both URLs serve correctly
- [x] `TestM3_DefaultNameFromPort` — register with empty name, server derives label from port
- [x] `TestM3_InvalidName` — `Bad_Name`, `API`, `has.dot`, leading/trailing hyphen, oversized → `invalid_name`
- [x] `TestM3_BadToken` — connect with unknown token → `bad_token`
- [x] `TestM3_NameCollision` — second session same slug + same name → `name_taken`; same session re-register → idempotent
- [x] `TestM3_HeartbeatTimeoutDropsSession` — client with long interval gets watchdog-killed within 1s when edge timeout is 200ms
- [x] `TestM3_HardCloseDropsSession` — client.Close() → server drops session + routes
- [x] `TestM3_PreviousNameUsableAfterDrop` — c1 disconnects → c2 can claim the same name (foundation for M5 reconnect)

**M3 done when:** Dynamic register/unregister with correct error codes and stable URLs by name.

**Verified 2026-05-18:** 11 e2e tests pass (M1 + M2 + M3) plus unit tests for `auth`, `naming`, `proto`, `config`.

---

## M4 — Per-dev wildcard cert architecture + libdns wiring ✅

Goal: per-slug wildcard cert lifecycle (one cert per slug, reused), pluggable DNS provider behind libdns, `provision-dev` admin command. Real ACME/certmagic issuance is deferred to a follow-up; the `Manager` interface lets it slot in without touching callers.

### Cert plumbing (`internal/certs`)
- [x] `Manager` interface: `GetCertificate(hello) / IssuanceCount() / PreWarm(slug)`
- [x] `SelfSignedManager`: per-slug `*.<slug>.<base>` + `<slug>.<base>` SAN cert, ECDSA P-256, 30-day validity
- [x] SNI-based cert selection (`extractSlug` parses `<app>.<slug>.<base>`)
- [x] Issuance counter (slog event + `IssuanceCount()` for tests/metrics)
- [x] Fallback cert for off-domain SNI (`localhost`, `*.<base>`) — covers the e2e/healthz path
- [ ] **Deferred:** `MagicManager` — certmagic + ACME DNS-01 wiring. Behind same `Manager` interface, so M5/M6 callers don't change when it lands.
- [ ] **Deferred:** local FS cert storage at `<data_dir>/certs/` (relevant once MagicManager is in)

### DNS provider (`internal/dns`, libdns)
- [x] Provider interface = composition of libdns Getter/Appender/Setter/Deleter
- [x] Cloudflare via `github.com/libdns/cloudflare` (compiled in; requires `dns_provider_creds`)
- [x] `StubProvider`: in-memory libdns provider for tests + dev (`dns_provider: stub`)
- [x] `Open(name, creds)` factory selects by `dns_provider:` config
- [x] `ProvisionSlug(ctx, provider, base, slug, v4, v6)` upserts apex + wildcard A/AAAA records
- [ ] **Deferred:** compile in Route53, DigitalOcean, Hetzner, GCloud DNS, Gandi (one-import-one-case per provider per PRD §5)

### Admin command (`beamd provision-dev`)
- [x] `--slug` flag (required), `--config` flag
- [x] Loads server config, opens DNS provider, calls `ProvisionSlug`
- [x] Pre-warms cert via the configured cert manager
- [x] Idempotent (SetRecords replaces records by `(name, type)` — verified in `TestM4_ProvisionSlugWritesDNSRecords`)

### Tests
- [x] `internal/certs` unit tests: extractSlug, reuse-across-apps, distinct-slugs, fallback, PreWarm
- [x] `internal/dns` unit tests: Open, ProvisionSlug, StubProvider AppendThenDelete, idempotency
- [x] `TestM4_CertReuseAcrossAppsUnderSlug` — first slug request issues cert; second app under same slug reuses (`issuance_count == 1`)
- [x] `TestM4_DistinctSlugsGetDistinctCerts` — second slug → second issuance
- [x] `TestM4_ProvisionSlugWritesDNSRecords` — DNS provider receives expected A/AAAA records, idempotent rerun
- [x] `TestM4_TwoTokensSameSlugShareCert` — two tokens mapping to the same slug share the cert
- [x] Manual smoke: `bin/beamd provision-dev --slug turing --config example/beamd.yaml` against stub provider succeeds

**M4 architecture done; ACME issuance deferred.** The cert-cache + SNI-selection + DNS provision flow is in place and tested. Real certmagic/ACME issuance plugs in behind `Manager` without touching the rest of the codebase.

**Verified 2026-05-18:** 15 e2e tests pass; new unit tests for `certs` and `dns` green; `provision-dev` smoke-tested manually.

---

## M5 — Client agent, local API, MCP server, CLI, reconnect ✅ (device-code deferred)

Goal: full client UX with reconnect-with-replay, agent, CLI, MCP. Device-code login is the lone deferred piece (tracked in the top-of-file Deferred section).

### Agent (`internal/daemon`)
- [x] Agent process; unix socket listener at `~/.beamd/agent.sock` (0600)
- [ ] **Deferred:** Windows named pipe equivalent
- [x] HTTP API on socket: `POST /open`, `POST /close`, `GET /list`, `GET /healthz`
- [x] `/open` blocks until tunnel registered (via client.Register's wait-for-session loop)
- [x] Agent owns the yamux conn to edge via a wrapped `*client.Client`

### Auto-start (`internal/daemon.EnsureRunning`)
- [x] CLI probes the socket; if absent, spawns `beamd agent --socket <path>` detached (`setsid`)
- [x] Agent log file at `~/.beamd/agent.log` (opened append, 0600)
- [x] Subsequent CLI calls reuse the running agent (probe succeeds → no respawn)

### Reconnect (`internal/client.Client`)
- [x] Background `manage()` goroutine watches `session.CloseChan()` and reconnects on close
- [x] Exponential backoff: 500ms → max 30s, ±25% jitter (`jitter()`)
- [x] On reconnect: re-hello + replay every entry in `c.intended` (`replayIntended`)
- [x] While disconnected: `Register` blocks up to `RegisterTimeout` waiting for a session; `/list` reports `Healthy: false`
- [x] Server-side: identical (slug, name) re-register from the same session is idempotent (PRD §8) — same logic that was added in M3 now exercised by the replay path

### CLI (`cmd/beam`)
- [x] `beamd login --server <url> --token <t>` — copy-paste flow; writes `~/.beamd/config`
- [ ] **Deferred:** `beamd login --server <url>` (no token) — device-code flow
- [x] `beamd open <port> [--as <name>]` — prints URL on stdout (and only the URL)
- [x] `beamd list` — name / port / health / URL table
- [x] `beamd close <name>`
- [x] `beamd agent --socket <path>` — internal entry point used by EnsureRunning

### Device-code flow
- [ ] **Deferred (whole subsection):** server `/v1/device/code` + `/v1/device/token`, client polling loop, `Confirmer` interface, OSS `beamd issue-token` confirmer. See top-of-file Deferred section.

### MCP server (`internal/mcp`, `beamd mcp`)
- [x] `beamd mcp` subcommand runs the stdio JSON-RPC 2.0 server
- [x] Methods: `initialize`, `notifications/initialized`, `tools/list`, `tools/call`, `ping`
- [x] Tool `expose_port(port: int, name?: string) → { content: [{text: <url>}] }`
- [x] Tool `remove_tunnel(name: string) → { content: [{text: "ok"}] }`
- [x] Tool `list_tunnels() → { content: [{text: <json items>}] }`
- [x] Schemas valid per MCP 2024-11-05 spec (input-schema JSON Schema, `serverInfo`)
- [x] All three tools dispatch to the agent's `LocalClient` — no logic duplication

### Tests
- [x] `TestDaemon_OpenListCloseRoundTrip` — agent's `/open` returns a working URL; `/list` shows it; `/close` removes it
- [x] `TestM5_HealthzReportsSlug` — `/healthz` returns slug + healthy
- [x] `TestM5_ReconnectReplaysRegistration` — `e.CloseAllSessions()` → client reconnects + replays → same URL still serves
- [x] `TestM5_MCPRoundTrip` — initialize → tools/list (3 tools) → tools/call expose_port → URL serves
- [ ] **Deferred:** device-code login E2E (lands with the device-code work)

**M5 architecture done; device-code login deferred.** Agent + reconnect + CLI + MCP all work end-to-end; copy-paste auth is the only login path.

**Verified 2026-05-18:** 19 e2e tests pass across M1–M5; all package unit tests green.

---

## M6 — Hardening ✅ (WS test + body-size limit deferred)

Goal: auth boundaries enforced, metrics + logs populated, graceful shutdown, edge correctness.

### Auth & limits
- [x] Token → slug enforced server-side; client cannot forge slug (no slug field in `hello`; slug derived from token via `auth.Store`)
- [x] Per-**slug** tunnel cap (`max_tunnels_per_token`) → `over_limit`. Counts aggregate across sessions for the same slug — the natural reading of "per token" since a token always maps to exactly one slug.
- [x] Bad-token rejection (existed since M3 via `TestM3_BadToken`)
- [x] `TestM6_PerTokenTunnelCap` — cap of 2; third register → `over_limit`

### Proxy correctness
- [x] `X-Forwarded-For` (chained), `X-Forwarded-Proto: https`, `X-Forwarded-Host` set in the proxy Director
- [x] Host header: preserved as the external host (`req.URL.Host = host` in Director where `host` is the public-facing hostname)
- [x] `responseRecorder` implements `http.Hijacker` so WebSocket upgrades pass through; full E2E test deferred
- [x] `http.Server.MaxHeaderBytes = 1 MiB` on every public conn
- [ ] **Deferred:** configurable request body size limit (PRD §12 — see top-of-file Deferred section)
- [ ] **Deferred:** WebSocket pass-through E2E test (architecture supports it; needs real WS server/client to verify)

### Bandwidth metering
- [x] `metrics.recordRequest(status, bytes, slug)` updates `bytes_proxied_total{slug}` on every public response
- [x] Per-request slog log line: `host, method, status, bytes, duration_ms, slug` (paths/headers excluded)
- [x] Exposed via Prometheus text format at `/metrics`
- [x] `TestM6_BandwidthCounterReflectsResponseBytes` — bytes counter non-zero after a 13-byte response

### Metrics + logs
- [x] `/metrics` path-bypass (alongside `/healthz`) on the edge
- [x] Counters: `beam_active_sessions`, `beam_active_tunnels`, `beam_cert_issuance_total`, `beam_requests_total{status}`, `beam_bytes_proxied_total{slug}`
- [x] slog structured logs for: session lifecycle, register/unregister/reclaim, cert issuance, per-request, shutdown
- [x] No paths or arbitrary request headers in logs by default (host/method/status only)
- [x] `TestM6_MetricsEndpointExposesCounters` — `/metrics` exposes all expected counters in valid Prometheus format

### Graceful shutdown
- [x] `Edge.Shutdown(ctx)` stops accepting new conns, sends `error{code:"shutdown"}` to every session, drains every per-public-conn `http.Server` via `Shutdown(ctx)` concurrently, then force-closes yamux
- [x] `beamd serve` handles SIGTERM/SIGINT → `Shutdown(ctx with 10s timeout)`
- [x] Client: on `error{code:"shutdown"}`, sets `skipBackoff` atomic; the next reconnect attempt fires immediately (no 500ms sleep)
- [x] `TestM6_GracefulShutdownNotifiesClients` — after `Shutdown()`, client flips unhealthy within 1s

### Reclaim-on-reconnect (bug found during M6)
- [x] During reconnect-with-replay (M5), the new session's `register` would race against the old session's `dropSession` and get `name_taken`. Fixed by checking `existing.session.yamux.IsClosed()` in `register` and taking over the route if the holder is already dead. Documented inline in `register`.

**M6 architecture done; WS pass-through E2E and body-size limit deferred.** Auth boundary verified, metrics + logs populate, shutdown drains cleanly with explicit client signaling.

**Verified 2026-05-18:** 24 e2e tests pass across M1–M6; all package unit tests green.

---

## v1 release prep

- [x] README: install (binary + Docker), quickstart (operator + developer), config reference, DNS provider matrix
- [x] Example operator setup walkthrough (Cloudflare DNS) — folded into README
- [x] GoReleaser config for cross-platform binaries (`linux/darwin × amd64/arm64` + GHCR image)
- [x] Dockerfile for `beamd`
- [ ] CONTRIBUTING.md
- [ ] CHANGELOG.md
- [ ] Tag v0.1.0 — gated on a real-ACME smoke test against LE staging
