# Agent local API (v1)

`beamd`'s **detached** tunnels are owned by a background process — the
**agent** (`beamd agent`). The agent holds the long-lived connection to
the edge and exposes a small HTTP API over a **unix domain socket** so
the CLI and other local programs can drive it. This is the same API the
`beamd open -d` / `close` / `list` / `status` commands use under the hood.

> Most callers should just shell out to the CLI (`beamd open <port> --as
> <name> -d --json`, `beamd close <name> --json`, …) — it spawns the agent
> on demand and prints clean JSON. Talk to this socket directly only when
> you want to avoid the process hop or need an endpoint the CLI doesn't
> surface. **Treat the shapes below as a stable v1 contract.**

## Socket

| | |
|---|---|
| Path | `~/.beamd/agent.sock` (override the dir by running the agent with `--socket <path>`) |
| Permissions | `0600`, owned by the user — filesystem permissions are the only access control; there is no in-band auth |
| Transport | plain HTTP/1.1 over the unix socket (no TLS) |

**Lifecycle.** The agent is spawned on demand the first time you run
`beamd open <port> -d` (it reads `~/.beamd/config` for the server + token).
It is *not* auto-started by the read-only commands (`list`, `close`,
`status`) or by connecting to the socket directly — if no agent is
running, there are simply no detached tunnels. To guarantee one exists,
run `beamd open <port> --as <name> -d` once, or run `beamd agent` as a
service (see `docs/running-the-client.md`).

Foreground tunnels (`beamd open` without `-d`) live in their own process
and are **not** visible through this API.

## Endpoints

### `POST /open`
Bring a local port up as a public URL (idempotent per name). This is
what `beamd open <port> -d` calls.

Request:
```json
{ "port": 3000, "name": "api" }
```
`name` is optional; when omitted the edge derives it from the port (the
decimal port number).

Response `200`:
```json
{
  "url": "https://api.turing.tunnel.dynami.sm",
  "name": "api",
  "port": 3000,
  "slug": "turing",
  "baseDomain": "tunnel.dynami.sm"
}
```
Errors: `400` (bad JSON / port out of range), `502` (edge rejected the
register, e.g. `name_taken` or `over_limit`). See the error shape below.

### `POST /close`
Tear down a tunnel by name. Idempotent. This is what `beamd close <name>`
calls.

Request:
```json
{ "name": "api" }
```
Response `200`:
```json
{ "removed": true }
```
`removed` is `false` when no tunnel by that name was present (still a
success). Errors: `400` (missing `name`).

### `GET /list`
List the agent's detached tunnels.

Response `200`:
```json
[
  { "name": "api", "port": 3000, "url": "https://api.turing.tunnel.dynami.sm", "healthy": true }
]
```
`healthy` reflects whether the agent currently has a live session to the
edge.

### `GET /healthz`
Agent + connection status.

Response `200`:
```json
{
  "status": "ok",
  "slug": "turing",
  "healthy": true,
  "transport": "quic",
  "configuredTransport": "auto",
  "fallbackCount": 0,
  "reconnectCount": 1
}
```

`transport` is omitted while disconnected. `configuredTransport` is always
`auto`, `quic`, or `tcp`. `fallbackCount` and `reconnectCount` are monotonic
for the current agent process. When present, `lastFallbackReason` is one of
`network`, `timeout`, or `handshake`; `lastCloseReason` is a fixed diagnostic
category rather than a raw error. Do not build automation around log text when
these fields are available.

## Error shape

Any non-`200` response carries:
```json
{ "error": "human-readable message" }
```

## Node example

```js
const http = require("http");
const os = require("os");
const path = require("path");

const socketPath = path.join(os.homedir(), ".beamd", "agent.sock");

function call(method, urlPath, body) {
  return new Promise((resolve, reject) => {
    const data = body ? JSON.stringify(body) : null;
    const req = http.request(
      {
        socketPath,
        method,
        path: urlPath,
        headers: data
          ? { "content-type": "application/json", "content-length": Buffer.byteLength(data) }
          : {},
      },
      (res) => {
        let buf = "";
        res.on("data", (c) => (buf += c));
        res.on("end", () => {
          const parsed = buf ? JSON.parse(buf) : null;
          if (res.statusCode >= 400) reject(new Error(parsed?.error ?? `HTTP ${res.statusCode}`));
          else resolve(parsed);
        });
      }
    );
    req.on("error", reject);
    if (data) req.write(data);
    req.end();
  });
}

// Ensure the agent exists first (spawns it, returns immediately):
//   $ beamd open 3000 --as api -d --json
const tunnel = await call("POST", "/open", { port: 3000, name: "api" });
console.log(tunnel.url); // https://api.turing.tunnel.dynami.sm

const tunnels = await call("GET", "/list");
await call("POST", "/close", { name: "api" });
```
