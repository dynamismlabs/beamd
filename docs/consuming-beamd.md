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
npm i beamd
```

Spawn it via the package's bin (`node_modules/.bin/beamd`) or
`require.resolve("beamd/bin/beamd.cjs")`. Everything below is just
`beamd <args>`.

## 2. Configure (once)

Point the client at your edge. For a host app, the clean path is a
**dedicated config file** you pass with `--config <path>` to every command —
it bypasses beamd's profile store entirely, so your app keeps its own
`{server, token}` out of the user's `$HOME` and never collides with their
interactive `beamd` profiles:

```yaml
# my-app-beamd.yaml — referenced via `--config my-app-beamd.yaml`
server: tunnel.example.com:443
token: <the developer token your operator issued>
```

(Interactive users instead run `beamd login`, which saves a named *profile*
under `~/.beamd/profiles/`. Automation should prefer `--config` and stay out
of that store.) The token maps to a **slug**; all of this app's tunnels live
under `*.<slug>.<base_domain>`.

## 3. Bring a tunnel up (detached + JSON)

```
beamd open 3000 --as my-app -d --json
```

`-d` detaches (the agent keeps it alive; the command returns immediately).
`--json` prints exactly one object and nothing else:

```json
{ "url": "https://my-app.<slug>.<base>", "name": "my-app", "port": 3000, "slug": "<slug>", "baseDomain": "<base>" }
```

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
beamd status --json   # {"profile":...,"agentRunning":bool,"server":...,"slug":...,"healthy":bool}
beamd list --json     # [{"name","url","port","healthy"}, …]
```

For anything you want exposed that isn't listed, just `beamd open … -d`
again (the first detached `open` spawns the agent automatically).

## 6. The one-label naming rule

The per-developer wildcard cert covers `*.<slug>.<base>` — **exactly one DNS
label deep**. So:

- ✅ `my-app.<slug>.<base>`, `proj-ws-api.<slug>.<base>`
- ❌ `a.b.<slug>.<base>` (nested labels don't get a cert and won't resolve)

Every `--as <name>` must be a single RFC 1123 label (lowercase
alphanumeric + hyphens). Encode structure with hyphens, not dots — e.g. a
worktree's API + web as `proj-ab12-api` and `proj-ab12-web`.

## 7. Tunnel cap

The edge caps concurrent tunnels per token via `max_tunnels_per_token`
(server config, default 25). If you expose many short-lived previews,
`close` the ones you no longer need so you don't hit the cap.
