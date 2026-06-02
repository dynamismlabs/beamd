# Driving beamd from another app

This is for building beamd into a host app (e.g. an AI task manager that
exposes each agent's dev server for remote review). It covers bundling the
binary, configuring it, and the bring-up / tear-down lifecycle. For the raw
socket API the CLI talks to, see [`agent-api.md`](agent-api.md).

## Mental model

- One **edge** (`beamd serve`) runs on a public box and owns a domain.
- Your app machine runs the **client**: `beamd open <port> -d` hands a tunnel
  to a background **agent** that holds it and returns immediately. The agent
  survives your spawn exiting; you reconnect to it later by command.
- **Your app is the source of truth** for what should be exposed. The agent
  doesn't persist registrations across reboots — on startup, re-open the
  tunnels you want (it's cheap and idempotent).

## 1. Bundle the binary

Add beamd as a dependency — npm installs only the binary for the host
platform (~4 MB):

```
npm i @beamd/cli
```

Spawn it via the package's bin (`node_modules/.bin/beamd`) or
`require.resolve("@beamd/cli/bin/beamd.cjs")`. Everything below is just
`beamd <args>`.

## 2. Configure (once)

Point the client at your edge. For a host app, the clean path is a
**dedicated config file** you pass with `--config <path>` to every command —
it bypasses beamd's account store entirely, so your app keeps its own
`{server, token}` out of the user's `$HOME` and never collides with their
interactive `beamd` accounts. The `token` here is a **workspace API key**
(hosted) or an operator-issued token (OSS) — not an interactive login session:

```yaml
# my-app-beamd.yaml — referenced via `--config my-app-beamd.yaml`
server: tunnel.example.com:443
token: <the developer token your operator issued>
agent_socket: /path/to/your-app/beamd-agent.sock  # pin a dedicated socket
# insecure_skip_verify: true   # ONLY for a self-signed dev edge
```

**Pin a dedicated `agent_socket`.** The detached agent is keyed by its socket
path; without one your app shares the default socket with the user's own
`beamd`, and an agent already running there keeps serving its *original*
credentials even after you change `server`/`token` here. A per-app socket
isolates you — and if you do rotate creds in this file, run `beamd reload
--config <path>` to restart the agent with them. (Don't set
`insecure_skip_verify` against a real edge; the cert is verified by default so
the token only rides a trusted connection.)

(Interactive users instead run `beamd login`, which saves an *account* keyed
by server under `~/.beamd/accounts/`. Automation should prefer `--config` and
stay out of that store — and skip scope selection: an API key's scope is fixed,
so `--scope` and `beamd default` don't apply to you.) By default tunnels live
at `<name>.<base_domain>`. If the
operator gave your token a **slug**, they're namespaced under
`<name>.<slug>.<base_domain>` instead — either way, read the URL from the
`open --json` output rather than assembling it yourself.

## 3. Bring a tunnel up (detached + JSON)

```
beamd open 3000 --as my-app -d --json
```

`-d` detaches (the agent keeps it alive; the command returns immediately).
`--json` prints exactly one object and nothing else:

```json
{ "url": "https://my-app.<base>", "name": "my-app", "port": 3000, "slug": "", "baseDomain": "<base>" }
```

(`slug` is `""` on a flat edge; on a namespaced one the `url` is
`https://my-app.<slug>.<base>`. Always trust `url`.)

Parse `url`, embed/open it. This is the path automation should use.

## 4. Tear down

```
beamd close my-app --json    # {"name":"my-app","removed":true|false}
```

Idempotent: exit 0 whether or not it existed. Never spawns an agent.

## 5. Reconcile on startup (lazy re-establish)

The agent isn't a persistent registry — after a reboot it isn't running and
holds nothing. On startup, reconcile against your own desired state:

```
beamd status --json   # {"agentRunning":bool,"server":...,"slug":...,"scope":...,"healthy":bool}
beamd list --json     # [{"name","url","port","healthy"}, …]
```

For anything you want exposed that isn't listed, just `beamd open … -d`
again (the first detached `open` spawns the agent automatically).

## 6. The one-label naming rule

The wildcard cert is **exactly one DNS label deep** — `*.<base>` (flat) or
`*.<slug>.<base>` (namespaced). So a tunnel name must be a single label:

- ✅ `my-app.<base>`, `proj-ws-api.<base>` (or `my-app.<slug>.<base>`)
- ❌ `a.b.<base>` (nested labels don't get a cert and won't resolve)

Every `--as <name>` must be a single RFC 1123 label (lowercase
alphanumeric + hyphens). Encode structure with hyphens, not dots — e.g. a
worktree's API + web as `proj-ab12-api` and `proj-ab12-web`.

## 7. Tunnel cap

The edge caps concurrent tunnels per token via `max_tunnels_per_token`
(server config, default 25). If you expose many short-lived previews,
`close` the ones you no longer need so you don't hit the cap.
