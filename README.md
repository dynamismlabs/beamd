# Beamd

Self-hostable, instant-URL tunnel for multi-app dev. One command turns a
locally-running app into a stable HTTPS URL on your own domain, with a
distinct subdomain per app under a per-developer wildcard zone. Built
for the AI-agent workflow: agent runs `npm run dev`, calls one tool,
gets back a working URL.

```
$ beamd up 3001 --as api
https://api.turing.beam.example.com
```

## Status

Pre-alpha. See [`prd.md`](prd.md) for the spec and [`TASKS.md`](TASKS.md)
for the implementation checklist + open deferred work.

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
- You give each developer a token. The token maps to a slug (`turing`,
  `hopper`, …).
- The developer runs `beamd up 3001 --as api` on their laptop.
  Beamd issues `*.turing.beam.example.com` from Let's Encrypt
  (via DNS-01 against your DNS provider), routes
  `api.turing.beam.example.com` to their laptop's port 3001.

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

Per-developer DNS (`*.turing.beam.example.com` + `turing.beam.example.com`)
is created automatically by `beamd provision-dev` — relative to the
detected zone, so subdomain `base_domain`s work.

### 2. Install

Not published yet — **build from source** (needs Go 1.25+):

```
git clone https://github.com/dynamismlabs/beamd && cd beamd
make build          # → bin/beamd, bin/beam-testapp
docker build -t ghcr.io/dynamismlabs/beamd:latest .   # if you run via Docker
```

Prebuilt binaries (releases page) and a published Docker image
(`ghcr.io/dynamismlabs/beamd:latest`) are **coming soon**.

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

Create `tokens.json` with one entry per developer:

```json
{
  "<long random token>": "turing",
  "<another long random token>": "hopper"
}
```

### 4. Run

```
sudo beamd serve --config /etc/beamd/beamd.yaml
```

`:443` needs root or `CAP_NET_BIND_SERVICE`. For a non-root install use
`setcap cap_net_bind_service=+ep /usr/local/bin/beamd`.

### 5. Onboard a developer

For each developer slug:

```
beamd provision-dev --slug turing --config /etc/beamd/beamd.yaml
```

This:

- Writes `turing.beam.example.com  A  203.0.113.10` and
  `*.turing.beam.example.com  A  203.0.113.10` to your DNS provider.
- Pre-warms the `*.turing.beam.example.com` certificate (issues from
  Let's Encrypt via DNS-01).

Hand the developer their token (the long random string from
`tokens.json`).

## Quickstart (developer)

```
beamd login --server beam.example.com:443 --token <token-from-operator>
beamd up 3001 --as api
# → https://api.turing.beam.example.com
```

The background agent stays running; subsequent `up` / `list` / `down`
calls reuse it. Tunnels survive network blips — the client reconnects
automatically and replays your registrations.

### MCP server (AI agents)

The same agent also exposes an MCP server over stdio:

```
beamd mcp
```

Wire that into your MCP-aware agent (Claude Code, Cursor, etc.) and
the agent gets three tools:

- `expose_port(port, name?)` → `https://...`
- `unexpose(name)`
- `list_tunnels()`

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
| `data_dir` | defaults to `/var/lib/beamd` | Where cert cache + ACME account state live |
| `max_tunnels_per_token` | defaults to 25 | Cap on concurrent tunnels per developer |

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
