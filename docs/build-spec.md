# Beamd — Build Spec & Task List

> Self-contained work plan for making beamd cleanly **consumable by other
> apps** (primarily "Flow", an AI task manager that exposes agent-built
> apps) and **distributable standalone** (like ngrok, on your own domain).
> Each item is a checkbox. Acceptance criteria are concrete. You should
> not need any external context beyond this file + the codebase.

---

## 0. Background (read first)

**What beamd is.** A self-hostable, instant-URL HTTPS tunnel for multi-app
dev. **One binary, `beamd`** (the brand), with subcommands — `serve` runs
the edge, the rest are the client:

- **`beamd serve`** — the *edge server*. One instance runs on a public VM.
  It terminates public HTTPS on `:443`, ALPN-demuxes between public
  visitors and client control connections (yamux-multiplexed over one TLS
  conn per developer), reverse-proxies each request to the right client,
  and issues per-developer **wildcard** certs `*.<slug>.<base_domain>`
  from Let's Encrypt via ACME DNS-01 (Cloudflare libdns).
- **`beamd open <port>`** — the *client*. Runs on whatever machine hosts the
  app being exposed; opens the outbound tunnel to the edge and forwards
  inbound requests to a local TCP port. **Foreground by default** (holds
  the tunnel in that process, like `ngrok`); `-d/--detach` hands it to a
  background worker, **`beamd agent`**. An MCP server (`beamd mcp`) and the
  agent's local socket are the programmatic controllers.

The **same binary** runs both roles: `beamd serve` on your public box,
`beamd open` on dev machines. Config + socket live under `~/.beamd/`.

**Identity model.** A **token** maps to a **slug** (`tokens.json`,
`{token: slug}`). A developer with slug `turing` gets `*.turing.<base>`. The
app name is chosen when you bring it up: `beamd open 3000 --as api` →
`https://api.turing.<base>`.

**Key constraint — wildcard depth.** The cert covers `*.<slug>.<base>`,
**one DNS label deep**. `api.turing.base` works; `a.b.turing.base` does not
(Let's Encrypt wildcards don't nest). Every distinct app/version must be
a single label (use hyphens: `proj-ws-api`, not dots).

**Current repo state (important):**
- Module path in `go.mod` is `github.com/treyhuffine/beamd` but the repo
  lives at `github.com/dynamismlabs/beamd` — **mismatch, fixed in §1.**
- No release is tagged; the Docker image
  `ghcr.io/...beamd:latest` is **not published**. Build from source.
- `base_domain` may be an **apex or a subdomain** of a Cloudflare zone —
  beamd auto-detects the zone (`internal/dns/dns.go: ResolveZone`).
- The edge HTTP surface is only `/healthz`, `/metrics`,
  `/.well-known/beam-auth`; everything else routes to a tunnel by `Host`.
- `.goreleaser.yaml` already cross-compiles `beamd`/`beam`/`beam-testapp`
  for linux+darwin × amd64+arm64 and builds the docker image.

**Consumer context (why this work exists).** Flow runs each agent task in
a git worktree named `<workspace>-<uuid-substring>` and wants to expose
the worktree's dev server at a stable public URL it can open or iframe
from any device. Flow drives `beamd` programmatically (bundled binary +
spawn, or the agent socket). Flow uses the **detached** path
(`beamd open … -d --json`) and needs reliable machine-readable output and
(later) per-link auth.

---

## Phase 0 — DO NOW (open-source + personal use)

§1–§9 below are the full **product** roadmap. For the immediate goal —
open-source beamd and use it personally with Flow — do only these, in
order. The current `beam`/`beamd` split already works; **no merge/rename
of the binary is required to use it.**

- [ ] **Module rename only** (§1's *rename* bullets — `go.mod` + imports + `.goreleaser` image template — **not** the single-binary consolidation). Rebuild/retag the image and redeploy; `beamd serve` is unchanged.
- [ ] *(Optional, for OSS usability)* **Tag `v0.1.0`** so GoReleaser publishes the image + binaries others can pull.

**Defer to "the product, later":** single-binary merge, `open`/`close`
rename, foreground mode, npm packaging, hosted, signed-URL auth (the rest
of §1–§9).

---

## 1. Module/identity rename + single-binary consolidation  `[priority: P0]`

Two structural changes done together: (a) repo/module/image/docs all say
`dynamismlabs`; (b) collapse the two binaries (`beam` client + `beamd`
server) into **one binary `beamd`** with subcommands. Brand = binary =
website ("Beamd"), and the `beam`/`beamd` ambiguity goes away.

**Rename:**
- [x] Change `module github.com/treyhuffine/beamd` → `github.com/dynamismlabs/beamd` in `go.mod`.
- [x] Rewrite every import: `find . -name '*.go' | xargs sed -i '' 's#github.com/treyhuffine/beamd#github.com/dynamismlabs/beamd#g'`.
- [x] Update `.goreleaser.yaml` image templates `ghcr.io/treyhuffine/beamd` → `ghcr.io/dynamismlabs/beamd` and the `image.source` label.
- [x] Grep the tree for `treyhuffine` and replace everywhere.

**Single binary:**
- [x] Merge `cmd/beam` and `cmd/beamd` into one entrypoint (keep `cmd/beamd`): a subcommand dispatcher where `serve` → edge code (`internal/edge`), and `open`/`close`/`list`/`login`/`mcp`/`agent` → client code (`internal/client`, `internal/daemon`, `internal/mcp`). Delete the `cmd/beam` main once merged.
- [x] Rename the internal background worker subcommand `daemon` → **`agent`** (it's internal, spawned by `-d`; not user-facing). Update `internal/daemon` references/log lines accordingly.
- [x] Move client state from `~/.beam/` → **`~/.beamd/`**: `config` and the socket (`~/.beamd/agent.sock`). Update `internal/client`/`internal/daemon` path constants and any docs.
- [x] Update `.goreleaser.yaml` `builds:` to produce a single `beamd` binary (drop the separate `beam` build; keep `beam-testapp` for the smoke test, or rename to `beamd-testapp`).
- **Acceptance:** `go build ./...`, `go vet ./...`, `go test ./...` pass; `grep -rn treyhuffine .` is empty; `beamd serve` runs the edge and `beamd open 3000` runs the client from the *same* binary; client state is under `~/.beamd/`; goreleaser snapshot builds one `beamd` per platform.

---

## 2. Command surface + programmatic interface  `[P0]`

Define the client command surface and foreground vs. background behavior
(the rename from `expose`/`unexpose` and the single-binary merge are in §1).

**Command surface (all under the one `beamd` binary):**
- **`beamd serve`** — run the edge (server role; see §0). Unchanged in spirit from today's `beamd serve`.
- **`beamd open <port> [--as <name>]`** — bring a local port up as a public URL.
  - **Foreground by default** (like `ngrok` / `docker run`): holds the
    tunnel in *this* process, prints the URL prominently, and tears the
    tunnel down on Ctrl-C / process exit. **No agent involved.**
  - **`-d, --detach`** — hand off to the background **agent**, print the
    URL, and return immediately. The agent is spawned on demand
    (`ensureAgent`) and is used **only** in detach mode. This is what
    automation (Flow) calls.
  - Optional shorthand: bare `beamd <port>` aliases `beamd open <port>`.
- **`beamd close <name>`** — tear down a detached tunnel (idempotent: exit 0
  if already gone). Foreground tunnels are stopped with Ctrl-C.
- **`beamd list`** — list detached tunnels.

- [x] Implement **foreground** mode for `beamd open` (the default): open the tunnel in-process, print the URL + a clear "tunnel live — Ctrl-C to stop" line, block until signal, then tear down.
- [x] Implement **`-d/--detach`**: the agent-backed path — register via the agent, print the URL, exit.
- [x] `--json` on `beamd open` (both modes): print exactly one JSON object and nothing else — `{"url":"https://<name>.<slug>.<base>","name":"<name>","port":<n>,"slug":"<slug>","baseDomain":"<base>"}`. In foreground+`--json`, print the object once when live, then keep running (tear down on signal).
- [x] `--json` on `beamd list`: array of `{"name","url","port","healthy"}`.
- [x] `beamd close <name>`: idempotent + `--json` returning `{"name","removed":true|false}`.
- [x] `beamd status --json`: agent running state, server, slug, connection health (for caller reconciliation).
- [x] Document the **agent local API** (the unix-socket HTTP server the detach path talks to). Write `docs/agent-api.md`: socket path (`~/.beamd/agent.sock`), endpoints, request/response JSON, and a Node example using `http` with `socketPath`. Treat these shapes as a stable v1 contract.
- **Acceptance:** `beamd open 3000 --as test` runs in the foreground, prints the URL, and Ctrl-C tears it down; `beamd open 3000 --as test -d --json | jq -e .url` returns immediately with the URL; `beamd close test` removes it and is a no-op the second time; piping any `--json` command into `jq` never fails on extra text; `docs/agent-api.md` lets a Node dev drive open/close/list over the socket with no other knowledge.

---

## 3. Release & distribution  `[P0]`

Make `beamd` installable as a binary (ngrok-style) **and** as an npm
package (for `npx` and for Flow to bundle), all from one tagged build.

- [ ] Add a GitHub Actions workflow that on `git tag vX.Y.Z` runs `goreleaser release --clean` (needs `GITHUB_TOKEN` + GHCR login).
- [ ] Verify `goreleaser release --snapshot --clean` locally produces tarballs + checksums + the docker image for all 4 platforms (one `beamd` binary per platform — see §1).
- [ ] **npm packaging (esbuild pattern).** Add a `npm/` dir + a publish script that, after goreleaser, generates and publishes:
  - per-platform packages `beamd-{darwin-arm64,darwin-x64,linux-x64,linux-arm64}` (scope under `@beamd/` or `@dynamismlabs/` if the bare names are taken) — each `package.json` sets `"os"` + `"cpu"` and ships the matching `beamd` binary in `bin/`.
  - a main package **`beamd`** listing those as `optionalDependencies`, with a `bin/beamd` JS shim that resolves `require.resolve('beamd-<os>-<arch>/bin/beamd')` and `execFileSync`s it (passing through argv/stdio/exit code).
- [ ] Wire npm publish into the release workflow (`NPM_TOKEN`).
- [ ] Tag **v0.1.0**.
- **Acceptance:** after release, on macOS arm64: `npx beamd@0.1.0 version` prints the version; `npm i beamd` in a fresh project installs only the matching platform package (~20MB) and `require.resolve('beamd/bin/beamd')` resolves; `docker pull ghcr.io/dynamismlabs/beamd:0.1.0` works.

---

## 4. Agent-as-a-service (optional)  `[P2]`

> **Mostly optional now.** With foreground-default `beamd open`, humans don't
> need the agent at all, and the detached/automation path auto-spawns it
> (`ensureAgent`). For the always-on consumer (Flow on a Mac Mini), the
> thing that must survive reboots is **Flow's own service** — Flow then
> re-establishes its tunnels lazily on demand (see the Flow spec; the
> agent never persists registrations). Ship the items below only for
> users who want *detached* tunnels to persist across reboots without a
> consuming app running.

- [ ] Ship `dist/launchd/com.dynamismlabs.beamd.plist` template that runs `beamd agent` (KeepAlive, RunAtLoad, logs to `~/.beamd/agent.log`).
- [ ] Ship `dist/systemd/beamd.service` (user unit) for Linux hosts.
- [ ] Add `beamd service install` / `beamd service uninstall` that writes/loads the right unit for the OS (otherwise document manual install).
- [ ] Document in `docs/running-the-client.md`: install service, where logs live, how to rotate the token.
- **Acceptance:** after `beamd service install` (or manual load), rebooting the host brings the agent back; note clearly that registrations are **not** persisted — whoever owned the tunnels must re-`open` them.

---

## 5. `beamd run` — wrap-and-up convenience  `[P2, optional]`

Mirror the loved Portless ergonomic `portless <name> <cmd>` so `beamd` is a
great **standalone** tool, not only an SDK.

- [x] Add `beamd run <name> -- <command...>`: pick a free local port, set `PORT=<port>` (and `--port` passthrough convention) in the child env, spawn the command, wait until the port is listening, then bring it up foreground (`beamd open <port> --as <name>`); stream child stdio; on Ctrl-C/child-exit, tear down the tunnel and kill the child. (This is the `portless <name> <cmd>` ergonomic.)
- [x] Print the URL once ready (respect `--json`).
- **Acceptance:** `beamd run myapp -- npx serve .` brings up `https://myapp.<slug>.<base>` serving the directory, and cleans up the tunnel on exit.

---

## 6. Preview link auth  `[P1]`  (spec already written)

Implement `docs/preview-auth-spec.md` (signed-URL → edge-set cookie). Gate
public tunnel URLs so only holders of a freshly-minted link can view,
while keeping browser + iframe UX seamless.

- [ ] Add config block `preview_auth { enabled, secret_env, cookie_name, default_ttl }` to `internal/config/server.go` (+ env overrides).
- [ ] `internal/preview/authlink.go`: `Sign(secret, host, ttl) string`, `Verify(secret, host, token) (ok bool, exp int64)` using HMAC-SHA256.
- [ ] Edge middleware before host→proxy dispatch in `internal/edge/edge.go`: exempt `/healthz`,`/metrics`,`/.well-known/*`; cookie valid → pass; else valid `?__beam=` param → set signed `HttpOnly; Secure; SameSite=None` host-scoped cookie + 302 to clean URL; else `401` with a small HTML page.
- [ ] When `preview_auth.enabled=false`, behavior is unchanged (fully public).
- **Acceptance:** with auth enabled, hitting a tunnel URL with no token → 401; with a valid signed link → loads and sets the cookie; sub-resources and WebSocket/HMR work after the cookie is set; an expired/forged token → 401.

---

## 7. Embedding — strip frame-busting headers  `[P1]`

Tunnel URLs are iframed inside the consumer app; apps that send
`X-Frame-Options`/restrictive CSP won't embed.

- [x] Add config `preview_embed: bool` (or fold into `preview_auth`).
- [x] In the reverse proxy (`internal/edge/edge.go: proxyFor`) add a `ModifyResponse` that, for tunnel hosts when enabled, deletes `X-Frame-Options` and removes/relaxes `Content-Security-Policy: frame-ancestors`.
- **Acceptance:** an app that sets `X-Frame-Options: DENY` still embeds in an iframe on a different origin when `preview_embed=true`.

---

## 8. Docs  `[P1]`

- [ ] `docs/agent-api.md` (from §2), `docs/running-the-client.md` (from §4).
- [ ] Update `README.md` install section to the published npm + binary + image once §3 lands (remove "coming soon"); reflect the single `beamd` binary (`beamd serve` / `beamd open`).
- [ ] Add a short `docs/consuming-beamd.md`: how an external app should drive beamd (bundle the `beamd` npm pkg → write `~/.beamd/config` → `beamd open <port> --as <name> -d --json` → tear down with `beamd close <name>` → re-establish lazily on demand), the one-label naming rule, and the tunnel-cap setting.
- **Acceptance:** a developer can integrate beamd into a Node app using only `docs/consuming-beamd.md` + `docs/agent-api.md`.

---

## 9. Deferred — hosted / multi-tenant  `[P3, not now]`

For a future hosted offering (same client, different server). Listed for
completeness; do not build yet.

- [ ] Multi-tenant provisioning API: authenticated endpoint to create a slug + token (wraps `add-developer`) so tenants self-serve.
- [ ] Device-code login endpoints behind `/.well-known/beam-auth` (the client already supports the no-`--token` flow).
- [ ] Per-tenant quotas + usage reporting (the `usage_reporter` webhook block already exists). **Groundwork done:** the edge counts per-slug **and per-tunnel** bytes in/out (incl. WebSocket) behind a `TrafficRecorder` seam — `Edge.AddTrafficSink(r)` lets a hosted, account-aware recorder receive every `(slug, name, in, out)` delta without touching the proxy. Self-hosted uses the built-in in-memory+persisted store (`data_dir/bandwidth.json`, `/metrics`).
- [ ] Abuse controls: per-IP rate limiting, body caps (body cap exists).
- [ ] **Custom domains** (a tenant serves previews on `preview.acme.com`). The core architecture already allows this — routing is keyed by `Host` with the slug **attached to the route**, never parsed from the hostname, and cert issuance is behind the `certs.Manager` interface. The new pieces are hosted/cert-layer only: (a) **on-demand TLS** via HTTP-01 or TLS-ALPN-01 — the edge can't DNS-01 a zone it doesn't control — gated by an "is this a verified custom domain?" decision fn; (b) a **hostname → tenant/tunnel** map provisioned by the control plane; (c) **ownership verification** (the CNAME-to-edge itself, or a TXT record). **Guardrail for all current work:** never derive slug/tenant by parsing the hostname. **Synergy:** a custom domain under the tenant's *own* registrable domain makes the §6 preview-auth cookie *first-party*, which sidesteps the Safari/iOS third-party-cookie caveat entirely.

---

## Suggested order
1 → 2 → 3 (unblocks the consumer) → 6 + 7 (unblocks embedding/sharing) → 4 → 5 → 8 → (9 later).
