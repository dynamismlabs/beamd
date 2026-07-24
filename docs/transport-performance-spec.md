# Beamd Transport Performance — Specification and Task Checklist

**Status:** A1 implemented (env-only window; pending production validation A1.8). Phased overall — QUIC/A2 opt-in, default-off until proven.

**Owner:** Dynamism

**Last updated:** 2026-07-23

**Scope:** `beamd` edge, Go client/agent, shared transport code, packaging, deployment, tests, and observability

This is the single canonical implementation document for A1 and A2. Section
16 is the ordered, executable task checklist; the surrounding sections define
its requirements and acceptance criteria. Check an item only when its
referenced requirements and tests pass. `TASKS.md` links here but does not
duplicate this checklist.

## 1. Executive decision

This work ships in two decoupled parts with a measurement gate between them. It
is not one flag-day cutover, and it is not a CLI-package-only change.

**Part A — the tuned yamux window (A1) — ships first, standalone, and by
default.** It is a small change to the existing concrete-yamux code: raise the
per-stream receive window from the 256 KiB library default to 4 MiB on both
edge and agent. The only external knob is the process environment variable
`BEAMD_YAMUX_STREAM_WINDOW_BYTES`; when absent it defaults to `4194304`. It
does **not** add YAML/profile fields, introduce the transport abstraction,
change unrelated yamux behavior, add capacity leases, or add QUIC. This
removes the single largest, most certain defect (a solo stream capped at
`256 KiB / RTT`) with a small, bounded change and makes the eventual TCP path
good.

**Part B — QUIC over UDP 443 — is built later, behind a default-off flag, and
becomes the default only after it beats the tuned-yamux baseline in the
deterministic impairment harness and succeeds in a real production-link
pilot.** Building QUIC is the large effort (a transport abstraction, both
adapters, listener/key lifecycle, selection/fallback, capacity, and a netem
qualification harness). Turning the flag *off* does not make that work cheap;
the flag buys a safe default and operational reversibility, not less code.
QUIC ships as opt-in (`transport: auto` on the agent, QUIC listener enabled on
the edge); forced `transport: quic` is for qualification and diagnosis. The
edge-wide rollback is `BEAMD_DISABLE_QUIC=true` plus an edge restart, after
which `auto` agents reconnect over TCP. The abstraction layer is introduced
only when QUIC is greenlit — it has no value while there is a single
implementation.

Between the two parts, **prove A2 on the real tunnel** (Section 16): with the
4 MiB window already live, measure both the small-response timeout signature
and large solo transfers on a lossy / high-RTT link. If they do not collapse,
Part B may not be needed at all.

The npm package is a launcher for the compiled Go `beamd` binary; no JavaScript
transport implementation is needed in either part.

The **eventual** full scope, once Part B is greenlit and proven, spans:

| Surface | Change | Part |
| --- | --- | --- |
| Shared Go transport | Tune the yamux window | **A (now)** |
| Shared Go transport | Add a transport-neutral session interface and add QUIC | B |
| Client/agent | Prefer QUIC (once proven), fall back to TCP/yamux, report the selected transport | B |
| Edge server | Listen on UDP 443 for QUIC, retain TCP 443 for public HTTPS and fallback | B |
| Control protocol | Enforce the existing protocol version; do not otherwise change the wire messages | B |
| Process config | `BEAMD_YAMUX_STREAM_WINDOW_BYTES` with a 4 MiB default (A); transport selection, QUIC address, capacity settings (B) | A + B |
| Metrics/logging | Effective yamux window startup log (A); transport selection, yamux gauge, fallback, capacity rejection (B) | A + B |
| Deployment | Publish UDP 443, raise host UDP buffers, set a memory limit | B |
| npm/release | Rebuild and ship the Go binary | A + B |

The **target end-state** production design (reached only after Part B passes
its gates) is:

1. Raw QUIC over UDP 443 is the preferred client-to-edge transport.
2. TLS + yamux over TCP 443 remains an automatic availability fallback for
   networks that block or impair UDP, carrying the 4 MiB window from Part A.
3. The edge admits at most 64 active proxy streams per session and 128
   globally. This bounds active receive-flow-control exposure; it is not a
   claim that all window credit is preallocated resident memory.
4. The edge admits at most 32 pre-authentication sessions and eight
   authenticated sessions.
5. Until QUIC is proven, the shipped default is TCP/yamux only; QUIC is opt-in.
   The default flips to QUIC-preferred (`auto`) only after the deterministic
   Section 15.3 gates and the real-link production pilot both pass.
6. When both transports ship, the client and edge are upgraded together. There
   is no mixed-version compatibility period, rollout cohort, or legacy protocol
   adapter.

The capacity leases, pre-auth/auth session limits, and per-scope metrics in
this document are part of **Part B** (and hosted-mode hardening), not Part A.
For the single-user OSS path, Part A intentionally ships without them; Part B
adds the global concurrent-stream cap before QUIC can be enabled.

Keeping TCP fallback is not legacy support. It is protection against networks
where UDP is unavailable — and, for a laptop agent on arbitrary networks that
throttle UDP, it may be the *common* path, not a degraded one. Its correctness
is required; the loss-heavy performance target applies to QUIC.

## 2. Why this design

Two distinct defects were measured:

- **A1:** yamux's default `MaxStreamWindowSize` is 256 KiB. A stream can move
  only approximately one window per effective round trip, so large solo
  transfers are window-limited.
- **A2:** all streams share one long-lived TCP connection. A solo response is
  emitted as a burst, and loss near the burst tail can fall into TCP
  retransmission timeout recovery. Parallel traffic hides the problem by
  keeping the connection ACK-clocked.

Increasing the yamux window fixes A1, but it does not fix A2. QUIC fixes the
cross-stream architectural weakness because delivery is stream-aware and
quic-go implements transport pacing without TCP head-of-line blocking between
independent streams. QUIC still has connection-level congestion control and
probe timeouts, so it is not assumed to eliminate every solo tail-loss event.
The netem gates in this specification must prove the A2 improvement before
QUIC becomes the default. The tuned yamux path is still useful as a fallback
and as an immediate improvement while the QUIC work lands.

This design does **not** add a rate limiter. Limits in this specification bound
concurrency and memory, not bytes per second.

## 3. Goals

- A single response must not be materially slower merely because it is the
  only active response.
- Transfers above 256 KiB must not plateau at `256 KiB / RTT`.
- HTTP, request bodies, streaming responses, SSE, WebSockets, and HMR must
  retain their current semantics.
- The agent must reconnect and replay intended registrations after either
  transport fails.
- A UDP-blocked network must continue to work over tuned TCP/yamux.
- Resource use must be explicitly bounded and observable.
- The same release artifact must contain the edge and agent implementation.
- Transport rollback must be available through either the edge-wide kill
  switch or the agent-local TCP override; a correctness rollback may replace
  both matching binaries.

## 4. Non-goals

- HTTP/3 for public visitor traffic
- WebTransport, QUIC datagrams, MASQUE, or 0-RTT
- Multi-region routing, active-active edge replication, or session handoff
- CDN caching or moving static assets out of the tunnel
- Multiple TCP connections intended to out-compete congestion control
- A user-configurable bandwidth throttle
- Live migration of existing sessions
- Supporting an old edge with a new agent, or an old agent with a new edge
- Perfect performance on the TCP fallback under the A2 loss pattern

## 5. Resulting data path

```text
public visitor -- HTTPS/TCP 443 --> edge
                                      |
                                      | one stream per public request/upgrade
                                      |
                         +------------+-------------+
                         | preferred: QUIC/UDP 443  |
                         | fallback: TLS+yamux/TCP  |
                         +------------+-------------+
                                      |
                                   agent
                                      |
                                localhost app
```

Public HTTPS remains exactly where it is. Only the long-lived edge-to-agent
tunnel gains a second transport.

## 6. Shared transport abstraction

Create `internal/tunnel` and migrate `internal/client` and `internal/edge` away
from concrete `*yamux.Session` and `*yamux.Stream` types.

### 6.1 Required files

```text
internal/tunnel/
  session.go
  errors.go
  yamux.go
  quic.go
  quic_stream.go
  session_test.go
  yamux_test.go
  quic_test.go
```

Delete `internal/mux` after all callers have moved. Do not leave two sources of
yamux configuration.

### 6.2 Interfaces

`internal/tunnel/session.go` must define:

```go
type Kind string
type ErrorCode uint64

const (
    KindQUIC  Kind = "quic"
    KindYamux Kind = "tcp"
)

type Stream interface {
    net.Conn
    CloseWrite() error
    Abort(ErrorCode)
    Done() <-chan struct{}
}

type CloseInfo struct {
    Code   ErrorCode
    Remote bool
    Reason string
    Cause  error
}

type Session interface {
    Kind() Kind
    OpenStream(context.Context) (Stream, error)
    AcceptStream(context.Context) (Stream, error)
    Done() <-chan struct{}
    IsClosed() bool
    CloseInfo() CloseInfo // valid after Done closes
    CloseWithError(ErrorCode, string) error
    LocalAddr() net.Addr
    RemoteAddr() net.Addr
}

type Listener interface {
    Accept(context.Context) (Session, error)
    Close() error
    Addr() net.Addr
}
```

The edge and client may depend only on these interfaces. Transport-specific
errors must be normalized with `errors.Is` against sentinels in
`internal/tunnel/errors.go`:

```go
var (
    ErrSessionClosed = errors.New("tunnel session closed")
    ErrOpenTimeout   = errors.New("tunnel stream open timeout")
    ErrCapacity      = errors.New("tunnel stream capacity reached")
)
```

Use stable QUIC application error codes:

| Code | Name | Meaning |
| ---: | --- | --- |
| `0x00` | `CloseNormal` | Normal connection close |
| `0x01` | `CloseShutdown` | Edge or agent is shutting down |
| `0x02` | `CloseProtocol` | Invalid control stream or protocol version |
| `0x03` | `CloseAuth` | Authentication was rejected |
| `0x04` | `CloseSuperseded` | Session was replaced |
| `0x05` | `CloseCapacity` | Edge session capacity was reached |
| `0x10` | `StreamCanceled` | Request, backend, or caller canceled a stream |
| `0x11` | `StreamCapacity` | Stream could not be admitted |

The yamux session adapter's `CloseWithError` ignores the application error
code and closes the whole yamux session. The yamux stream adapter's `Abort`
also ignores its code but follows the per-stream behavior in Section 6.3. Both
must normalize session-shutdown errors to `ErrSessionClosed`.

### 6.3 Stream `net.Conn` contract

Transport streams are exposed through `tunnel.Stream`, which remains usable
where the standard library expects a `net.Conn`. `quic.Stream` is not a
complete `net.Conn`: its `Close` closes only the write side.
`quic_stream.go` must supply:

- `Read`, `Write`, and all three deadline methods
- connection-level `LocalAddr` and `RemoteAddr`
- `CloseWrite()`, mapping to graceful QUIC stream `Close`
- `Abort(code)`, calling both `CancelRead(code)` and `CancelWrite(code)`
- `Close()`, performing the normal `net.Conn` close expected by
  `http.Transport`: send a graceful FIN on the write side and cancel only the
  read side
- `Done()`, closed after both directions finish or immediately after abort

The wrapper must serialize `Write`, `CloseWrite`, and the send-side portion of
`Close` with a mutex. `http.Transport` can call `Close` after receiving an
early response while its request-body writer is still blocked. In that case,
`Close` first sets an immediate write deadline without holding the mutex,
waits for the writer to return, takes the mutex, sends the graceful FIN, and
then cancels the read side. If `Abort` wins the send-side terminal operation,
it calls `CancelWrite` immediately; it always calls `CancelRead`, so either
blocked direction wakes up. A send-side `sync.Once` prevents `CloseWrite`,
`Close`, and `Abort` from issuing conflicting terminal operations.

Normal proxy completion should half-close each direction before final cleanup.
Cancellation and error paths should call `Abort`. This distinction prevents
blocked readers from leaking without discarding normally completed writes.
Never call `CancelWrite` from normal `Close`: quic-go permits it after a
graceful stream close, but doing so can discard reliable data that was queued
and not yet acknowledged.

The edge's request-context watcher must call `Abort` when a public request is
canceled before normal stream completion. Read/write error paths on either
side must also abort. Tests must distinguish normal FIN completion from reset
for response bodies, uploads, early responses, and cancellation.

The yamux stream wrapper must provide the same interface. yamux has no public
per-stream reset, so its `Abort` sends the local FIN and relies on the
configured five-second `StreamCloseTimeout` for forced cleanup. Its `Done`
closes when local close and remote EOF are both observed, or when that timeout
expires.

All close, `Done`, and capacity-release callbacks must use `sync.Once`.

One lifecycle goroutine per session owns the terminal log/metric emission
after `Session.Done` closes. It reads `CloseInfo` and classifies the result into
the fixed close-reason set. Accept loops and copy loops must not each emit a
second session-close record.

## 7. QUIC transport

Add a direct dependency on `github.com/quic-go/quic-go` pinned to `v0.60.0`.
That release requires Go 1.25, which matches this repository's `go.mod`.

This is raw QUIC carrying beamd's existing streams. Do not add `http3`.

### 7.1 TLS and ALPN

- QUIC ALPN: `beamd-quic/1`
- TCP fallback ALPN: retain `beam/1`
- QUIC TLS minimum: TLS 1.3
- Certificate source: clone the edge's existing certificate configuration and
  retain its `GetCertificate` callback
- Client SNI: derive the original hostname from `serverAddr` with
  `net.SplitHostPort` and set `tls.Config.ServerName` explicitly before
  `quic.DialAddr`; never derive SNI from a resolved UDP address
- Certificate verification: same trust behavior as the existing TLS client
- `InsecureSkipVerify`: retain for local development only
- `Allow0RTT`: `false`
- Datagrams: disabled

Do not add an application protocol version merely because the transport
changed. The existing NDJSON messages are unchanged, so `ProtoVersion`
remains `1`.

### 7.2 QUIC configuration

Use separate server and client configs because their incoming-stream roles
differ.

Common values:

```go
HandshakeIdleTimeout:           10 * time.Second
MaxIdleTimeout:                 75 * time.Second
KeepAlivePeriod:                0 // application heartbeat already runs every 20s
InitialStreamReceiveWindow:     4 << 20
MaxStreamReceiveWindow:        16 << 20
InitialConnectionReceiveWindow: 16 << 20
MaxConnectionReceiveWindow:     64 << 20
MaxIncomingUniStreams:          -1
EnableDatagrams:                false
Allow0RTT:                      false
DisablePathMTUDiscovery:        false
```

Role-specific values:

| Endpoint | `MaxIncomingStreams` | Reason |
| --- | ---: | --- |
| Edge | `1` | The agent opens exactly one bidirectional control stream |
| Agent | `64` | The edge opens at most 64 concurrent data streams |

Use `OpenStreamSync` in the QUIC adapter. The adapter derives a five-second
context with `context.WithTimeoutCause(ctx, 5*time.Second, ErrOpenTimeout)`.
If that adapter-owned timer expires, return `ErrOpenTimeout`. If the caller's
context is canceled or reaches its own earlier deadline, propagate the
caller's error unchanged. This distinction lets the reverse proxy recognize a
disconnected public requester instead of rewriting cancellation as a 502.

The 4 MiB initial per-stream window avoids recreating A1 during QUIC's
auto-tuning ramp. The 64 MiB connection maximum prevents 64 streams from each
claiming a 16 MiB connection-level commitment.

The agent should dial with `quic.DialAddr` and retain one in-memory
`quic.NewLRUTokenStore(8, 4)` for the lifetime of the `Client`, so reconnects
can reuse address-validation tokens. Do not persist client tokens and do not
use `DialAddrEarly`. `DialAddr` owns its private UDP socket and closes it when
the returned QUIC connection closes; do not pretend the caller has a separate
raw socket handle.

### 7.3 Listener and key material

The edge must:

1. Open a real `*net.UDPConn`.
2. Pass it directly to `quic.Transport` so quic-go can use ECN, batched I/O,
   GSO, and path-MTU discovery where the OS supports them.
3. Call `Transport.Listen`, not a 0-RTT listener.
4. Run an accept loop beside the existing TCP accept loop.

Bind and validate both listeners before logging readiness or entering either
accept loop. If QUIC is enabled and its UDP bind/listen fails, close the TCP
listener and fail startup. Running TCP-only is allowed only through the
explicit `disable_quic` switch. A fatal error from either accept loop must
initiate common shutdown rather than leave a half-running edge.

Move the current `ready` log out of `cmd/beamd/main.go`. `Edge.Serve` emits it
only after TCP and enabled UDP listeners are bound, key material is loaded,
and both accept loops are ready. Include both listen addresses and enabled
transports. Every successful `Accept` must re-check edge shutdown before
dispatching because a listener may still return an already-queued connection
after close.

Generate and persist these keys beneath `data_dir` with mode `0600`:

```text
quic-stateless-reset.key
quic-token-generator.key
```

Each is 32 cryptographically random bytes, written atomically. Configure
`StatelessResetKey` and `TokenGeneratorKey` from them. A missing key is
generated; a malformed key is a startup error, not silently replaced.

Accepted QUIC connections, including connections still waiting for a control
hello, must be tracked explicitly. Closing a `quic.Listener` does not close
accepted connections.

There are 32 tunnel pre-authentication slots:

- For TCP, retain the existing bounded raw TLS handshake first. Clear the raw
  TLS deadline after handshake. Only a connection that negotiated `beam/1`
  enters the tunnel pre-auth pool; ordinary public HTTPS must not consume
  these slots.
- For QUIC, `Transport.Listen` accepts only after the transport handshake, so
  acquire the slot immediately after `Accept`.

Once in the pool, the control stream must open and deliver a valid hello
within five seconds. Set a five-second control-stream deadline and clear it
after `hello_ok`. The yamux adapter closes its session if the context around
`AcceptStream` expires. Release the pre-auth slot on every failure.

After token and protocol validation, acquire one of eight authenticated
session slots without waiting, then release the pre-auth slot. If none is
available, send the existing `over_limit` control error, close with
`CloseCapacity`, and release the pre-auth slot. A successful session holds its
authenticated slot until `Session.Done`.

### 7.4 Control and data streams

The protocol is unchanged:

1. Agent establishes the transport session.
2. Agent opens the first bidirectional stream.
3. Agent sends the existing NDJSON `hello`.
4. Edge authenticates and sends `hello_ok`.
5. This stream remains the control stream.
6. For each public request, the edge opens a new bidirectional stream, writes
   `<tunnel-name>\n`, and proxies the existing HTTP bytes.

For yamux, run an unexpected-stream guard after accepting the control stream;
if the agent opens another stream, close the session as a protocol violation.
QUIC prevents a second agent-opened stream at the transport layer with
`MaxIncomingStreams: 1`.

## 8. Tuned TCP/yamux fallback

### 8.1 Part A: stream-window change only

Part A changes only the receive-window maximum. Preserve every other value in
the current `internal/mux.Config()` implementation:

```go
cfg := yamux.DefaultConfig()
cfg.KeepAliveInterval = 20 * time.Second
cfg.ConnectionWriteTimeout = 30 * time.Second
cfg.MaxStreamWindowSize = uint32(windowBytes) // default 4 << 20
cfg.LogOutput = io.Discard
```

The external contract is:

- Environment variable: `BEAMD_YAMUX_STREAM_WINDOW_BYTES`
- Compiled default when the variable is absent: `4194304` (4 MiB)
- Accepted values: base-10 byte counts from `262144` through `16777216`,
  inclusive
- A present empty, malformed, negative, overflowing, or out-of-range value is
  a startup error; never silently retain the default
- Parse once at edge or agent process startup and pass the validated value into
  `internal/mux`; do not read the environment independently for every session
- Do not add `yamux_stream_window_bytes` to server YAML, explicit client YAML,
  accounts, profiles, project configuration, or login persistence

The setting belongs to the receiver. The edge value controls response bytes
from agent to edge (downloads); the agent value controls request bytes from
edge to agent (uploads). Values may differ safely. Deploy the edge first, then
restart or `beamd reload` the agent. Replace the existing `internal/mux`
comment that says both sides must use identical configuration.

At edge and agent startup, emit one structured informational log containing
`yamux_stream_window_bytes=<effective value>`. Part A does not add a
Prometheus metric.

Four MiB is a conservative bandwidth-delay-product default: 100 Mbit/s over a
250 ms round trip needs about 3.1 MB of in-flight data. It is not magic and it
does not remove flow control; it raises the ceiling from 256 KiB to 4 MiB per
grant cycle. Yamux grows buffers as data arrives rather than reserving the
whole window at session creation, but the possible per-stream receive
exposure is still 16 times larger. Observe process memory during rollout.

This path fixes A1. It is the production TCP path and eventual availability
fallback, but it is not expected to eliminate A2.

### 8.2 Part B only: yamux adapter hardening

Do not implement this subsection during Part A. It lands with the
transport-neutral adapter and capacity work in Part B.

When Part B begins, the yamux adapter must additionally use:

```go
cfg.AcceptBacklog = 64
cfg.EnableKeepAlive = false
cfg.KeepAliveInterval = 20 * time.Second // yamux validates it even when disabled
cfg.StreamOpenTimeout = 5 * time.Second
cfg.StreamCloseTimeout = 5 * time.Second
```

The application control heartbeat then becomes the single liveness mechanism.
Running both yamux keepalives and application heartbeats is redundant and can
leave a session transport-alive while its control stream is unusable.

In yamux v0.1.2, `AcceptBacklog` sizes both the incoming accept queue and the
local in-flight-SYN semaphore. It is not an active-stream or memory limit.
Keep `max_streams_per_session <= AcceptBacklog`; both ship as 64.

yamux does not offer a context-aware open operation.
`StreamOpenTimeout` starts only after a local SYN slot is acquired and closes
the entire session asynchronously if the peer never acknowledges; it does not
bound the `OpenStream` call itself. The adapter must therefore:

1. Derive the same adapter-owned five-second timeout described in Section 7.2.
2. Acquire an adapter-local 64-slot gate using that context. If acquisition is
   canceled, return the correct caller or adapter timeout without touching the
   session.
3. Run `yamux.OpenStream` behind a result channel with capacity one.
4. If `OpenStream` returns an error or no stream, release the gate immediately.
   If it returns a stream, transfer gate ownership to the stream and release it
   only when the wrapper's `Done` closes.
5. If the context expires after `OpenStream` starts, close the entire yamux
   session to unblock the call and wait for the result goroutine. Abort any
   returned stream and release its gate through `Done`; otherwise release the
   gate directly. Return `ErrOpenTimeout` only for the adapter timer and
   propagate caller cancellation unchanged.

Closing the session on a stuck open is deliberate: yamux offers no safe
per-open cancellation, and its own open timeout already treats this condition
as a session failure. Normal client reconnect/replay then recovers.

Do not build a goroutine wrapper for accept. yamux v0.1.2 already exposes
`AcceptStreamWithContext(ctx)`; call it directly and propagate its context
error without closing an otherwise healthy session.

## 9. Client transport selection and reconnect behavior

Add `Transport` to `internal/client.Options` with values `auto`, `quic`, and
`tcp`.

### 9.1 Selection

- **Until QUIC passes the deterministic Section 15.3 gates and the real-link
  production pilot, the shipped default is `tcp`, and `auto`/`quic` are
  opt-in.** Once both gates pass, the default flips to `auto`. The behavior
  below describes `auto`.
- `auto` is the production pilot and normal production mode because it retains
  TCP fallback. Forced `quic` is for `beamd check`, qualification, and
  diagnosis; do not use it as the unattended production-agent mode.
- On the first connection, `auto` tries QUIC, including the control hello,
  with a three-second candidate deadline.
- If QUIC fails, `auto` immediately attempts TCP/yamux with a five-second
  candidate deadline.
- `quic` never falls back.
- `tcp` never attempts UDP.
- Do not race two authenticated sessions. Only one candidate may send `hello`
  at a time.

The caller's outer context remains authoritative. Agent startup and
`beamd check` must allow at least ten seconds so both `auto` candidates can
run. Forced `quic` and forced `tcp` use a five-second candidate deadline.

Fallback is allowed only for transport availability failures: network/socket
errors, timeout, or a QUIC transport handshake failure. Certificate
verification failure, ALPN/protocol mismatch, `bad_token`, `bad_version`,
scope rejection, and any other authenticated control error are terminal and
must not be retried over TCP. The fixed fallback reason set is
`network|timeout|handshake`. Unknown/unclassified errors are terminal.

Before attempting the next candidate, synchronously close the failed
candidate's control stream and transport session, then wait for `Session.Done`
with a one-second cleanup bound. Closing a connection returned by
`quic.DialAddr` also closes its internally owned UDP socket. A timed-out QUIC
hello must not be allowed to finish authentication in the background while
TCP sends another hello. If cleanup does not complete within that bound,
return an error instead of starting the fallback candidate.

### 9.2 Reconnect

- Remember the last transport that completed `hello_ok`.
- Reconnect using the last successful transport first.
- If it fails, try the other transport in the same reconnect cycle when mode
  is `auto` and the failure is fallback-eligible.
- After falling back to TCP, do not probe QUIC on every retry. Retry TCP first
  for ten minutes. After that interval, the next reconnect probes QUIC first.
- Do not tear down a healthy TCP session merely to upgrade it. `beamd reload`
  provides an explicit immediate re-probe.
- Reuse the existing exponential backoff, jitter, shutdown bypass, and
  registration replay behavior.

`connectOnce` must be split into:

```text
dial transport -> open control -> hello/hello_ok -> install session
```

The common hello and session-install code must not be duplicated between
QUIC and yamux.

### 9.3 Data-stream handling

Change the client session fields to:

```go
transport tunnel.Session
control   tunnel.Stream
```

`acceptStreamsLoop` and `handleStream` must accept `tunnel.Stream`.

Add a 64-slot handler semaphore. If a misconfigured or malicious edge exceeds
the limit, abort the extra stream and increment a fixed-label metric/log
counter. Under the shipped configuration this should never happen.

Bound stream setup:

- the tunnel-name prefix is at most 63 bytes plus `\n`;
- the edge has five seconds to write the prefix, then clears the deadline;
- the agent has five seconds to read the complete prefix with a capped
  64-byte reader, then clears the deadline;
- a missing newline, oversized prefix, invalid RFC 1123 label, or unknown name
  aborts the stream;
- the backend connection uses `net.Dialer.DialContext` with a five-second
  timeout, not unbounded `net.Dial`.

For duplex proxying:

1. Copy request bytes from tunnel stream to backend.
2. On request EOF, call `CloseWrite` on the backend TCP connection when
   supported.
3. Copy response bytes from backend to tunnel stream.
4. On response EOF, call the tunnel stream's `CloseWrite`.
5. Wait for both copy loops, then perform final cleanup.
6. On context cancellation or either non-EOF error, abort both directions.

This retains WebSocket and streaming behavior and removes reliance on
"first copy to finish closes everything."

## 10. Edge integration

### 10.1 Edge fields

Replace concrete yamux fields:

```go
type Session struct {
    transport tunnel.Session
    control   tunnel.Stream
    kind      tunnel.Kind
    ...
}
```

Add to `Edge`:

- QUIC UDP connection, transport, and listener
- QUIC accept-loop wait group
- a set containing pre-authenticated and authenticated transport sessions
- pre-authentication and authenticated-session semaphores
- one global active-stream semaphore
- a set of every accepted raw TCP connection and a TCP-handler wait group
- an active-proxy wait group and a set of hijacked public connections
- shutdown state shared by both listeners

Move the existing `handleClient` control logic to
`handleTunnelSession(tunnel.Session)`. The TCP ALPN path wraps the TLS
connection in `tunnel.NewYamuxServer`; the UDP path already yields a tunnel
session.

### 10.2 Stream admission

Before `ReverseProxy.Transport.DialContext` opens a stream:

1. Load the current route and session.
2. Acquire a global lease.
3. Acquire the session lease.
4. Re-load `routes[host]` and verify it is still the same route pointer,
   session, and name captured in step 1; reject a stale route.
5. Re-check that the session is still open.
6. Pass the public request context to `OpenStream`; the adapter applies its
   own five-second open timeout.
7. Write the name prefix under its five-second deadline.
8. Return a connection whose close starts transport cleanup and whose
   `Done` releases both leases exactly once.

Acquire global before session everywhere. If a failure occurs before a tunnel
stream exists, release all acquired leases before returning. If a stream has
already been created, initiate `Abort`/close immediately but release the
leases only when `tunnel.Stream.Done` closes. Prefix-write failure follows the
latter rule.

Defaults:

| Limit | Value |
| --- | ---: |
| Active data streams per session | 64 |
| Active data streams across edge | 128 |
| Stream-open timeout | 5 seconds |

WebSockets and other upgraded connections hold one lease for their full
lifetime.

If capacity is unavailable, do not queue an unbounded goroutine and do not
wait five seconds. Return `ErrCapacity` immediately. The reverse proxy
`ErrorHandler` must map it to:

```http
HTTP/1.1 503 Service Unavailable
Retry-After: 1
Content-Type: application/json

{"error":"tunnel capacity reached"}
```

Other session/open failures remain `502 Bad Gateway`. A request canceled by
the public client must not be rewritten as a 502 or 503 after its context has
ended.

Extend `countingConn` or add a `leasedConn` wrapper so traffic accounting and
capacity release each occur once, including on prefix-write failure,
WebSocket close, proxy error, and context cancellation. Traffic accounting may
finalize when the application closes the connection; the admission leases
must remain held until `tunnel.Stream.Done`.

This distinction is load-bearing on yamux: `Stream.Close` is a local
half-close, and its receive buffer may remain until remote FIN or the
five-second close timeout. Releasing the lease at `Close` would make the
window-by-active-stream budget false during cancellation churn.

### 10.3 Resource accounting

Treat the configured values as conservative receive-flow-control exposure,
not preallocated resident memory:

- `4 MiB * 128 = 512 MiB` bounds edge yamux data-stream window exposure only
  while the global leases remain held.
- A QUIC connection can auto-tune its aggregate receive window up to 64 MiB;
  eight authenticated sessions therefore expose at most 512 MiB of QUIC
  connection flow-control credit.
- QUIC does not reserve the maximum window at connection creation, and yamux
  buffers grow only as data arrives.
- The 32 pre-auth sessions, control streams, TLS/TCP/UDP buffers, HTTP buffers,
  goroutine stacks, traffic/request-log queues, certificate state, and library
  bookkeeping are additional memory.

The 512 MiB yamux and 512 MiB QUIC figures are separate ceilings and may
coexist; neither is a total-process-memory promise. Holding leases through
`Stream.Done`, bounding pre-auth and authenticated sessions, and running with
the production `GOMEMLIMIT` are all required parts of the resource bound.

On the agent, validate the resolved
`BEAMD_YAMUX_STREAM_WINDOW_BYTES * 64 <= 1073741824` (1 GiB). This allows the
documented 16 MiB emergency maximum but makes its worst-case exposure
explicit. Default agent exposure is 256 MiB.

### 10.4 Protocol version enforcement

Keep `proto.ProtoVersion = 1`, add `proto.CodeBadVersion`, and enforce equality:

- Edge rejects a `hello` whose version is not exactly `1`.
- Client rejects a `hello_ok` whose version is not exactly `1`.
- Rejection closes the whole transport session.

This is validation that the current code is missing, not a new protocol
version.

### 10.5 Shutdown

Shutdown order must be:

1. Mark the edge as shutting down.
2. Close the TCP and QUIC listeners so no new public or tunnel connections
   enter.
3. Snapshot all accepted raw TCP connections and all pre-authenticated and
   authenticated tunnel sessions.
4. Send the existing NDJSON `shutdown` control message to authenticated
   sessions concurrently, with each control write bounded by the earlier of
   one second or the shutdown context deadline.
5. Run `http.Server.Shutdown` for ordinary public HTTP connections and wait on
   the active-proxy wait group so hijacked WebSocket connections are included
   in the drain.
6. At the caller's deadline, call `Close` on every remaining ordinary
   `http.Server`, every tracked hijacked public connection, and every accepted
   raw TCP connection. `Shutdown` alone does not force active ordinary
   connections or a stalled TLS handshake to exit.
7. Close remaining QUIC sessions with `CloseShutdown`; close yamux sessions.
8. Close the QUIC transport/UDP socket.
9. Wait for accept, raw TCP handler, and tunnel-session goroutines.
10. Flush final traffic counters.

Traffic counters must be flushed after the drain, not before it.
`http.Server.Shutdown` does not manage hijacked connections, so the explicit
set and active-proxy wait group are required. Admission must stop adding to
the wait group before shutdown begins waiting. Register each raw TCP
connection and increment its handler wait group before dispatching the handler;
the handler removes it and calls `Done` on every exit. It must re-check
shutdown before handing a completed TLS connection to either public HTTP or
the tunnel path. Listener admission, all relevant wait-group `Add` calls, and
the shutdown transition must synchronize on the same mutex so no `Add` can
race the first shutdown `Wait`.

On receipt of the NDJSON `shutdown` message, the agent sets the existing
skip-backoff flag but does not close the session immediately. It continues
serving already-open data streams until the edge closes the transport after
the drain.

## 11. Configuration

### 11.1 Part A process environment

Part A has one external setting on both edge and agent:

```text
BEAMD_YAMUX_STREAM_WINDOW_BYTES=<base-10 bytes>
```

It follows the default and validation contract in Section 8.1. It is
process-wide rather than an identity/account property. The detached agent
already inherits the environment of the CLI process that starts it; preserve
that behavior. A changed agent value takes effect after `beamd reload`. A
changed edge value takes effect after an edge restart.

Use `int64` while parsing and validating, then convert to `uint32` only when
constructing the yamux configuration. Internal runtime option fields are
allowed for dependency injection, but they must use `yaml:"-"` and must not be
saved by login/profile code. All client entry points—including the detached
agent and `beamd check`—must use the same resolver.

### 11.2 Part B server configuration

Add to `internal/config.Server` only when Part B begins:

```go
ListenQUIC             string `yaml:"listen_quic"`
DisableQUIC            bool   `yaml:"disable_quic"`
MaxStreamsPerSession   int    `yaml:"max_streams_per_session"`
MaxStreamsTotal        int    `yaml:"max_streams_total"`
MaxPreAuthSessions     int    `yaml:"max_pre_auth_sessions"`
MaxSessionsTotal       int    `yaml:"max_sessions_total"`
```

Initialize defaults before YAML unmarshalling so an omitted boolean retains
the shipped default while an explicit `false` is honored.

| YAML field | Default |
| --- | --- |
| `listen_quic` | Same host and numeric port as `listen_https` |
| `disable_quic` | `true` until QUIC passes its gates, then `false` |
| `max_streams_per_session` | `64` |
| `max_streams_total` | `128` |
| `max_pre_auth_sessions` | `32` |
| `max_sessions_total` | `8` |

Validation:

- QUIC address must parse as a UDP listen address.
- per-session streams must be between `1` and `64`, matching the agent handler
  cap and QUIC incoming-stream credit.
- global streams must be greater than or equal to per-session streams and no
  greater than `128`.
- pre-authentication sessions must be between `1` and `128`.
- authenticated sessions must be between `1` and `8`.
- the resolved `BEAMD_YAMUX_STREAM_WINDOW_BYTES` value multiplied by
  `max_streams_total` must not exceed
  `536870912` (512 MiB) unless a future explicit unsafe override is added.
- the fixed 64 MiB maximum QUIC connection window multiplied by
  `max_sessions_total` must not exceed 512 MiB.
- When the field lands in Part B, `disable_quic: true` is its initial shipped
  default and also a rollback switch. While QUIC is unproven,
  startup logs an informational note that only the tuned TCP path is enabled —
  not worded as "degraded." After QUIC is proven and made default,
  `disable_quic: true` reverts to a warning-level rollback switch.

Add environment overrides:

```text
BEAMD_LISTEN_QUIC
BEAMD_DISABLE_QUIC
BEAMD_MAX_STREAMS_PER_SESSION
BEAMD_MAX_STREAMS_TOTAL
BEAMD_MAX_PRE_AUTH_SESSIONS
BEAMD_MAX_SESSIONS_TOTAL
```

Boolean and integer parse failures must fail startup; do not silently keep a
default.

Example production YAML:

```yaml
listen_https: ":443"
listen_quic: ":443"                  # Part B
disable_quic: true                   # Part B: true until QUIC passes its gates
max_streams_per_session: 64          # Part B
max_streams_total: 128               # Part B
max_pre_auth_sessions: 32            # Part B
max_sessions_total: 8                # Part B
```

The Part A process environment is configured separately:

```text
BEAMD_YAMUX_STREAM_WINDOW_BYTES=4194304
```

### 11.3 Part B client/profile configuration

Part A adds no client/profile field. Add `Transport` only with Part B:

```go
Transport string `yaml:"transport,omitempty"`
```

The shipped default remains `tcp` until QUIC passes every gate; it then changes
to `auto`:

```yaml
transport: tcp
```

Add the environment override:

```text
BEAMD_TRANSPORT=auto|quic|tcp
```

Environment overrides win over the profile. The detached agent already
inherits the caller's environment; preserve that behavior. A changed profile
or environment takes effect after `beamd reload`.

Normal logins use `internal/config.Account`, not only `Client`. Add the same
`Transport` field to `Account`, copy it in `Account.Client()`, and apply one
shared default/validation/environment function to explicit configs and
account-derived clients.

## 12. CLI and local-agent diagnostics

The JavaScript npm shim needs no transport logic.

The Go CLI must add:

- `--transport auto|quic|tcp` to `beamd check` for explicit preflight tests;
- `transport` and `handshakeMs` to `beamd check --json`;
- `Client.Transport() tunnel.Kind`;
- a `transport:` line in human `beamd status`;
- `transport` in `beamd status --json`.

Add these exact fields to `daemon.HealthzResponse`:

```go
Transport           string `json:"transport,omitempty"`
ConfiguredTransport string `json:"configuredTransport"`
FallbackCount       uint64 `json:"fallbackCount"`
LastFallbackReason  string `json:"lastFallbackReason,omitempty"`
ReconnectCount      uint64 `json:"reconnectCount"`
LastCloseReason     string `json:"lastCloseReason,omitempty"`
```

`Transport` is empty while disconnected; `ConfiguredTransport` always reports
`auto`, `quic`, or `tcp`. Counts are monotonic for the current agent process.
The two reason fields use the fixed categories from Sections 9 and 13, never
raw errors.

Example:

```text
agent:     running
tunnel:    connected
transport: quic
```

When `auto` falls back, log one structured warning:

```text
event=transport_fallback from=quic to=tcp reason=<fixed category>
```

Do not print a warning on every reconnect attempt. The current selected
transport belongs in the agent's startup/session-connected log.

## 13. Metrics and logs

Part A requires only the effective-window startup log in Section 8.1. The
metrics in this section land with Part B. Extend the existing
dependency-free Prometheus exposition.

Required metrics:

```text
beam_transport_listener_up{transport="quic|tcp"}
beam_transport_sessions_active{transport="quic|tcp",state="preauth|authenticated"}
beam_transport_sessions_total{transport="quic|tcp"}
beam_transport_streams_active{transport="quic|tcp"}
beam_transport_handshake_errors_total{transport="quic|tcp",reason="timeout|tls|protocol|other"}
beam_transport_session_closes_total{transport="quic|tcp",reason="normal|shutdown|idle|protocol|network|other"}
beam_transport_stream_open_errors_total{transport="quic|tcp",reason="timeout|closed|other"}
beam_transport_capacity_rejections_total{scope="preauth_session|authenticated_session|session_stream|global_stream"}
beam_transport_stream_capacity{scope="session|global"}
beam_yamux_stream_window_bytes
```

Labels must come only from the fixed sets above. Do not label by remote
address, slug, tunnel, raw error, or QUIC connection ID.

`beam_transport_sessions_total` increments only after `hello_ok`, when a
session transitions into the authenticated state. Pre-auth attempts are
represented by the active gauge and handshake-error counter, so rejected
connections do not inflate successful-session totals.

`beam_transport_streams_active` counts leased data streams, including yamux
streams retained during their close timeout; it excludes control streams.
Capacity rejection increments only
`beam_transport_capacity_rejections_total`, because no transport open was
attempted. Preserve the existing `beam_active_sessions` metric unchanged for
current tests, docs, and scrapers; the new stateful metric is additive.

Fallback and reconnect attempts happen on the agent before the edge can
observe them, so they are not edge Prometheus metrics. Keep process-local
fallback and reconnect counters plus last fixed-category reasons in agent
health/status, and emit structured events. Add `ReconnectCount` and
`LastCloseReason` beside the fallback fields in `daemon.HealthzResponse`.

Required structured session log fields:

```text
event
transport
session_id
remote_addr
slug              # only after authentication
handshake_ms
active_streams
close_reason
error_category
```

`session_id` is a process-local random identifier used only for correlating
logs. Never log tokens or raw control messages.

Do not enable qlog by default. A future debug-only flag may write qlog to a
bounded temporary directory, but it is outside this implementation.

## 14. File-by-file implementation map

This table describes the eventual end state. Entries explicitly marked A1 are
the only files in Part A; all transport-abstraction and QUIC work is Part B.

| File/package | Work |
| --- | --- |
| `go.mod`, `go.sum` | Add `quic-go v0.60.0`; run `go mod tidy` |
| `internal/tunnel/*` | New interfaces and QUIC/yamux adapters |
| `internal/mux/*` | **A1:** accept the validated window and set only `MaxStreamWindowSize`; **B:** delete after adapter migration |
| `internal/proto/proto.go` | Make comments transport-neutral; add and enforce `CodeBadVersion` |
| `internal/client/client.go` | **A1:** `Options.YamuxStreamWindowBytes` → `mux.Client`; **B:** generic sessions, selection/fallback, reconnect, duplex half-close, handler cap, diagnostics |
| `internal/client/client_test.go` | Run behavioral tests against both adapters; selection and reconnect tests |
| `internal/edge/edge.go`, `single_listener.go` | UDP listener, generic session handler, admission leases, public-connection tracking, shutdown lifecycle |
| `internal/edge/traffic.go` | Exactly-once traffic and lease release |
| `internal/edge/reqevents.go` | Track/untrack hijacked public connections for shutdown |
| `internal/edge/metrics.go` | Fixed-label transport metrics |
| `internal/edge/*_test.go` | QUIC/TCP, capacity, shutdown, and metrics tests |
| `internal/config/server.go` | **A1:** carry the resolved window in a `yaml:"-"` runtime field for `edge.New`; **B:** server transport/capacity fields and validation |
| `internal/config/window.go` | **A1:** the shared env resolver `ResolveYamuxWindow` + window bounds (new file) |
| `internal/config/client.go` | **B only:** client transport fields/defaults/validation (A1 adds no field here) |
| `internal/config/accounts.go` | **B only:** persist the transport selection and copy it through `Account.Client()` |
| `internal/config/*_test.go` | A1 window environment tests; B transport/capacity tests |
| `internal/daemon/api.go` | Add selected transport to health response |
| `internal/daemon/daemon.go`, tests | Populate and verify local transport/fallback/reconnect health |
| `cmd/beamd/client.go` | **A1:** resolve the window for open/run + the detached agent (log at agent startup); **B:** pass client options and display transport |
| `cmd/beamd/check.go` | **A1:** resolve the window for `check`; **B:** forced-transport preflight and JSON timing |
| `cmd/beamd/resolve.go`, tests | **B only:** carry account and explicit-config transport settings through resolution |
| `cmd/beamd/main.go` | **A1:** resolve the window for `serve` and log it in the ready line; **B:** move readiness after listener bind and update help text |
| `test/e2e/*` | **A1:** asymmetric-window checksum transfers + command-level env/log tests; **B:** parameterize the suite over QUIC and TCP |
| `test/perf/*` | Deterministic payload server/client and result output |
| `scripts/perf-netem.sh` | Linux namespace/netem harness |
| `.github/workflows/test.yml` | Add unit, integration, race, vet, and vulnerability jobs |
| `.goreleaser.yaml`, `.github/workflows/release.yml` | Preserve all cross-build targets and run the packaging smoke test before publish |
| `Dockerfile` | Expose TCP and UDP 443 |
| `example/docker-compose.yml` | **A1:** forward `BEAMD_YAMUX_STREAM_WINDOW_BYTES` (key-only, absent when unset); **B:** publish both protocols |
| `example/.env.example` | **A1:** document `BEAMD_YAMUX_STREAM_WINDOW_BYTES`; **B:** add transport/capacity overrides |
| `example/beamd.yaml` | **B only:** document QUIC listener and capacity fields |
| `README.md`, `docs/setup.md`, `docs/deploy-spec.md` | UDP/firewall, diagnostics, fallback, sysctl, and rollback instructions |
| `docs/agent-api.md`, `docs/build-spec.md`, `docs/hosted-mode.md` | Update agent health and yamux-only architecture descriptions |
| `docs/post-manual-testing.md`, `docs/security-hardening.md` | Add dual-transport validation and security/lifecycle requirements |
| `scripts/build-npm.mjs` | No transport logic; verify every platform package includes the matching new Go binary |
| `npm/shim.cjs` | No code change expected; keep it as a binary launcher |

## 15. Test plan

### 15.1 Unit and contract tests

Part A must implement:

- default yamux window is exactly 4 MiB;
- absent environment uses the default on both edge and agent startup paths;
- exact minimum/maximum values pass, while empty, malformed, negative,
  overflowing, below-minimum, and above-maximum values fail startup;
- only `MaxStreamWindowSize` differs from the current yamux configuration;
- the resolved value reaches every current connection path: edge, foreground
  client, detached agent, normal account, explicit `--config`, and
  `beamd check`;
- asymmetric edge/agent values work safely, with payloads above 256 KiB
  checksum-verified in both upload and download directions;
- existing unit and end-to-end behavior remains green.

Part B must implement:

- server transport/capacity environment overrides and memory-product
  validation;
- `max_streams_total=128` is accepted and `129` is rejected;
- QUIC and yamux adapters satisfy the same session contract;
- QUIC `Close`, `CloseWrite`, `Abort`, deadlines, and addresses;
- normal QUIC `http.Transport` close emits FIN rather than reset, while
  cancellation emits a bidirectional reset;
- an early backend response with a deliberately blocked request-body writer
  proves QUIC `Close` unblocks and never races an in-flight `Write`;
- QUIC stream-open timeout normalization;
- caller cancellation propagates unchanged instead of becoming
  `ErrOpenTimeout`;
- yamux accept cancellation uses `AcceptStreamWithContext` without closing the
  session;
- candidate failure closes and joins the failed session before fallback;
- terminal auth/certificate/protocol errors never fall back;
- `CloseInfo` is stable and the session close metric/log emits exactly once;
- session/global lease acquisition and exactly-once release;
- canceled yamux streams retain their leases through remote EOF or the
  five-second close timeout;
- capacity errors map to 503 with `Retry-After: 1`;
- other stream-open errors map to 502;
- the 33rd pre-auth and ninth authenticated sessions are rejected without
  leaking slots;
- readiness is emitted only after all enabled listeners bind;
- an accept result queued during shutdown is closed rather than dispatched;
- stale route identity is rejected after lease acquisition;
- prefix length/read/write deadlines and backend-dial timeout are enforced;
- selected transport appears in daemon health and CLI JSON;
- protocol version mismatch is rejected on both sides;
- fallback reason classification uses only the fixed metric labels;
- QUIC key files are created atomically, mode 0600, reused, and rejected when
  malformed;
- shutdown closes listeners, drains requests, closes pre-auth sessions, and
  force-closes a stalled raw TCP/TLS handshake without leaving accept,
  handler, or copy goroutines.

### 15.2 End-to-end matrix

Run the current end-to-end behavior once with forced QUIC and once with forced
TCP:

- small GET;
- 253 KiB, 257 KiB, 1 MiB, 16 MiB, and 100 MiB downloads;
- the same sizes as successful request uploads with the test edge's
  `max_request_body_bytes` set to 128 MiB;
- a separate test proving the production-default 32 MiB body cap still returns
  413 for a 100 MiB upload;
- concurrent requests at 1, 8, 32, and 64 streams;
- chunked/streaming response;
- SSE;
- WebSocket bidirectional traffic;
- backend half-close;
- public-client cancellation;
- backend disappearance;
- agent reconnect and registration replay;
- edge shutdown and immediate agent reconnect;
- checksum validation on every payload.

Additional selection tests:

- `auto` selects QUIC when UDP works;
- `auto` falls back when UDP is dropped;
- `quic` fails clearly when UDP is dropped;
- `tcp` never opens a UDP socket;
- an established TCP fallback survives normally;
- `beamd reload` causes `auto` to probe QUIC again.

Capacity test:

- Hold 64 upgraded connections open on one session.
- The 65th receives 503 within 250 ms.
- Close one connection.
- The next request succeeds.
- All active-stream gauges return to zero.
- Across multiple sessions, hold 128 upgraded connections open.
- The 129th receives 503 within 250 ms; after one closes, the next succeeds.

Run:

```text
go test ./...
go test -race ./internal/... ./test/e2e/...
go vet ./...
govulncheck ./...
```

The release workflow must also cross-build the final binary for every shipped
darwin/linux and amd64/arm64 target and execute the npm packaging smoke test.

### 15.3 Performance harness

This section is the deterministic synthetic qualification harness. It runs
edge and agent inside controlled Linux namespaces; it does not claim to be the
production link. G1 first proves the original A2 symptom against the real
edge, and B4.5 separately validates the qualified implementation on the real
production link before any default flip.

The network impairment must apply only to the agent-to-edge leg. Do not shape
the public test client's connection to the edge, or the results conflate two
links.

`scripts/perf-netem.sh` should create Linux network namespaces and a veth pair,
run the edge and agent on opposite sides, and shape both directions explicitly
with these profiles:

| Profile | One-way delay | Loss | Rate |
| --- | ---: | ---: | ---: |
| `clean` | 75 ms | 0% | 100 Mbit/s |
| `lossy` | 75 ms | 1% random | 100 Mbit/s |
| `high-rtt-clean` | 250 ms | 0% | 20 Mbit/s |
| `high-rtt-lossy` | 250 ms | 1% random | 20 Mbit/s |

For each profile and transport, record JSON containing size, direction,
concurrency, TTFB, elapsed time, throughput, checksum result, and selected
transport. Run five unmeasured warm-ups per case. Then run at least 50 measured
iterations for 36-byte, 253 KiB, 257 KiB, and 1 MiB cases; 20 for 16 MiB; and
five for 100 MiB.

For every gated case, establish a same-direction, same-payload,
concurrency-one direct baseline over the same shaped veth: a raw QUIC stream
for the QUIC case and a raw TCP connection for the yamux/TCP case. The direct
fixture excludes beamd framing and reverse proxying but retains the same
crypto, qdisc, endpoints, CPU limits, and direction. Do not compare QUIC
against a TCP or iperf baseline when measuring protocol overhead.

Separately, compare beamd-over-QUIC directly with beamd-over-tuned-yamux on the
same host, impairment profile, direction, payload, concurrency, and run order.
This head-to-head comparison decides whether QUIC becomes the default.

Performance gates:

- Every throughput and tail gate below must pass separately for upload and
  download, at concurrency one, with zero corruption/errors.
- QUIC `clean`, 16 MiB and 100 MiB median throughput: at least 80% of its
  direct-QUIC baseline.
- QUIC `lossy`, 16 MiB median throughput: at least 60% of its direct-QUIC
  baseline, and p95 completion time no more than 2x direct p95.
- QUIC `lossy`, sequential 253 KiB, 257 KiB, and 1 MiB: p95 duration no more
  than 3x median and maximum no more than 5x median.
- QUIC `lossy`, solo 16 MiB median throughput: at least 70% of the aggregate
  throughput of an eight-stream run using eight 16 MiB payloads.
- QUIC `high-rtt-lossy`, 16 MiB median throughput: at least 60% of its
  direct-QUIC baseline, and p95 completion time no more than 2x direct p95.
- QUIC `high-rtt-lossy`, 100 sequential 36-byte responses: p95 TTFB no more
  than 3x median and p99 no more than 5x median.
- TCP `clean` and `high-rtt-clean`, 16 MiB median throughput: at least 70% of
  the corresponding direct-TCP baseline, demonstrating that 256 KiB is no
  longer the limiting window.
- Head-to-head clean-path gate: QUIC may not regress tuned-yamux median
  throughput or p95 completion time by more than 10% for any gated size or
  direction.
- Head-to-head A2 gate: on at least one lossy profile that reproduced A2 in the
  pre-build measurement, QUIC must either reduce solo small-response p95 by at
  least 30% or improve solo 16 MiB median throughput by at least 25%, while no
  other A2 primary metric regresses by more than 10%.
- The QUIC small-response distribution must not retain the tuned-TCP
  1/2/4-second timeout ladder.
- All functional sizes, directions, and concurrency cases: zero corruption
  and zero hangs.

Store raw JSON and a summary beside metadata containing the beamd commit,
Go/quic-go/yamux versions, kernel and OS, CPU/RAM/container limits, interface
offload state, exact `tc qdisc` output, and effective beamd configuration.

The netem suite is a manual or scheduled privileged job, not a required
unprivileged pull-request job. Passing it is necessary but not sufficient for
the default flip; the B4.5 production-link pilot is also required.

## 16. Canonical implementation task list

Work top to bottom. Part A is an independent release. Complete and record the
measurement gate before starting any Part B implementation.

### Part A — A1 tuned yamux window

- [x] **A1.1 — Freeze scope.** Confirm this change contains no
  `internal/tunnel`, QUIC dependency, capacity lease, yamux keepalive/backlog/
  timeout change, YAML/profile/account field, or protocol change.
- [x] **A1.2 — Implement the environment resolver.** Add
  `BEAMD_YAMUX_STREAM_WINDOW_BYTES` with compiled default `4194304`, base-10
  byte parsing, inclusive range `262144..16777216`, and fatal handling for a
  present invalid value. Parse once per process as specified in Sections 8.1
  and 11.1.
- [x] **A1.3 — Wire every process path.** Pass the resolved value to the edge,
  foreground client, detached agent, normal-account path, explicit-config
  path, and `beamd check`. Do not persist it in user configuration.
- [x] **A1.4 — Tune yamux and nothing else.** Change the existing
  `internal/mux` constructor to set `MaxStreamWindowSize` from the validated
  value while preserving every other current setting. Correct its misleading
  identical-config comment.
- [x] **A1.5 — Add operational visibility.** Log the effective value once at
  edge startup and once at agent startup. Add the variable and its default to
  `example/.env.example`, setup/deployment documentation, and release notes.
- [x] **A1.6 — Complete A1 tests.** Pass every Part A case in Section 15.1,
  including invalid environment values, all connection paths, asymmetric
  receiver values, and checksum-verified payloads above 256 KiB in both
  directions.
- [x] **A1.7 — Verify the repository.** Run `go test ./...`, the relevant race
  tests, `go vet ./...`, existing end-to-end tests, and the npm packaging smoke
  test.
- [ ] **A1.8 — Release independently.** Deploy the edge first, confirm its
  effective-window log, release/reload the agent, confirm its log, validate a
  16 MiB upload and download, and observe process memory. Record the release
  commit and results under `test/perf/results/`.

Part A is complete after A1.1–A1.8. The measurement gate below is a separate
decision task, not part of A1 completion.

### Measurement gate — prove A2 before Part B

- [ ] **G1.1 — Establish the tuned-TCP baseline.** Verify A1 is live on both
  receivers and record the effective window, commit, host, OS, Go/yamux
  versions, CPU/memory limits, and impairment settings.
- [ ] **G1.2 — Exercise the small-response signature.** On the real tunnel,
  run at least 100 measured 36-byte responses and 50 measured 253 KiB
  responses under clean and lossy/high-RTT conditions, first sequentially at
  concurrency one and then at concurrency eight.
- [ ] **G1.3 — Exercise large solo transfers.** Measure 16 MiB and 100 MiB
  uploads and downloads at concurrency one and eight, with checksum
  verification.
- [ ] **G1.4 — Record comparable statistics.** Capture p50, p95, p99, maximum,
  throughput, errors, and the raw timing sequence. Explicitly note any
  approximately 1/2/4-second timeout ladder.
- [ ] **G1.5 — Make and record the decision.** Part B is justified only when a
  repeatable A2 signature remains after A1—for example, solo small-response
  p95 is both over one second and over twice the concurrency-eight p95, the
  timeout ladder is visible, or solo large-transfer throughput is below 70%
  of the corresponding eight-stream aggregate. Save raw results and a dated
  go/no-go decision under `test/perf/results/`.
- [ ] **G1.6 — Stop when A2 is not proven.** If the gate does not justify Part
  B, mark B1–B4 “not currently justified” in the decision record and make no
  QUIC changes.

### Part B, Change 1 — abstraction and generic-session guardrails

- [ ] **B1.1 — Add `internal/tunnel`.** Implement the interfaces, normalized
  errors, close information, and yamux adapter from Section 6.
- [ ] **B1.2 — Move callers behind the abstraction.** Remove concrete yamux
  types from client and edge code while keeping production forced to TCP.
- [ ] **B1.3 — Add the Part B yamux hardening.** Implement Section 8.2,
  including bounded open/accept behavior and lifecycle-safe stream cleanup.
- [ ] **B1.4 — Add resource admission.** Implement session/global leases,
  pre-auth/authenticated session limits, stale-route checks, exact release
  semantics, and memory-product validation from Section 10.
- [ ] **B1.5 — Prove no behavioral change.** Pass the existing functional,
  cancellation, shutdown, capacity, race, and TCP performance tests before
  adding QUIC.

### Part B, Change 2 — QUIC engine, default off

- [ ] **B2.1 — Add and pin quic-go.** Add `github.com/quic-go/quic-go`
  `v0.60.0` and record the Go-version requirement.
- [ ] **B2.2 — Implement QUIC transport.** Complete the listener, dialer,
  stream adapter, TLS/ALPN, flow control, key persistence, and lifecycle
  requirements in Sections 6 and 7.
- [ ] **B2.3 — Add dual-transport tests.** Run the shared session contract and
  full HTTP/streaming/WebSocket/reconnect/cancellation suite over forced QUIC
  and forced TCP.
- [ ] **B2.4 — Keep QUIC unreachable by default.** Ship the edge with
  `disable_quic: true` and the agent with `transport: tcp`; use forced QUIC
  only in tests and explicit `beamd check` qualification.

### Part B, Change 3 — selection, flags, diagnostics, and rollback

- [ ] **B3.1 — Implement transport modes.** Add `tcp`, `auto`, and `quic`
  selection with the exact fallback classification, cleanup, reconnect, and
  re-probe behavior in Section 9.
- [ ] **B3.2 — Implement the two rollback controls.**
  `BEAMD_DISABLE_QUIC=true` plus an edge restart is the global kill switch;
  `BEAMD_TRANSPORT=tcp` plus `beamd reload` is the local-agent override.
- [ ] **B3.3 — Make `auto` the only production pilot mode.** It must prefer
  QUIC and fall back to tuned TCP. Forced `quic` remains diagnostic and must
  never silently fall back.
- [ ] **B3.4 — Add diagnostics.** Implement `check`, `status`, health fields,
  fixed-label metrics, structured logs, selected-transport reporting, and
  protocol-version enforcement.
- [ ] **B3.5 — Rehearse rollback.** With an established `auto` agent, enable
  the edge kill switch and restart the edge; verify the agent reconnects over
  TCP without changing its configuration. Separately verify the local
  `BEAMD_TRANSPORT=tcp` override.

### Part B, Change 4 — deployment, qualification, and default flip

- [ ] **B4.1 — Prepare production networking.** Publish UDP 443, update
  Docker/firewall configuration, persist required UDP sysctls, and document
  memory and macOS socket-buffer guidance.
- [ ] **B4.2 — Build the qualification harness.** Implement Section 15.3 and
  store raw JSON plus environment metadata under `test/perf/results/`.
- [ ] **B4.3 — Pass functional qualification.** Complete the Section 15.2
  matrix over both transports with zero corruption, hangs, or semantic
  regressions.
- [ ] **B4.4 — Pass synthetic protocol and head-to-head performance gates.**
  In the deterministic Section 15.3 harness, QUIC must pass its direct baseline
  gates, stay within the clean-path regression budget, materially beat tuned
  yamux on a profile that reproduces A2, and eliminate the tuned-TCP timeout
  ladder.
- [ ] **B4.5 — Pilot in `auto`.** Enable the edge QUIC listener, keep the
  default agent mode unchanged, explicitly opt the production agent into
  `auto`, validate both directions/WebSockets/reconnect over the real
  production link, and observe metrics and memory.
- [ ] **B4.6 — Flip defaults only after the pilot.** Change the agent default
  to `auto` and edge default to `disable_quic: false`, while permanently
  retaining both rollback controls and the tuned TCP path.

Do not begin B1 without a recorded G1 go decision. Do not execute B4.6 until
B1–B4.5, the functional and performance gates, and the rollback rehearsal all
pass. After B4.6, complete the final production validation and Definition of
Done.

## 17. Production host requirements

For the current single-user deployment, use one stable public VM with at least
2 vCPU and 2 GiB RAM. Keep the architecture single-cell; multi-region work is
not justified yet.

Required network exposure:

```text
443/tcp  public HTTPS + TLS/yamux fallback
443/udp  QUIC tunnel transport
```

The tunnel-control hostname must resolve directly to this host. A TCP-only
reverse proxy in front of it cannot carry the QUIC path.

On Linux, persist:

```text
net.core.rmem_max=7340032
net.core.wmem_max=7340032
```

Set these on the host, not only inside the container. Keep path-MTU discovery
enabled.

On a macOS agent, first run the performance qualification with the OS default.
If quic-go reports an undersized UDP buffer or the gates fail for socket-buffer
reasons, test:

```text
sudo sysctl -w kern.ipc.maxsockbuf=8441037
```

Do not make a laptop-wide sysctl change silently from the CLI; document and
surface the diagnostic.

For a 2 GiB container/VM:

```text
GOMEMLIMIT=1400MiB
```

Alert or investigate when:

- process RSS remains above 1.5 GiB;
- any capacity rejection occurs during normal single-user work;
- the agent unexpectedly selects TCP;
- QUIC stream-open errors persist for more than five minutes;
- reconnects repeat without a stable session.

Docker must publish both protocols:

```yaml
ports:
  - "443:443/tcp"
  - "443:443/udp"
```

The Dockerfile must declare:

```dockerfile
EXPOSE 443/tcp 443/udp
```

## 18. Cutover and rollback

Part A (the yamux window) is an ordinary release: build, deploy the edge, and
restart the agent. The edge receiver setting improves downloads; the agent
receiver setting improves uploads. Mismatched versions or values are safe, so
no firewall, sysctl, or flag-day coordination is needed. The coordinated
flag-day below applies to **Part B**, when QUIC is enabled.

Because there is one user, use a coordinated flag-day deployment:

1. Build the edge, CLI binaries, npm artifacts, and image from one commit.
2. Open host/cloud firewall UDP 443 and add the Docker UDP port mapping.
3. Apply the UDP sysctls.
4. Stop the local agent.
5. Install or stage the matching local `beamd` binary/npm release.
6. Deploy the new edge.
7. Run the staged matching binary's `beamd check --transport tcp` against the
   new edge.
8. Run `beamd check --transport quic`.
9. Start/reload the agent in `auto` mode.
10. Confirm `beamd status` reports `transport: quic`.
11. Validate a 16 MiB download, a 16 MiB upload, a WebSocket, and reconnect.
12. Watch transport metrics and memory for at least ten minutes.

If QUIC validation fails but TCP works, set `BEAMD_DISABLE_QUIC=true` on the
edge and restart the edge. An agent in `auto` must reconnect over TCP without
an agent configuration change. If the edge cannot be changed immediately, or
to isolate an agent-side problem, set `BEAMD_TRANSPORT=tcp` for the agent and
run `beamd reload`. Either control independently restores the old data path;
using both is optional.

If correctness fails on both transports, roll back the edge image/binary and
the local binary to the previous tag. Roll back both sides together; do not add
mixed-version compatibility code solely for rollback.

## 19. Definition of done

**Part A is done when:**

- [ ] every A1 checklist item is complete;
- [ ] an absent `BEAMD_YAMUX_STREAM_WINDOW_BYTES` produces an effective
  4 MiB window on edge and agent;
- [ ] every accepted and rejected environment value behaves as specified;
- [ ] the edge controls downloads and the agent controls uploads, verified
  with asymmetric values and checksums;
- [ ] no unrelated yamux setting or Part B architecture changed;
- [ ] startup logs expose the effective value and repository/e2e/package tests
  pass;
- [ ] the edge-first and agent-reload production validation succeeds.

The G1 measurement is deliberately separate. A1 may be complete and released
even if G1 concludes that Part B is unnecessary.

**Part B is complete only when:**

- [ ] a recorded G1 result justifies Part B;
- [ ] every B1–B4 checklist item is complete;
- [ ] neither `internal/client` nor `internal/edge` imports yamux or quic-go
  directly;
- [ ] the npm shim remains a launcher and contains no transport code;
- [ ] UDP 443 QUIC is the default selected production path;
- [ ] both the edge-wide and agent-local rollback controls are rehearsed;
- [ ] TCP/yamux fallback has a verified 4 MiB default window;
- [ ] all HTTP, streaming, WebSocket, reconnect, cancellation, and shutdown tests
  pass over both transports;
- [ ] stream and memory limits are enforced and observable;
- [ ] `beamd check` can force either transport;
- [ ] `beamd status` reports the active transport;
- [ ] Docker and host configuration expose/tune UDP;
- [ ] unit, race, vet, e2e, and netem qualification pass;
- [ ] QUIC stays within the clean-path regression budget and materially beats
  tuned yamux on a profile that reproduces A2;
- [ ] production validation shows no 256 KiB/RTT throughput plateau and no
  concurrency-dependent tail collapse on the QUIC path.

## 20. References

- [quic-go v0.60.0 release](https://github.com/quic-go/quic-go/releases/tag/v0.60.0)
- [quic-go v0.60.0 API](https://pkg.go.dev/github.com/quic-go/quic-go@v0.60.0)
- [quic-go flow-control guidance](https://quic-go.net/docs/quic/flowcontrol/)
- [quic-go stream lifecycle](https://quic-go.net/docs/quic/streams/)
- [quic-go server and listener lifecycle](https://quic-go.net/docs/quic/server/)
- [quic-go UDP buffer guidance](https://quic-go.net/docs/quic/optimizations/)
