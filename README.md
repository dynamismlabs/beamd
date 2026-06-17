# Beamd

Self-hostable, instant-URL tunnel for multi-app dev. One command turns a
locally-running app into a stable HTTPS URL on your own domain — a distinct
subdomain per app. Built for the AI-agent workflow: agent runs `npm run dev`,
calls one tool, gets back a working URL.

```
$ beamd open 3001 --as api
https://api.beam.example.com
```

By default tunnels live at `<name>.<base>`. On a shared edge you can
*namespace* each developer under their own `<name>.<slug>.<base>` zone — see
[Onboarding](#5-onboard-a-developer); it's opt-in, not required.

## Status

Launched and generally available. See [`prd.md`](prd.md) for the spec and
[`TASKS.md`](TASKS.md) for the implementation checklist + open deferred work.

## How it works (operator view)

```
[ Internet ]
     │
     ▼  :443 (TLS)
[ beamd ]  ─── ACME DNS-01 ──▶  [ DNS provider (e.g. Cloudflare) ]
     ▲
     │  one TLS conn per developer
     │  (ALPN "beam/1", yamux-multiplexed)
     ▼
[ beamd client (+ background agent) ]
     │
     │  loopback
     ▼
[ developer's local apps :3001, :3002, ... ]
```

- You run **one** `beamd` process on a server with a public IP.
- You point a domain (e.g. `beam.example.com`) at it.
- You hand out tokens. By default a token is **flat** — its tunnels live at
  `<name>.beam.example.com`, and beamd issues `*.beam.example.com` from
  Let's Encrypt (DNS-01 against your provider).
- The developer runs `beamd open 3001 --as api` and gets
  `api.beam.example.com` routed to their laptop's port 3001.
- **Optional — multi-tenant:** give a token a *slug* (`beamd add-developer
  --slug turing`) and that developer's tunnels are namespaced under
  `*.turing.beam.example.com` instead, so untrusted developers can't collide
  on names. Most self-hosters don't need this.

## Quickstart (operator)

> **For a step-by-step walkthrough from "I have nothing" to "my tunnel
> works in the browser",** see [`docs/setup.md`](docs/setup.md). It
> covers domain registration, Cloudflare setup, server provisioning,
> tokens, and verification at every step.

This section is the condensed version, assuming you're already
comfortable with Linux/Cloudflare/DNS.

The walkthrough uses **Cloudflare** as the DNS provider and
**Let's Encrypt** for certificates. Other libdns providers slot in the
same way (PRs welcome — see `internal/dns/dns.go`).

### 1. DNS setup

In Cloudflare:

1. Add your domain as a zone (e.g. `example.com`). Your `base_domain`
   can be that apex (`example.com`) **or any subdomain of it**
   (`beam.example.com`, `tunnel.dynami.sm`, …) — beamd auto-detects
   which Cloudflare zone owns `base_domain`, so you don't need a
   dedicated domain.
2. Create an **API Token** with permission `Zone → DNS → Edit` scoped
   to that zone. Copy the token.
3. Create one A record pointing `base_domain` at the server:
   `beam.example.com  A  <your beamd server IP>` (use name `@` if
   `base_domain` is the zone apex). Keep it **DNS only** (gray cloud),
   not proxied.

Tunnel DNS is created automatically by `beamd provision-dev`, relative to the
detected zone (so subdomain `base_domain`s work): `*.beam.example.com` for a
flat edge, or `*.<slug>.beam.example.com` per developer when you namespace.

### 2. Install

`beamd` is one binary — same one for the edge (`beamd serve`) and the client
(`beamd open`). Pick whichever install fits:

```
# npm (no Go toolchain needed; great for bundling into a Node app)
npm i -g @beamd/cli     # installs the `beamd` command; or ad-hoc: npx @beamd/cli <cmd>

# Docker (for running the edge)
docker pull ghcr.io/dynamismlabs/beamd:latest

# Prebuilt binaries: https://github.com/dynamismlabs/beamd/releases
```

Or build from source (needs Go 1.25+):

```
git clone https://github.com/dynamismlabs/beamd && cd beamd
make build          # → bin/beamd, bin/beam-testapp
```

### 3. Configure

Copy [`example/beamd.yaml`](example/beamd.yaml) to
`/etc/beamd/beamd.yaml` and edit:

```yaml
base_domain: beam.example.com
edge_ipv4: 203.0.113.10         # this server's public IPv4
listen_https: ":443"
acme_email: ops@example.com
dns_provider: cloudflare
dns_provider_creds: ""          # better: leave blank, set via env var
token_store: "file:/etc/beamd/tokens.json"
data_dir: /var/lib/beamd
```

Set the Cloudflare token in the environment instead of writing it to
disk in the YAML:

```
BEAMD_DNS_PROVIDER_CREDS=<your-Cloudflare-API-token>
```

Create `tokens.json` — one entry per token, mapping it to a **slug**. An
empty slug `""` is flat (tunnels at `<name>.<base>`); a non-empty slug
namespaces that token under `<name>.<slug>.<base>`:

```json
{
  "<long random token>": "",
  "<another random token>": "turing"
}
```

(Or skip the manual file and use `beamd add-developer` — it mints the token
and provisions DNS + cert in one step. See [Onboarding](#5-onboard-a-developer).)

### 4. Run

```
sudo beamd serve --config /etc/beamd/beamd.yaml
```

`:443` needs root or `CAP_NET_BIND_SERVICE`. For a non-root install use
`setcap cap_net_bind_service=+ep /usr/local/bin/beamd`.

### 5. Onboard a developer

**Flat (default)** — tunnels at `<name>.beam.example.com`:

```
beamd provision-dev --config /etc/beamd/beamd.yaml
```

Writes `*.beam.example.com  A  203.0.113.10` to your DNS provider and
pre-warms the `*.beam.example.com` cert (Let's Encrypt, DNS-01). Hand the
developer a token from `tokens.json` (one mapped to `""`).

**Namespaced (opt-in)** — give each developer their own zone, so untrusted
developers can't collide on names:

```
beamd provision-dev --slug turing --config /etc/beamd/beamd.yaml
```

Writes `turing.beam.example.com` + `*.turing.beam.example.com` and pre-warms
`*.turing.beam.example.com`. That developer's token must map to `turing`.

> Prefer one command? `beamd add-developer [--slug <name>]` mints the token,
> writes it to `tokens.json`, and provisions DNS + cert together.

## Quickstart (developer)

```
beamd login --server beam.example.com --token <token-from-operator>
beamd open 3001 --as api
# → https://api.beam.example.com
#   (runs in the foreground; Ctrl-C to stop — like ngrok)
```

By default `open` runs in the **foreground** and holds the tunnel until you
Ctrl-C. Add **`-d` / `--detach`** to hand it to a background agent and
return immediately — then `beamd list`, `beamd close <name>`, and `beamd
status` manage the detached tunnels. Either way the tunnel survives
network blips: the client reconnects and replays your registrations.

Add `--json` to `open` / `list` / `close` / `status` for machine-readable
output (one object/array, nothing else) — see
[`docs/agent-api.md`](docs/agent-api.md) for driving beamd from another
program.

### Naming the tunnel

The subdomain label defaults to the port number. Set it explicitly, or
derive it from your project:

```
beamd open 3001 --as api      # literal label  → api.<base>
beamd open 3001 --from dir     # derive it      → <cwd-name>.<base>
```

`--from` sources: `port` (default), `dir` (folder name), `repo` (git repo
name), `branch` (current git branch), `worktree` (`<branch>-<repo>` when in a
linked git worktree on a non-`main`/`master` branch, else the repo name).
`--as` (literal) wins if you pass both.

### Accounts — many edges, no logout churn

Stay logged into several edges at once; each is an **account**, keyed by its
server (the kubectl / `gh auth` model):

```
beamd login                     # hosted: opens a browser; edge + orgs assigned automatically
beamd login --server acme.com   --token <T>   # self-host / OSS: pass the edge + token
beamd accounts                  # list them; * marks the current
beamd open 3001 --server acme.com   # one-off override (any command takes --server)
beamd logout --server acme.com  # remove one
```

On the **hosted** service a bare `beamd login` does browser login and the edge
is assigned for you — `--server` is the **self-host opt-out**. (This repo's OSS
binary has no built-in service, so it always takes `--server`.) The first
account becomes current; `BEAMD_SERVER=<edge>` overrides per-shell.

### Scope (org) — one login, many orgs

On a hosted edge you belong to one or more **scopes** — your personal namespace
plus any teams. Scope is a *selector*, not a separate login: pick it per
command, per project, or set a standing default.

```
beamd orgs                      # list the orgs your account can act in
beamd default acme              # your standing default scope (personal until set)
beamd open 3001 --scope beta    # one-off override
beamd whoami                    # show the resolved account + scope
```

Precedence: `--scope` > a project `beamd.yaml` `scope:` > `beamd default` > personal.
A self-hosted OSS edge has no scopes — your token fixes the namespace.

### Project config (`beamd.yaml`)

Pin the right edge + scope + name to a repo so `open` / `run` need zero flags.
Create it with `beamd link` (interactive, like `vercel link`), or by hand:

```yaml
# beamd.yaml
server: acme.com   # which edge (account)
scope: acme        # which org (hosted; omit for an OSS edge)
from: repo         # how to name the tunnel (or `name: <literal>`)
```

beamd walks up from the cwd to find it; a gitignored `beamd.local.yaml`
overrides it (like `.env` / `.env.local`). A `beamd.yaml` references an edge +
scope — **never a token** — so it's safe to commit.

### Wrap a command (`beamd run`)

Run a dev server and expose it in one step — picks a free port, sets `$PORT`,
waits for it to listen, opens the tunnel, and tears everything (tunnel +
process tree) down on exit:

```
beamd run -- npm run dev       # name from beamd.yaml / --from / port
beamd run api -- npm run dev    # explicit name
```

`run` resolves the edge + scope + name exactly like `open` (accounts, `beamd.yaml`,
`--as` / `--from`) and makes **any** framework reachable through the tunnel:
it sets `$BEAMD_URL` (the public URL, for OAuth callbacks / absolute links),
injects the allowed-host env so Vite/Next don't reject the tunnel domain,
adds `--port` / `--host` for frameworks that ignore `$PORT` (Vite, Astro,
Angular, …), and resolves project-local binaries via `node_modules/.bin`.

### MCP server (AI agents)

The same agent also exposes an MCP server over stdio:

```
beamd mcp
```

Wire that into your MCP-aware agent (Claude Code, Cursor, etc.) and
the agent gets three tools:

- `expose_port(port, name?)` → `https://...`  (= `beamd open`)
- `remove_tunnel(name)`  (= `beamd close`)
- `list_tunnels()`  (= `beamd list`)

## Configuration reference

Every field in `beamd.yaml` can be overridden by the matching
`BEAMD_<UPPER_SNAKE_CASE>` env var (e.g. `BEAMD_DNS_PROVIDER_CREDS`).

| Field | Required | Notes |
|---|---|---|
| `base_domain` | yes | e.g. `beam.example.com` |
| `edge_ipv4` | yes for `provision-dev` | Public IPv4 this beamd is reachable at |
| `edge_ipv6` | no | Optional IPv6 target |
| `listen_https` | yes | Public ingress + ALPN-demuxed client control. `:443` in prod, `:8443` in dev |
| `acme_email` | yes | Contact address registered with Let's Encrypt |
| `acme_ca` | no | ACME directory URL. Blank = LE prod. `off` = self-signed (dev only) |
| `dns_provider` | yes | One of: `cloudflare`, `stub` (more on the way) |
| `dns_provider_creds` | provider-specific | Cloudflare: `Zone:DNS:Edit` API token |
| `dns_zone` | no | Registered zone to write records in. Blank = auto-detect from `base_domain` (recommended). Set to skip the lookup, e.g. `dynami.sm` when `base_domain` is `tunnel.dynami.sm` |
| `token_store` | yes | `file:<path>` (JSON `{token: slug}` map), or `memory:` for tests |
| `data_dir` | defaults to `/var/lib/beamd` | Where beamd persists state — cert cache, ACME account, and per-tunnel bandwidth totals (`bandwidth.json`) |
| `max_tunnels_per_token` | defaults to 25 | Cap on concurrent tunnels per developer |
| `max_request_body_bytes` | defaults to 32 MiB (`33554432`) | Per-request public body cap; oversized requests get HTTP 413. Set `-1` to disable |
| `preview_embed` | defaults to false | Strip `X-Frame-Options` + CSP `frame-ancestors` from tunnel responses so previews embed cross-origin in an iframe |

## DNS providers

The cert layer uses [libdns] under the hood for DNS-01 ACME
challenges. The OSS binary currently compiles in:

- `cloudflare` (reference)
- `stub` (in-memory, for tests / dev)

Adding more (Route53, DigitalOcean, Hetzner, GCloud DNS, Gandi) is one
import + one switch case in `internal/dns/dns.go` — PRs welcome.

[libdns]: https://github.com/libdns

## Build / develop

```
make build         # produces bin/beamd, bin/beam-testapp
make test          # runs all unit + e2e tests
make run-server    # runs beamd against example/beamd.yaml
make smoke-test    # spins up beam-testapp + drives it through your tunnel
```

Cutting a release (npm + binaries + Docker + `go install`): see
[`docs/releasing.md`](docs/releasing.md).

## Smoke-testing a real deployment

After [setup](docs/setup.md), `make smoke-test` exercises the proxy path
end-to-end against your live edge: header forwarding, body
round-tripping, response-size correctness, latency tolerance. One-line
pass/fail per check, cleanup on exit.

See [`docs/post-manual-testing.md`](docs/post-manual-testing.md) for
the full "what to do after smoke testing passes" playbook —
release-tag workflow, the highest-value gaps left in the test suite,
and the Tier 2 / Tier 3 decision tree.

## License

Apache 2.0.
