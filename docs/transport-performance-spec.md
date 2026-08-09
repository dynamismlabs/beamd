# Beamd Transport Performance — Specification and Task Checklist

**Status:** A1 shipped. G1 is GO and Part B is implemented default-off. The
active-data liveness correction passed its exact staging recheck, and the next
fresh matrix completed 39 of 48 blocks with 796 clean records before block 40
exposed a distinct TCP/yamux caller-open timeout. Six concurrent 8 MiB uploads
under 500 ms RTT / 1% loss delayed a new stream SYN beyond the shared
five-second adapter bound; the adapter then closed the healthy shared session,
producing one 502 and seven downstream route-loss errors. TCP now has a
separate 60-second caller bound below yamux's 75-second establishment timer,
and targeted mode can reproduce the exact frozen mixed workload. Local
verification is complete; its exact staging recheck and then a fresh complete
matrix are the remaining B4.4 sequence. Qualification and a production-link pilot still
gate enabling QUIC for the hosted service. The compiled and self-hosted
defaults permanently remain TCP with the edge QUIC listener disabled.

**Owner:** Dynamism

**Last updated:** 2026-08-08

**Scope:** `beamd` edge, Go client/agent, shared transport code, packaging, deployment, tests, and observability

This is the single canonical implementation document for A1 and A2. Section
16 is the ordered, executable task checklist; the surrounding sections define
its requirements and acceptance criteria. Check an item only when its
referenced requirements and tests pass. `TASKS.md` links here but does not
duplicate this checklist.

**Before executing Part B, read [`transport-a2-findings.md`](transport-a2-findings.md)** —
the measured findings, the reasoning behind the A2 corrections (the original
solo-transfer hypothesis was disproven; the real defect is head-of-line blocking
under mixed load), the zrok/OpenZiti prior-art research, and the Part B handoff.
Raw evidence is under `test/perf/results/`.

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

**Part B — QUIC over UDP 443 — is built behind a default-off edge flag and is
activated by product policy only after it beats the tuned-yamux baseline in
the deterministic impairment harness and succeeds in a real production-link
pilot.** Building QUIC is the large effort (a transport abstraction, both
adapters, listener/key lifecycle, selection/fallback, capacity, and a netem
qualification harness). Turning the flag *off* does not make that work cheap;
the flag buys a safe default and operational reversibility, not less code.
The same binary supports both product modes. Self-hosted/token accounts retain
`transport: tcp` and self-hosted edges retain `disable_quic: true` when the
operator supplies no override. Hosted/session accounts resolve an omitted
transport to `auto`, and the hosted edge deployment explicitly enables its
QUIC listener after B4 and the pilot pass. Forced `transport: quic` is for
qualification and diagnosis. The edge-wide rollback is
`BEAMD_DISABLE_QUIC=true` plus an edge restart, after which `auto` agents
reconnect over TCP. The abstraction layer is introduced only when QUIC is
greenlit — it has no value while there is a single implementation.

Between the two parts, **prove A2 on real beamd processes** (Section 16): with the
4 MiB window already live, measure interactive-request latency under concurrent
bulk load (the head-of-line test) across lossy / high-RTT profiles, with the
solo small-response and large-transfer signatures as controls. If interactive
latency does not degrade under load, Part B may not be needed. (Measured
2026-07-24: it degraded 10–38× in the controlled reproduction — GO. Operator
approved the default-off Part B implementation on 2026-07-25; remote-edge
confirmation is optional and is not an implementation prerequisite.)

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

The **target end-state** production design is:

1. Raw QUIC over UDP 443 is the preferred hosted client-to-edge transport
   after the hosted rollout gates pass.
2. TLS + yamux over TCP 443 remains an automatic availability fallback for
   networks that block or impair UDP, carrying the 4 MiB window from Part A.
3. The edge admits at most 64 active proxy streams per session and 128
   globally. This bounds active receive-flow-control exposure; it is not a
   claim that all window credit is preallocated resident memory.
4. The edge admits at most 128 concurrent raw TCP TLS handshakes, 32 tunnel
   pre-authentication sessions, and eight authenticated sessions.
5. Product defaults remain deliberately different:
   - self-hosted/token accounts, legacy accounts with no kind, and standalone
     client configs default to `tcp`; the compiled edge default remains
     `disable_quic: true`;
   - hosted/session accounts default to `auto`, and the hosted deployment
     explicitly sets `disable_quic: false` only after the deterministic
     Section 15.3 gates and the real-link production pilot both pass.
   Explicit account/profile settings and environment overrides retain
   precedence in both modes.
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
- **A2:** all of an app's streams multiplex over one long-lived TCP connection,
  so TCP's strict in-order delivery couples them. Under concurrency a
  latency-sensitive request stalls behind other streams' bulk data — queued
  behind it in the send buffer even without loss (bufferbloat), and blocked
  behind a lost packet's retransmit under loss (head-of-line blocking). Measured
  2026-07-24 (`test/perf/results/hol-2026-07-24/`): a 4 KiB request inflates
  ~10x on a clean link and up to ~38x under bursty loss (reaching 1.7–7.7 s on
  wifi/mobile) once concurrent bulk shares the tunnel.

The original A2 hypothesis — that *solo* transfers are slow under loss and
parallel traffic *hides* it — was measured **false**: solo bulk throughput is
fine (~97% of the 8-stream aggregate), with no 1/2/4 s timeout ladder. The real
defect is the reverse — concurrent bulk is what inflates interactive latency —
so the gates that follow were corrected to measure it.

Increasing the yamux window fixes A1, but it does not fix A2 — the window is not
the bottleneck; the single shared connection is. QUIC fixes A2 because each
stream is delivered independently, so a small interactive request is not stuck
behind a bulk transfer's queued or lost packets. This is the same conclusion
OpenZiti reached with its westworld3/dilithium "Transwarp" UDP transport; beamd
uses off-the-shelf quic-go rather than a custom protocol. QUIC still has
connection-level congestion control, so it is not assumed to eliminate every
effect; the interactive-latency-under-load gate (Section 15.3) must prove QUIC
collapses the head-of-line penalty before the hosted service activates it. The
tuned yamux path remains the self-hosted default, hosted fallback, and the
immediate A1 win while QUIC lands.

This design does **not** add a rate limiter. Limits in this specification bound
concurrency and memory, not bytes per second.

## 3. Goals

- A latency-sensitive request must not be materially slower merely because other
  (bulk) streams are concurrently active on the same tunnel connection.
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
                         | hosted: QUIC/UDP 443     |
                         | default/fallback: TCP    |
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
    Code      ErrorCode
    CodeValid bool // distinguishes CloseNormal (0) from "no application code"
    Remote    bool
    Reason    string
    Cause     error
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
| `0x06` | `CloseIdle` | Application heartbeats timed out |
| `0x10` | `StreamCanceled` | Request, backend, or caller canceled a stream |
| `0x11` | `StreamCapacity` | Stream could not be admitted |

The yamux session adapter's `CloseWithError` ignores the application error
code and closes the whole yamux session. The yamux stream adapter's `Abort`
also ignores its code but follows the per-stream behavior in Section 6.3. Both
must normalize session-shutdown errors to `ErrSessionClosed`.

`CloseInfo` is first-terminal-event-wins and is stored before `Session.Done`
closes. A later local cleanup call must not overwrite a remote or network
failure already recorded. The adapters populate it as follows:

| Terminal event | `CodeValid` / `Code` | `Remote` | Fixed close-reason label |
| --- | --- | --- | --- |
| Local `CloseWithError` | `true` / requested code | `false` | map requested code below |
| Remote QUIC application close | `true` / peer code | `true` | map peer code below |
| QUIC idle timeout | `false` | as reported when knowable | `idle` |
| QUIC stateless reset, no viable path, socket/network failure | `false` | as reported when knowable | `network` |
| QUIC transport/protocol violation or version failure after a session exists | `false` | as reported when knowable | `protocol` |
| Remote clean yamux EOF | `false` | `true` | `normal` |
| Local yamux `CloseWithError` | `true` / requested code retained locally | `false` | map requested code below |
| Unclassified yamux/session failure | `false` | only when knowable | `network` or `other` |

Application-code mapping is fixed: `CloseNormal` and `CloseSuperseded` map to
`normal`; `CloseShutdown` maps to `shutdown`; `CloseProtocol` and `CloseAuth`
map to `protocol`; `CloseIdle` maps to `idle`; `CloseCapacity` and unknown
codes map to `other`. Capacity has its own rejection counter. `Reason` is the
local description or peer application-close description, sanitized to valid
UTF-8 and capped at 256 bytes before storage or logging. It is never a metric
label. `Cause` retains the original wrapped error for `errors.Is` /
`errors.As` and debug diagnostics.

Stream I/O errors are deliberately not normalized beyond standard `io.EOF`,
context errors, deadlines/`net.Error`, and `ErrSessionClosed`. Proxy callers
classify those cases and treat any other non-EOF stream error as an abort;
they must not depend on concrete yamux or quic-go error types.

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
- `Done()`, closed after application-level completion of both directions,
  immediately after abort, or before the parent session's `Done` becomes
  observable

The QUIC wrapper has an explicit send state `open -> fin -> reset` and receive
state `open -> terminal`. `fin -> reset` escalation is allowed; `reset` never
returns to a graceful state. `Done` is application-level completion, not
acknowledgement of the FIN or reset (quic-go exposes no such combined signal):
it closes when send is `fin|reset` and receive is terminal, immediately on
`Abort`, or on parent-session death. Read EOF, a read reset, or a session error
marks receive terminal; an ordinary deadline timeout does not. `CloseWrite`
alone does not mark receive terminal.

Use `writeMu` to serialize `Write` with the graceful-FIN portion of
`CloseWrite` / `Close`, plus a short `terminalMu` to arbitrate graceful close
against abort:

1. `Write` holds `writeMu`, verifies send state is `open`, and then writes.
2. `CloseWrite` takes `writeMu` and then `terminalMu`. If send is `open`, it
   changes the state to `fin` and calls the underlying QUIC `Close`; if send is
   already `fin`, it is idempotent; if send is `reset`, it must not call
   underlying `Close`.
3. `Abort` never waits for `writeMu`. It takes only `terminalMu`, changes
   `open|fin -> reset`, and calls `CancelWrite` immediately. This wakes a
   flow-control-blocked `Write` and safely escalates a previously graceful FIN.
   It always calls `CancelRead`, so the receive direction becomes terminal.
4. `Close` first sets an immediate write deadline without either mutex so an
   early-response request-body writer wakes. It then performs the graceful
   send close above if the stream was not aborted and cancels only the read
   direction.

Holding `terminalMu` across the underlying graceful `Close` prevents
`CancelWrite` from racing it. If graceful close is still waiting for
`writeMu`, `Abort` can acquire `terminalMu` and cancel the blocked write first.
Do not use one shared send-side `sync.Once`: it would prevent the required
`fin -> reset` escalation and can deadlock `Abort` behind a blocked writer.

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
configured five-minute `StreamCloseTimeout` for forced cleanup. The same
fallback applies to graceful `CloseWrite`: yamux v0.1.2 cannot distinguish the
two operations through its public API. Its `Done` closes when local close and
remote EOF are both observed, or through an adapter fallback one second after
yamux's raw timeout. That guard keeps the admission lease alive until raw
stream cleanup is guaranteed to have begun. Parent-session death overrides the
timeout and closes `Done` immediately. If parent death races a blocked raw
`Close`, the wrapper must remember that it is already complete and must not arm
a new fallback timer when the raw call returns.

Five minutes deliberately restores yamux's upstream default. Expiry forcibly
closes the stream and sends RST; on the peer, that reset takes precedence over
data already in `recvBuf`. The former five-second setting can therefore corrupt
otherwise healthy slow response tails and reproduces the observed failure
mechanism locally. Do not shorten the graceful-close fallback unless the
adapter first gains a separate, tested per-stream reset for `Abort`.

Each session adapter owns a mutex-protected registry of every child stream
wrapper. `OpenStream` and `AcceptStream` register the wrapper before returning
it. Registration racing session shutdown must either return a registered live
stream or abort it and return `ErrSessionClosed`; it must never return an
orphan. On underlying session death, the adapter stores `CloseInfo`, marks the
session closed, snapshots and clears the registry under its mutex, releases
the mutex, terminalizes every child, and only then closes `Session.Done`.
Therefore, once `Session.Done` is observable, every child `Stream.Done` is
already closed and all leases can release. Ordinary child completion
unregisters exactly once. Never call child callbacks while holding the
registry mutex.

All `Done`, unregister, and capacity-release callbacks use their own
`sync.Once`; terminal send-state arbitration does not.

One lifecycle goroutine per session owns the terminal log/metric emission
after `Session.Done` closes. It reads `CloseInfo` and classifies the result into
the fixed close-reason set. Accept loops and copy loops must not each emit a
second session-close record.

## 7. QUIC transport

Add a direct dependency on `github.com/quic-go/quic-go` pinned to `v0.60.0`.
That release requires Go 1.25. This repository pins Go 1.25.12 as the minimum
patched toolchain and also supports newer maintained Go releases.

This is raw QUIC carrying beamd's existing streams. Do not add `http3`.

### 7.1 TLS and ALPN

- QUIC ALPN: `beamd-quic/1`
- TCP fallback ALPN: retain `beam/1`
- QUIC TLS minimum: TLS 1.3
- Certificate source: clone the edge's existing certificate configuration and
  retain its `GetCertificate` callback
- Client SNI: derive the original hostname from `serverAddr` with
  `net.SplitHostPort` and set `tls.Config.ServerName` explicitly before
  resolving or dialing; never derive SNI from a resolved UDP address
- Certificate verification: same trust behavior as the existing TLS client
- `InsecureSkipVerify`: retain for local development only
- 0-RTT: disabled (`Allow0RTT: false` on the server; the field is server-only)
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
DisablePathMTUDiscovery:        false
```

Role-specific values:

| Endpoint | `MaxIncomingStreams` | Reason |
| --- | ---: | --- |
| Edge | `1` | Concurrent credit for the agent's one control stream; defense in depth, not a lifetime stream count |
| Agent | `64` | The edge opens at most 64 concurrent data streams |

Use `OpenStreamSync` in the QUIC adapter. The adapter derives a five-second
context with `context.WithTimeoutCause(ctx, 5*time.Second, ErrOpenTimeout)`.
If that adapter-owned timer expires, return `ErrOpenTimeout`. If the caller's
context is canceled or reaches its own earlier deadline, propagate the
caller's error unchanged. This distinction lets the reverse proxy recognize a
disconnected public requester instead of rewriting cancellation as a 502.

The 4 MiB initial per-stream window avoids recreating A1 during QUIC's
auto-tuning ramp. The 64 MiB value is aggregate connection-level receive
credit, not a per-stream commitment; it bounds the sum even though individual
streams may auto-tune as high as 16 MiB.

The three- or five-second transport-candidate context starts **before DNS** and
is reused through DNS resolution, QUIC handshake, control-stream open, and
`hello_ok`; no stage restarts the timer. Split the original host and port, keep
the original host in `tls.Config.ServerName`, and resolve it with a
context-aware `net.Resolver` under that candidate context. For an IP literal,
skip DNS. Then call `quic.DialAddr` with a numeric `ip:port`, so its internal
`net.ResolveUDPAddr` performs no DNS. Try resolved addresses in resolver order
within the one shared budget, closing and joining each failed QUIC connection
before trying another; only one candidate may send `hello` at a time. Retry
another resolved address only for address-specific network/socket failure.
Certificate verification, ALPN/protocol, authentication, and other terminal
errors stop the address loop immediately. A resolver failure/no-address result
is `network`; expiration of the candidate context is `timeout`.

Retain one in-memory `quic.NewLRUTokenStore(8, 4)` for the lifetime of the
`Client`, so reconnects can reuse address-validation tokens keyed by the
original TLS server name. Do not persist client tokens and do not use
`DialAddrEarly`. Each `DialAddr` attempt owns its private UDP socket and closes
it with the returned QUIC connection (or before returning a dial failure);
tests must prove failed address attempts and canceled candidates leak neither
sockets nor background authentication.

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

`disable_quic: true` is a hard isolation boundary, not merely "do not enter
the accept loop." Resolve environment overrides before QUIC-specific
validation. In disabled mode, do not parse/bind `listen_quic`, load or generate
QUIC keys, construct a UDP socket or `quic.Transport`, or start any QUIC
goroutine. TCP readiness must succeed even if the UDP port is occupied or QUIC
key files are missing or malformed. This property is required for rollback
and has dedicated startup tests.

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

When QUIC is enabled, each is 32 cryptographically random bytes, written
atomically. Configure
`StatelessResetKey` and `TokenGeneratorKey` from them. A missing key is
generated; a malformed key is a startup error, not silently replaced.

Accepted QUIC connections, including connections still waiting for a control
hello, must be tracked explicitly. Closing a `quic.Listener` does not close
accepted connections.

There is a fixed 128-slot raw TCP TLS-handshake gate in front of the per-tunnel
pool. Acquire it without waiting immediately after TCP `Accept`, before
starting the handler goroutine, and release it after the bounded TLS handshake
on every ALPN outcome. If it is full, close the raw connection and increment
the `tls_handshake` capacity-rejection counter. This gate includes ordinary
public HTTPS and tunnel clients because ALPN is unknown before the handshake;
the existing handshake deadline remains required.

There are then 32 tunnel pre-authentication slots:

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

The control stream must remain bidirectionally open for the full session
lifetime. Any control EOF, reset, read/write error, or liveness failure is
session-terminal: record the cause and close the transport session. A valid
control heartbeat or any successful read or write on an authenticated data
stream refreshes session activity; the existence of an open stream alone does
not. This distinction is required on TCP/yamux because control heartbeats share
the data stream's ordering domain and may be head-of-line blocked behind an
active bulk transfer. Session teardown owns control-stream cleanup; do not send
a separate stream FIN after claiming the connection close. Over QUIC that FIN
can arrive before the authoritative application close and make an intentional
shutdown appear to the peer as a local protocol failure.

Run the unexpected-agent-stream guard on the edge for **both** adapters after
accepting the control stream. Any second agent-opened stream is aborted and the
session closes with `CloseProtocol`. QUIC's `MaxIncomingStreams: 1` limits only
concurrently open peer streams; it is defense in depth, not a lifetime limit,
so the guard must cover the control-close/new-stream race. Tests must attempt a
second stream immediately after control EOF and prove it is never authenticated
or installed.

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
cfg.StreamOpenTimeout = 75 * time.Second
cfg.StreamCloseTimeout = 5 * time.Minute
```

The application-level activity watchdog then becomes the single liveness
mechanism. It is fed by control heartbeats and successful authenticated
data-stream I/O. Running yamux keepalives as well is redundant and can leave a
session transport-alive while its control stream is unusable.

The 75-second stream-establishment value is the upstream yamux default and is
distinct from the adapter's caller-visible 60-second `OpenStream` bound.
yamux starts this timer after sending a stream SYN, waits for the peer's ACK,
and closes the entire session on expiry. The ACK shares one TCP ordering domain
with bulk stream data. A five-second library timer therefore tears down a
healthy session when loss and concurrent bulk data delay the ACK through
head-of-line blocking; this reproduced in the second staging qualification at
concurrency eight. A later qualification proved that the caller-visible open
can also remain blocked beyond five seconds while six bulk streams occupy the
same lossy high-RTT TCP connection. Retain the 75-second library timer and use
a separate 60-second adapter call bound: it is above the legitimate observed
delay but remains below the library timer, so the adapter still captures
`ErrOpenTimeout`, closes the session, and joins the stuck open before yamux's
asynchronous teardown wins. Emit a fixed-cardinality structured warning with
`event=yamux_stream_open_timeout transport=tcp` for the library expiry.

The five-minute close value is a correctness boundary, not a liveness
interval. `yamux.Stream.Close` starts this timer after an ordinary FIN; expiry
discards the peer's unread buffered tail through RST. A five-second value
reproduced `connection reset` locally, matching the mechanism that can surface
as the `unexpected EOF` seen on a 100 MiB lossy staging download. B4.4b
confirmed that causal link on staging. Normal cooperative FIN exchange still
completes `Done` immediately; only an abandoned stream retains resources for
the fallback interval.

In yamux v0.1.2, `AcceptBacklog` sizes both the incoming accept queue and the
local in-flight-SYN semaphore. It is not an active-stream or memory limit.
Keep `max_streams_per_session <= AcceptBacklog`; both ship as 64.

yamux does not offer a context-aware open operation. Its 75-second
`StreamOpenTimeout` starts only after a local SYN slot is acquired and closes
the entire session asynchronously if the peer never acknowledges; it does not
bound the `OpenStream` call itself. The adapter must therefore:

1. Derive an adapter-owned 60-second timeout. This is intentionally distinct
   from QUIC's five-second bound in Section 7.2 and below yamux's 75-second
   stream-establishment timer.
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

- Resolution order is
  `BEAMD_TRANSPORT` (when present) > explicit account/profile `Transport` >
  the product default below. A present empty or invalid environment value is
  fatal.
- When `Transport` is omitted, a hosted account whose persisted
  `Account.Kind` is `session` resolves to `auto`. A self-hosted/API-key account
  whose kind is `token`, a legacy account with a missing or unknown kind, and a
  standalone client config all resolve to `tcp`. `Kind` is local product
  metadata, not a server-side authorization boundary.
- The `session => auto` behavior may ship in the common binary before hosted
  activation because the hosted edge listener remains explicitly disabled
  until B4 and the production-link pilot pass. Release coordination must avoid
  imposing repeated fallback delays on unattended hosted agents while that
  listener is disabled.
- `auto` is the hosted production pilot and normal hosted production mode
  because it retains TCP fallback. Forced `quic` is for `beamd check`,
  qualification, and diagnosis; do not use it as the unattended
  production-agent mode.
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
- the fixed raw-TLS-handshake gate plus pre-authentication and
  authenticated-session semaphores
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
   transport-specific open timeout.
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
| QUIC stream-open timeout | 5 seconds |
| TCP/yamux stream-open timeout | 60 seconds |

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
five-minute raw close timeout. The wrapper releases its lease no earlier than
one second after that fallback. Releasing the lease at `Close` would make the
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
- The bounded 128 raw TLS handshakes, 32 pre-auth sessions, control streams,
  TLS/TCP/UDP buffers, HTTP buffers, goroutine stacks, traffic/request-log
  queues, certificate state, and library bookkeeping are additional memory.

The 512 MiB yamux and 512 MiB QUIC figures are separate ceilings and may
coexist; neither is a total-process-memory promise. Holding leases through
`Stream.Done`, bounding raw handshakes, pre-auth and authenticated sessions,
and running with the production `GOMEMLIMIT` are all required parts of the
resource bound.

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

Server configuration precedence is normative:

```text
compiled defaults < YAML < present environment override
```

Initialize defaults before YAML unmarshalling so an omitted boolean retains
the shipped default while an explicit YAML `false` is honored. Apply
environment overrides after YAML and before validation; in particular,
`BEAMD_DISABLE_QUIC=true` must override `disable_quic: false` so the global
rollback works. A present-empty or invalid environment override is fatal.

| YAML field | Default |
| --- | --- |
| `listen_quic` | Same host and numeric port as `listen_https` |
| `disable_quic` | `true` permanently as the compiled/self-hosted default |
| `max_streams_per_session` | `64` |
| `max_streams_total` | `128` |
| `max_pre_auth_sessions` | `32` |
| `max_sessions_total` | `8` |

Validation after all overrides:

- When QUIC is enabled, its address must parse as a UDP listen address. Disabled
  mode skips this QUIC-only validation as required by Section 7.3.
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
- `disable_quic: true` is both the permanent compiled/self-hosted default and
  the hosted rollback switch. Startup with only the tuned TCP path is a normal
  self-hosted state and must not be worded as "degraded." Hosted deployment
  checks, rather than a global startup warning, detect an unintended disabled
  listener after hosted activation.

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
default. Environment changes take effect after an edge restart.

The yamux window and stream-total settings are coupled by the 512 MiB exposure
limit. Produce an error that names both effective values and the permitted
maximum. Common compatible ceilings are:

| Yamux window | Maximum `max_streams_total` | Maximum `max_streams_per_session` with the other constraints |
| ---: | ---: | ---: |
| 4 MiB (default) | 128 | 64 |
| 8 MiB | 64 | 64 |
| 16 MiB | 32 | 32 |

An operator increasing the window to 16 MiB must lower both stream settings;
the setting remains tunable, but startup must not silently exceed the memory
budget.

Example self-hosted YAML:

```yaml
listen_https: ":443"
listen_quic: ":443"                  # Part B
disable_quic: true                   # compiled/self-hosted default
max_streams_per_session: 64          # Part B
max_streams_total: 128               # Part B
max_pre_auth_sessions: 32            # Part B
max_sessions_total: 8                # Part B
```

The hosted deployment uses the same binary and explicitly sets
`BEAMD_DISABLE_QUIC=false` (or `disable_quic: false`) only after B4 and the
production-link pilot pass. It must continue to carry the environment kill
switch so rollback does not require a new binary.

The Part A process environment is configured separately:

```text
BEAMD_YAMUX_STREAM_WINDOW_BYTES=4194304
```

### 11.3 Part B client/profile configuration

Part A adds no client/profile field. Add `Transport` only with Part B:

```go
Transport string `yaml:"transport,omitempty"`
```

An explicit setting is unchanged:

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
account-derived clients. That resolver implements this permanent product
split when the field is omitted:

| Client source | Empty `transport` resolves to |
| --- | --- |
| Hosted device-login account (`kind: session`) | `auto` |
| Self-hosted/API-key account (`kind: token`) | `tcp` |
| Legacy/ambiguous account (missing or unknown `kind`) | `tcp` |
| Standalone explicit client config with no `kind` | `tcp` |

An explicit `transport` value wins over the kind-derived default, and
`BEAMD_TRANSPORT` wins over both. Login must persist `kind: session` for hosted
device-code login and `kind: token` for pasted-token/self-hosted login; it need
not persist a redundant `transport` value. Refreshing/re-logging into an
existing account must preserve that account's non-empty explicit `Transport`
pin. This keeps existing and ambiguous credentials conservative while allowing
the hosted product to choose QUIC without a separate binary. Managed paid
automation/API-key configs cannot be distinguished safely from OSS token
configs, so their generator must persist `transport: auto` explicitly; a
bearer token or hostname must never be used to infer the product tier.

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
beam_transport_capacity_rejections_total{scope="tls_handshake|preauth_session|authenticated_session|session_stream|global_stream"}
beam_transport_stream_capacity{scope="session|global"}
beam_yamux_stream_window_bytes
```

Labels must come only from the fixed sets above. Do not label by remote
address, slug, tunnel, raw error, or QUIC connection ID.

Always expose both fixed listener series. TCP is `1` after its listener is
ready. QUIC is `1` only while its enabled listener is ready and is `0` when
disabled or down; do not omit the series. `beam_transport_stream_capacity` is
the configured ceiling (`max_streams_per_session` or `max_streams_total`), not
remaining slots; subtract the corresponding active gauge when an operator
needs remaining capacity.

Use `quic.Transport.ConnContext` to observe server-side QUIC attempts that fail
before `Listener.Accept` can return them. Mark an attempt accepted before
dispatch; if its context closes first, increment exactly one fixed-category
handshake error. An accepted connection is accounted through the normal
pre-auth/session lifecycle and must not also increment a handshake error when
it later closes.

`beam_transport_sessions_total` increments only after `hello_ok`, when a
session transitions into the authenticated state. Pre-auth attempts are
represented by the active gauge and handshake-error counter, so rejected
connections do not inflate successful-session totals.

`beam_transport_streams_active` counts leased data streams, including yamux
streams retained through the full five-minute raw close fallback and the
adapter's one-second accounting guard; it excludes control streams.
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
  validation, including conflicting YAML/environment values and present-empty
  overrides;
- the permanent product-default matrix: omitted transport resolves to `auto`
  only for `kind: session`, while `kind: token`, missing kind, and standalone
  configs resolve to `tcp`; explicit configuration overrides that default and
  `BEAMD_TRANSPORT` overrides both;
- login persists `kind: session` for hosted device-code accounts and
  `kind: token` for pasted-token/self-hosted accounts without requiring a
  redundant persisted transport value, and re-login preserves an existing
  explicit transport pin;
- `max_streams_total=128` is accepted and `129` is rejected;
- QUIC and yamux adapters satisfy the same session contract;
- QUIC `Close`, `CloseWrite`, `Abort`, deadlines, and addresses;
- `CloseWrite -> Abort` escalation, `Abort` before/racing `Close`, `Abort`
  racing a flow-control-blocked `Write`, repeated concurrent terminal calls
  under `-race`, and proof that a deadline timeout alone does not close `Done`;
- normal QUIC `http.Transport` close emits FIN rather than reset, while
  cancellation emits a bidirectional reset;
- an early backend response with a deliberately blocked request-body writer
  proves QUIC `Close` unblocks and never races an in-flight `Write`;
- QUIC stream-open timeout normalization;
- yamux stream open survives a locally blocked SYN beyond the former
  five-second caller bound, then completes without closing the shared session;
- a blocking DNS resolver is bounded by the one full candidate context,
  numeric dialing retains the original SNI, multiple resolved addresses share
  one budget, and failed address attempts leak no UDP sockets;
- caller cancellation propagates unchanged instead of becoming
  `ErrOpenTimeout`;
- yamux accept cancellation uses `AcceptStreamWithContext` without closing the
  session;
- candidate failure closes and joins the failed session before fallback;
- terminal auth/certificate/protocol errors never fall back;
- `CloseInfo` covers local/remote application close, idle timeout, stateless
  reset, transport/socket error, and yamux local/remote close; simultaneous
  terminal events are first-wins and the session close metric/log emits once;
- session/global lease acquisition and exactly-once release;
- killing a session with idle, active, and prefix-failed child streams closes
  every child `Done` before `Session.Done` is observable and releases every
  lease/gauge; race this against both open and accept registration;
- canceled yamux streams retain their leases through remote EOF or the
  five-minute raw close timeout plus the adapter accounting guard, and a
  graceful FIN with a buffered unread tail remains byte-exact beyond the
  former five-second boundary;
- capacity errors map to 503 with `Retry-After: 1`;
- other stream-open errors map to 502;
- the 129th concurrent raw TLS handshake, 33rd pre-auth session, and ninth
  authenticated session are rejected without leaking slots;
- readiness is emitted only after all enabled listeners bind;
- `disable_quic=true` reaches TCP readiness while the UDP port is occupied and
  while QUIC key files are missing or malformed, without touching those files;
- an accept result queued during shutdown is closed rather than dispatched;
- stale route identity is rejected after lease acquisition;
- prefix length/read/write deadlines and backend-dial timeout are enforced;
- selected transport appears in daemon health and CLI JSON;
- protocol version mismatch is rejected on both sides;
- control EOF/error is session-terminal, and a second agent-opened stream is
  rejected over both adapters even when raced immediately after control close;
- successful authenticated data-stream reads and writes keep the session live
  while control heartbeats are TCP head-of-line blocked, but an open stalled
  stream does not prevent the 60-second idle close;
- fallback reason classification uses only the fixed metric labels;
- QUIC key files are created atomically, mode 0600, reused, and rejected when
  malformed;
- shutdown closes listeners, drains requests, closes pre-auth sessions, and
  force-closes a stalled raw TCP/TLS handshake without leaving accept,
  handler, or copy goroutines.

### 15.2 End-to-end matrix

Run the current end-to-end behavior once with forced QUIC and once with forced
TCP:

- public TLS ALPN serves a real HTTP/2 request and the forced HTTP/1.1 path
  remains healthy;
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
production link. The accepted G1 run used real beamd edge and agent processes
inside the controlled topology and proved the corrected mixed-load A2 symptom;
it disproved the original solo-transfer hypothesis. B4.5 separately validates
the qualified implementation on the real production link before hosted
activation. It does not gate or change the permanent self-hosted defaults.

The network impairment must apply only to the agent-to-edge leg. Do not shape
the public test client's connection to the edge, or the results conflate two
links.

`scripts/perf-netem.sh` should create Linux network namespaces and a veth pair,
run the edge and agent on opposite sides, and shape both directions explicitly
with these profiles:

The host kernel and selected `tc` must both support deterministic `netem seed`
configuration. Select a non-system build only through an absolute `TC_BIN`
path. Before topology setup, the harness must verify the selected userspace
parser, copy that binary into the new evidence directory, and use the immutable
recorded copy for every qdisc mutation and snapshot. The first real seeded
qdisc must fail closed if the kernel lacks support, and every analyzer-validated
snapshot must retain the exact requested seeds.

| Profile | One-way delay | Loss | Rate |
| --- | ---: | ---: | ---: |
| `clean` | 75 ms | 0% | 100 Mbit/s |
| `lossy` | 75 ms | 1% random | 100 Mbit/s |
| `high-rtt-clean` | 250 ms | 0% | 20 Mbit/s |
| `high-rtt-lossy` | 250 ms | 1% random | 20 Mbit/s |

For each profile and transport, record JSON containing size, direction,
concurrency, TTFB, elapsed time, throughput, checksum result, selected
transport, and every warm-up sample. Run five unmeasured warm-ups per case.
They remain excluded from statistics but are not discarded from evidence. If
any warm-up fails, retain the full attempted warm-up batch, skip measured
iterations, classify the failure, and exit the case nonzero. Then run at least
50 measured iterations for 36-byte, 253 KiB, 257 KiB, and 1 MiB cases; 20 for
16 MiB; and five for 100 MiB.

The per-operation deadline is part of the frozen workload and must be recorded
in metadata. Use 20 minutes by default. Use 60 minutes for 16 MiB and 100 MiB
protocol cases in `high-rtt-lossy`; this remains a hang guard, not a performance
gate. The former uniform 20-minute deadline was below the expected completion
time of a healthy 100 MiB direct-QUIC transfer on that profile: the preceding
16 MiB samples took approximately 3.6–4.5 minutes each, and the first 100 MiB
warm-up reached the deadline. The longer bound must not relax the zero-error,
zero-corruption, checksum, sample-count, throughput, or tail-latency gates.

For every gated case, establish a same-direction, same-payload,
concurrency-one direct baseline over the same shaped veth: one raw QUIC stream
for the QUIC case and one TLS/TCP connection for the yamux/TCP case. Each
fixture uses one warmed long-lived connection, the same certificate/trust,
TLS version, QUIC flow-control configuration, qdisc, endpoints, CPU limits,
and direction as beamd. Establish and warm the connection before measurement;
handshake/DNS time is recorded separately and excluded from transfer samples.
The direct fixture excludes only beamd framing, multiplexing, and reverse
proxying. Do not compare QUIC against a TCP or iperf baseline when measuring
protocol overhead.

Separately, compare beamd-over-QUIC directly with beamd-over-tuned-yamux on the
same host, impairment profile, direction, payload, and concurrency. Use at
least three recorded deterministic netem seeds per gated profile and
counterbalance transport order (`QUIC,TCP` then `TCP,QUIC`, or equivalent)
rather than running every sample of one transport first. Summaries aggregate
the same seed/order blocks. This head-to-head comparison decides whether QUIC
is activated for hosted/session accounts.

Freeze the primary mixed-load workload rather than inheriting mutable script
defaults:

- one warmed tunnel session for each transport/profile/direction;
- six continuous concurrent 8 MiB bulk streams on that same tunnel;
- a five-second bulk ramp before interactive measurement;
- sequential 4 KiB and 65 KiB interactive requests, eight warm-ups and at
  least 50 measured samples per condition;
- `condition=baseline|underload` recorded separately from
  `transport=tcp|quic`;
- baseline and under-load cases for download and upload separately, with the
  interactive and bulk traffic moving in the gated direction;
- zero request errors, corruption, timeouts, or missing cases, enforced by a
  fail-closed analyzer.

Run every beamd fixture process with an explicit, harness-created empty HOME
directory with mode `0700`; never inherit the invoking operator's HOME. This
keeps accounts, global naming defaults, agent state, and credentials out of
the measurements and permits unattended runners whose service environment has
no HOME. Record
`runtime_environment.beamd_home_isolated=true` and
`runtime_environment.beamd_home_inherited=false`; the analyzer must reject
qualification evidence without both assertions.

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
- Head-to-head A2 gate (primary — the problem QUIC is built to fix): for every
  lossy profile/direction where tuned yamux reproduces the defect (interactive
  4 KiB under-load p95 is at least 3x baseline and at least one second), QUIC
  must cut that under-load p95 by at least 50%. At least one lossy profile must
  qualify in each direction or the result is inconclusive, not a pass. QUIC
  must not regress any other lossy profile's under-load p95, or the clean-path
  interactive-under-load p95, by more than 10%.
- Head-to-head A2 gate (secondary guardrails): QUIC must not regress solo
  small-response p95 or solo large-transfer throughput by more than 10% — these
  were already healthy on tuned-yamux, so they are guardrails, not the target.
- The QUIC small-response distribution must not introduce a recurring
  1/2/4-second or other timer-backoff ladder. Tuned TCP did not reproduce the
  originally alleged ladder, so this is a guardrail rather than an expected
  before/after win.
- All functional sizes, directions, and concurrency cases: zero corruption
  and zero hangs.

Store raw JSON and a summary beside metadata containing the beamd commit,
Go/quic-go/yamux versions, kernel and OS, CPU/RAM/container limits, interface
offload state, the exact `tc` version and binary hash, exact `tc qdisc` output,
effective beamd configuration, direct fixture settings, frozen workload
values, netem seeds, transport order, isolated-HOME policy, and whether
handshake time was included (it must be `false` for transfer gates). Persist
the actual beamd, fixture, analyzer, harness, and `tc` binaries or their
manifest-verified hashes.

Before a fail-closed exit caused by a tagged warm-up or measured request error,
corruption, or unsuccessful raw sample, persist the fully tagged offending
record to `raw-failures.jsonl`. This file is diagnostic evidence only and must
never be fed into or used to relax the passing analyzer matrix. Error samples retain
elapsed time and partial payload bytes; for downloads, bytes means response-body
bytes read, while for uploads it is an atomic snapshot of request-body bytes
consumed by `net/http` when `Client.Do` returned and is not a wire-delivery
guarantee. Background bulk-load failures retain their progress snapshot and
console log instead of being represented as serial request records.

`MODE=targeted` is the reusable incident-recheck path. It retains the clean
manifest, deterministic `tc`, isolated HOME, host-resource, topology, qdisc,
configuration, log, and checksum requirements, but runs exactly one profile,
seed, direction, and workload for both ordered transports. Protocol targets
run one payload through the direct fixture and beamd. Mixed targets run the
frozen beamd baseline and under-load cases at both interactive sizes with the
same six-stream bulk load used by qualification; a direct mixed fixture does
not exist. It writes `targeted-summary.json` and exits without
invoking the full B4 analyzer; targeted evidence can establish a cause/fix but
can never substitute for the complete qualification. Reusing a seed reproduces
the same impairment parameters, not the same packet-loss trace: transport
order and preceding traffic advance netem's seeded PRNG state. Targeted serial
clients are fail-fast at request granularity. A targeted concurrent beamd case
is fail-fast at phase granularity: it retains the complete concurrent warm-up
or measurement batch containing the first failure, then stops that case. When
the test infrastructure remains viable, case failures are accumulated through both
transports before final qdisc, process-memory, configuration, manifest,
integrity, stage-status, and summary artifacts are written; the final exit
remains nonzero. Its inputs are:

```text
TARGET_PROFILE=lossy
TARGET_SEED=101
TARGET_DIRECTION=download
TARGET_WORKLOAD=protocol
TARGET_SIZE_BYTES=104857600
TARGET_TRANSPORTS="tcp quic"
TARGET_BEAMD_CONCURRENCY=1
TARGET_WARMUPS=5
TARGET_ITERATIONS=5
```

The direct fixture remains concurrency one. `TARGET_BEAMD_CONCURRENCY` accepts
1 through 64 for protocol targets; omitting `TARGET_ITERATIONS` retains the
normal size/profile matrix count. With `TARGET_WORKLOAD=mixed`, payload size and
beamd protocol concurrency do not apply; the harness binds the frozen 4 KiB and
65 KiB interactive sizes, concurrency-one probes, six 8 MiB bulk streams, and
five-second ramp, while `TARGET_WARMUPS` and `TARGET_ITERATIONS` select the
interactive batches. Targeted metadata and verification bind every applicable
value so a serial protocol recheck cannot be mistaken for a mixed-load
regression test.

The block-40 incident recheck is:

```text
TARGET_WORKLOAD=mixed
TARGET_PROFILE=high-rtt-lossy
TARGET_SEED=101
TARGET_DIRECTION=upload
TARGET_TRANSPORTS="quic tcp"
TARGET_WARMUPS=8
TARGET_ITERATIONS=50
```

The netem suite is a manual or scheduled privileged job, not a required
unprivileged pull-request job. Passing it is necessary but not sufficient for
hosted activation; the B4.5 production-link pilot is also required.

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
- [x] **A1.8 — Release independently.** Commit `f901bb5` was deployed edge-first
  and is live on staging, OSS, and production edges plus the agent. The operator
  accepted the release as complete on 2026-07-25 after the observed
  responsiveness improvement. A dedicated artifact containing both 16 MiB
  directions and memory observation was not captured; that historical evidence
  gap is recorded and is not a Part B prerequisite because B4 repeats the
  stricter bidirectional and memory validation before hosted activation.

Part A is complete after A1.1–A1.8. The measurement gate below is a separate
decision task, not part of A1 completion.

### Measurement gate — prove A2 before Part B

> **Status (2026-07-25): GO — Part B implementation operator-approved,
> default-off.** Two measured axes (real beamd edge + shaped agent; complete
> committed cases with zero errors/corruption):
> **(1) bulk throughput** shows no A2 penalty (solo ~97% of 8-stream aggregate,
> no timeout ladder) — `g1-local-2026-07-24/`; **(2) interactive latency under
> mixed load** is SEVERELY degraded — a 4 KB request inflates 10× (clean) to
> ~38× (bursty loss), reaching 1.7–7.7 s once bulk shares the one tunnel TCP
> connection (`hol-2026-07-24/`). That is the shared-connection head-of-line
> problem QUIC fixes (§2). Full decision + caveats (QUIC fix expected but B4
> must confirm before hosted activation; load-dependent; controlled reproduction):
> `test/perf/results/decision-2026-07-24-g1.md`. The controlled reproduction is
> accepted as the implementation gate. Remote-edge confirmation remains
> optional rigor; B4.5 is the required production-link gate for activating
> QUIC in hosted production.

- [x] **G1.1 — Establish the tuned-TCP baseline.** Ran real A1 beamd edge and
  agent processes with impairment applied before dial and recorded the qdiscs
  and topology under `g1-local-2026-07-24/`. The binary did not expose its
  commit (`beamd: n/a`), so the decision record supplies `f901bb5`; B4 must
  satisfy the complete metadata contract in Section 15.3.
- [x] **G1.2 — Exercise the solo small-response control.** Measured 36-byte and
  253 KiB sequential/concurrent downloads across clean, lossy, and high-RTT
  loss. Clean/lossy used 100/50 samples; the expensive high-RTT profile used
  60/20. The alleged solo timeout-collapse signal was negative.
- [x] **G1.3 — Exercise the large-transfer control.** Measured concurrency-one
  versus eight with checksums using 8 MiB on clean/lossy and 512 KiB on the
  severely throughput-limited high-RTT-loss profile. Solo throughput remained
  ~97% of aggregate under loss. This reduced download control disproved the
  original hypothesis; the comprehensive 16/100 MiB bidirectional matrix is a
  B4 guardrail, not a reason to delay implementation.
- [x] **G1.3b — Exercise the mixed-load head-of-line signature (PRIMARY).**
  Saturate the tunnel with several concurrent bulk downloads (many visitors) and,
  on the same tunnel, measure a small interactive request's latency ALONE vs
  UNDER that load, across clean/wifi/mobile/bursty-loss profiles
  (`scripts/perf-hol.sh`). Interactive tail latency inflating under load —
  worst with loss — is the defect QUIC targets and the signal that decided G1.
  The completed run recorded all 16 planned profile/condition/size cases with
  50 samples and zero errors.
- [x] **G1.4 — Record comparable statistics.** Saved raw JSON, p50/p95/p99,
  maximum, throughput, errors, qdisc state, and analyses under
  `test/perf/results/{g1-local,hol}-2026-07-24/`. Explicitly recorded that the
  approximately 1/2/4-second timeout ladder did not reproduce.
- [x] **G1.5 — Make and record the decision.** Part B is justified when a
  repeatable transport defect remains after A1. The **primary** signal (the one
  that actually fired, 2026-07-24) is interactive latency under mixed load: a
  small request's p95 inflating severely (≥3x, into seconds) when concurrent
  bulk shares the one tunnel connection (`scripts/perf-hol.sh`). The
  solo-transfer signals — solo small-response p95 over one second and over twice
  the concurrency-eight p95, a visible 1/2/4 s timeout ladder, or solo
  large-transfer throughput below 70% of the eight-stream aggregate — are
  supporting evidence but are NOT sufficient on their own: all three came back
  negative here while the head-of-line penalty was severe. Save raw results and
  a dated go/no-go under `test/perf/results/`. The dated GO was recorded on
  2026-07-24 and operator-approved on 2026-07-25.
- [x] **G1.6 — Apply the stop condition.** Not triggered: G1.3b proved a severe
  repeatable defect, so the recorded decision authorizes B1–B4 with QUIC
  default-off and preserves B4 as the hosted-activation gate.

### Part B, Change 1 — abstraction and generic-session guardrails

- [x] **B1.1 — Add `internal/tunnel`.** Implement the interfaces, normalized
  errors, close information, and yamux adapter from Section 6.
- [x] **B1.2 — Move callers behind the abstraction.** Remove concrete yamux
  types from client and edge code while keeping production forced to TCP.
- [x] **B1.3 — Add the Part B yamux hardening.** Implement Section 8.2,
  including bounded open/accept behavior and lifecycle-safe stream cleanup.
- [x] **B1.4 — Add resource admission.** Implement session/global leases,
  the fixed raw-TLS-handshake gate, pre-auth/authenticated session limits,
  stale-route checks, session-child lifecycle ordering, exact release
  semantics, and memory-product validation from Section 10.
- [x] **B1.5 — Prove no behavioral change.** Pass the existing functional,
  cancellation, shutdown, capacity, race, and TCP performance tests with the
  abstraction in place; retain G1 as the pre-QUIC TCP performance baseline.

### Part B, Change 2 — QUIC engine, default off

- [x] **B2.1 — Add and pin quic-go.** Add `github.com/quic-go/quic-go`
  `v0.60.0` and record the Go-version requirement.
- [x] **B2.2 — Implement QUIC transport.** Complete the listener, dialer,
  context-bounded DNS/numeric dialing, abort-escalatable stream adapter,
  control-stream invariant, TLS/ALPN, flow control, key persistence, and
  lifecycle requirements in Sections 6 and 7.
- [x] **B2.3 — Add dual-transport tests.** Run the shared session contract and
  full HTTP/streaming/WebSocket/reconnect/cancellation suite over forced QUIC
  and forced TCP.
- [x] **B2.4 — Keep QUIC unreachable by default.** Ship the edge with
  `disable_quic: true` and the generic/self-hosted agent with
  `transport: tcp`; use forced QUIC only in tests and explicit `beamd check`
  qualification.

### Part B, Change 3 — selection, flags, diagnostics, and rollback

- [x] **B3.1 — Implement transport modes.** Add `tcp`, `auto`, and `quic`
  selection with the exact fallback classification, cleanup, reconnect, and
  re-probe behavior in Section 9.
- [x] **B3.1a — Implement permanent product-aware defaults.** Resolve an
  omitted transport to `auto` only for hosted `kind: session` accounts and to
  `tcp` for `kind: token`, missing/unknown-kind legacy accounts, and standalone
  configs. Preserve explicit/account values (including across re-login) and
  the `BEAMD_TRANSPORT` precedence; keep the server's compiled
  `disable_quic: true` default. Exercise the complete resolution and
  login-persistence matrix in tests.
- [x] **B3.2 — Implement the two rollback controls.**
  `BEAMD_DISABLE_QUIC=true` plus an edge restart is the global kill switch;
  `BEAMD_TRANSPORT=tcp` plus `beamd reload` is the local-agent override.
  Prove the edge kill switch bypasses all UDP/key initialization.
- [x] **B3.3 — Make `auto` the only production pilot mode.** It must prefer
  QUIC and fall back to tuned TCP. Forced `quic` remains diagnostic and must
  never silently fall back.
- [x] **B3.4 — Add diagnostics.** Implement `check`, `status`, health fields,
  fixed-label metrics, structured logs, selected-transport reporting, and
  protocol-version enforcement.
- [x] **B3.5 — Rehearse rollback.** With an established `auto` agent, enable
  the edge kill switch and restart the edge; verify the agent reconnects over
  TCP without changing its configuration. Separately verify the local
  `BEAMD_TRANSPORT=tcp` override.

### Part B, Change 4 — deployment, qualification, and hosted activation

- [ ] **B4.1 — Prepare production networking.** Publish UDP 443, update
  Docker/firewall configuration, persist required UDP sysctls, and document
  memory and macOS socket-buffer guidance.
- [x] **B4.2 — Build the qualification harness.** Implement Section 15.3 and
  its fail-closed validation, frozen mixed-load workload, direct baselines,
  counterbalanced transport order, and complete metadata under
  `test/perf/results/`.
- [x] **B4.3 — Pass functional qualification.** Complete the Section 15.2
  matrix over both transports with zero corruption, hangs, or semantic
  regressions.
- [x] **B4.3a — Complete the pre-production staging rehearsal.** On
  2026-07-27, deploy matching `90acefa` edge and agent binaries, publish both
  TCP and UDP 443, apply the UDP sysctls and key permissions, and pass forced
  TCP, forced QUIC, and `auto` checks; HTTP/1.1 and HTTP/2; forwarded headers;
  SSE; 16 MiB upload/download checksums; WebSockets; edge restart; rollback;
  suspend/reap/resume/registration replay; and a sleep-inhibited ten-minute
  real-link soak. The final soak completed 60/60 HTTP/2 probes plus ten
  WebSocket probes with zero failures, session closes, heartbeat timeouts,
  stream-open errors, or capacity rejections; edge RSS ended at about 19 MiB.
  This is staging evidence only. The host used for that rehearsal was later
  resized; the current qualification host has two online CPUs and Linux
  `MemTotal=2063216640`, satisfying the Section 17 synthetic-run floor. More
  CPU or RAM is not expected to correct transport-semantic failures.
- [ ] **B4.4 — Pass synthetic protocol and head-to-head performance gates.**
  In the deterministic Section 15.3 harness, QUIC must pass its direct baseline
  gates, stay within the clean-path regression budget, materially beat tuned
  yamux on every qualifying A2 profile/direction, and introduce no recurring
  timer-backoff ladder. Three full attempts are partial evidence, not B4 verdicts:
  the 2026-07-27/28 run stopped after 13 of 48 completed blocks when two of five
  measured lossy/TCP/100 MiB downloads returned `unexpected EOF`; the
  2026-07-29/30 restart also stopped after 13 completed blocks, this time in
  lossy/seed-101/download/TCP at 16 MiB and concurrency eight. The second
  failure's warm-up batch killed the route, but the harness discarded warm-up
  results, so all eight measured requests surfaced only the downstream
  `status 404` symptom. The matching QUIC concurrent case passed. The
  2026-07-30 through 2026-08-02 restart cleared both defects and retained 725
  error-free records through 36 completed blocks. Block 37 stopped on the first
  direct-QUIC 100 MiB `high-rtt-lossy` warm-up at the harness's uniform
  20-minute per-operation deadline; the preceding 16 MiB direct samples took
  approximately 3.6–4.5 minutes. The remaining attempts saw the fixture's
  resulting normal connection close. No yamux stream-open timeout, route loss,
  corruption, resource pressure, or product-process failure occurred.
- [x] **B4.4a — Correct the qualification-discovered TCP truncation locally.**
  Restore yamux's raw close fallback to five minutes, keep the wrapper/lease
  fallback strictly after raw cleanup, add the byte-exact delayed-drain
  regression, and preserve elapsed time plus partial byte counts in a separate
  raw failure artifact. Do not weaken any zero-error qualification gate.
  Completed 2026-07-28: the five-second failure reproducer fails with
  `connection reset`, the unchanged delayed-drain test passes at five minutes,
  parent-close/timer ordering passes 100 race-detector repetitions, and the
  complete Go/race/vet/package checks pass.
- [x] **B4.4b — Confirm the first correction on staging.** Deploy matching candidate
  edge and agent binaries, first repeat the same-seed direct-TCP 100 MiB
  baseline, then rerun the same-parameter lossy/seed-101/download/TCP/100 MiB
  beamd case with five successful checksum-verified samples, followed by its
  matching direct-QUIC and beamd-QUIC controls. Record versions, effective
  configuration, partial-byte diagnostics, logs, and memory through the
  non-qualifying `MODE=targeted` evidence path. Completed before the
  2026-07-29 restart with candidate
  `6aac41a49ad2691b34d6c1b310b50b7ded80c2a9`: all five measured beamd TCP
  100 MiB samples passed under the requested lossy seed-101 profile, as did the
  controls. This is not claimed to replay the original packet-loss sequence.
- [x] **B4.4c — Correct the concurrent TCP timeout and evidence gap locally.**
  Keep the adapter-owned five-second open bound but restore yamux's internal
  stream-ACK establishment timeout to 75 seconds; retain a structured terminal
  cause if that library timer ever expires. Prove the session survives an ACK
  delayed beyond five seconds. Record all direct and beamd warm-ups, fail closed
  before measurement after any warm-up failure, and allow targeted mode to bind
  beamd concurrency, warm-up count, and iteration count. Completed 2026-07-30:
  the delayed-ACK regression survives beyond the former five-second library
  timer; modified packages pass under the race detector; the complete Go suite,
  serial broad race suite, vet, shell/Python syntax, and diff checks pass.
- [x] **B4.4d — Confirm the concurrent correction on staging.** Deploy one
  immutable matching candidate and run
  lossy/seed-101/download/16 MiB with eight warm-ups, eight measured requests,
  and beamd concurrency eight over TCP followed by QUIC. Require zero warm-up
  or measured failures, retained raw samples, exact target metadata, viable
  qdisc/integrity/process evidence, and no route loss. Completed 2026-07-30 on
  candidate `92b9306a41d553e400dcc133f1ba37b78cfcc59f`: all four direct/beamd
  TCP/QUIC stages passed with the exact 16 MiB, concurrency-eight, eight-warm-up,
  eight-iteration inputs and no raw failures.
- [x] **B4.4e — Correct the high-RTT/loss harness deadline locally.** Preserve
  the 20-minute default, use a recorded and analyzer-enforced 60-minute
  per-operation bound for 16 MiB and 100 MiB `high-rtt-lossy` protocol cases,
  and retain every existing fail-closed gate. Completed 2026-08-03 after the
  third full attempt stopped at exactly the former 20-minute deadline with 36
  completed blocks and 725 prior records carrying zero errors or corruption.
- [x] **B4.4f — Correct active-session liveness under TCP head-of-line
  blocking locally.** The exact high-RTT/loss deadline recheck completed direct
  QUIC, beamd QUIC, and direct TCP, then the first beamd-TCP warm-up ended after
  9,745,280 of 104,857,600 bytes. Edge evidence shows the 60-second control
  heartbeat expired while that stream was continuously transferring data; TCP
  ordering had delayed the heartbeat behind the bulk stream. Count successful
  authenticated data-stream reads and writes as session activity while keeping
  stream existence insufficient to suppress the idle timeout. Completed
  2026-08-03 with repeated focused and race regressions, the edge suite, the
  complete Go suite, serial broad race suite, vet, analyzer tests, syntax, and
  diff checks passing.
- [x] **B4.4g — Confirm active-data liveness and restart qualification.** On
  immutable candidate `372a88f03f8247b0c45bac928ca21ae32a47de90`, the exact
  high-rtt-lossy/seed-101/download/100 MiB target passed direct and beamd over
  both QUIC and TCP. The fresh matrix then completed 39 blocks and retained 796
  successful records with zero errors or corruption. Block 40's
  high-rtt-lossy/seed-101/upload/TCP mixed case failed during the second 4 KiB
  under-load warm-up: the first warm-up took 25.97 seconds, the next stream open
  reached the shared five-second adapter bound and returned 502, and the closed
  session made the remaining requests return 404. There was no OOM, resource
  pressure, corruption, heartbeat expiry, or kernel fault.
- [x] **B4.4h — Correct the TCP caller-open bound and mixed-target gap locally.**
  Keep QUIC at five seconds. Give TCP/yamux a separate 60-second caller-visible
  bound, still below yamux's 75-second internal establishment timer so stuck
  opens retain adapter-owned terminal cause and join-safe teardown. Prove a
  locally blocked yamux SYN survives beyond the former five-second bound and
  the session remains usable. Extend `MODE=targeted` to bind and fail closed on
  the frozen mixed workload over both ordered transports without claiming a B4
  verdict. Completed 2026-08-08: the focused regression, complete Go suite,
  focused and serial full race suites, vet, analyzer tests, embedded Python and
  shell syntax, and diff checks pass.
- [ ] **B4.4i — Confirm the mixed correction and restart the full
  qualification.** On one immutable candidate, run the exact
  high-rtt-lossy/seed-101/upload mixed workload with eight warm-ups and 50
  measured requests per interactive case over QUIC then TCP. Require all four
  baseline/under-load records per transport, live error-free bulk snapshots,
  no raw failures, and complete manifest/qdisc/config/log/memory evidence. Only
  after it passes, start a fresh counterbalanced 48-block run from block one.
  Do not splice or resume prior evidence: its manifest binds an older candidate
  and its matrix is incomplete.
- [ ] **B4.5 — Pilot in `auto`.** Enable the edge QUIC listener, keep the
  compiled/self-hosted defaults unchanged, run the hosted production session
  in `auto`, validate both directions/WebSockets/reconnect over the real
  production link, and observe metrics and memory.
- [ ] **B4.6 — Activate the permanent hosted policy after the pilot.**
  Explicitly set `disable_quic: false` in the hosted edge deployment; the
  tested `kind: session => auto` resolver is already implemented. Confirm
  hosted sessions and managed paid configs select QUIC, while fresh
  self-hosted/token, missing-kind, and ordinary standalone clients still
  select TCP and a default self-hosted edge does not bind UDP. Permanently
  retain both rollback controls and the tuned TCP path.

> **Implementation status:** B1–B3, the B4 qualification code/functional
> matrix, and the non-production staging rehearsal are complete. The compiled
> and self-hosted defaults intentionally remain TCP with the QUIC listener
> disabled. B4.1 and B4.4–B4.6 are operational rollout gates: apply
> production-host UDP/firewall/sysctl changes, run and retain the privileged netem
> qualification on a conforming Linux host, complete the production-link
> `auto` pilot, and only then activate QUIC in the hosted deployment.

The recorded and operator-approved G1 GO satisfies the prerequisite to begin
B1. Do not execute B4.6 until B1–B4.5, the functional and performance gates,
the product-default matrix, and the rollback rehearsal all pass. After B4.6,
complete the final hosted production validation and Definition of Done.

## 17. Production host requirements

For the current single-user deployment, use one stable public VM with at least
2 vCPU and a nominal 2 GiB-class RAM allocation. Qualification requires at
least two online CPUs and Linux `MemTotal >= 2000000000` bytes; the usable
threshold accounts for memory reserved by the kernel on a nominal 2 GiB VM.
Keep the architecture single-cell; multi-region work is not justified yet.

Required network exposure for the hosted deployment or an explicitly
QUIC-enabled self-hosted deployment:

```text
443/tcp  public HTTPS + TLS/yamux fallback
443/udp  QUIC tunnel transport
```

The tunnel-control hostname must resolve directly to this host. A TCP-only
reverse proxy in front of it cannot carry the QUIC path.

A default self-hosted deployment requires only `443/tcp`; it neither binds nor
requires `443/udp` until its operator explicitly enables QUIC.

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
- after `auto` is enabled for the hosted pilot or hosted session policy, the
  agent unexpectedly selects TCP;
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
flag-day below applies to the **hosted Part B activation** (or an operator's
explicit self-hosted QUIC opt-in). It is not required for the default
self-hosted TCP deployment.

Because there is one user, use a coordinated flag-day deployment:

1. Build the edge, CLI binaries, npm artifacts, and image from one commit.
2. Open host/cloud firewall UDP 443 and add the Docker UDP port mapping.
3. Apply the UDP sysctls.
4. Stop the local agent.
5. Install or stage the matching local `beamd` binary/npm release.
6. Deploy the new edge with the compiled
   `BEAMD_DISABLE_QUIC=true`-equivalent default.
7. Run the staged matching binary's `beamd check --transport tcp` against the
   new edge.
8. Set `BEAMD_DISABLE_QUIC=false` (or `disable_quic: false`) and restart the
   edge. Confirm readiness lists both transports and
   `beam_transport_listener_up{transport="quic"} 1` when metrics are enabled.
9. Run `beamd check --transport quic`.
10. Start/reload the agent in `auto` mode.
11. Confirm `beamd status` reports `transport: quic`.
12. Validate a 16 MiB download, a 16 MiB upload, a WebSocket, and reconnect.
13. Watch transport metrics and memory for at least ten minutes.

If QUIC validation fails but TCP works, set `BEAMD_DISABLE_QUIC=true` on the
edge and restart the edge. Confirm the readiness record lists TCP only and the
fixed QUIC listener gauge is `0`; the restart must not read QUIC keys or touch
UDP. An agent in `auto` must reconnect over TCP without an agent configuration
change. If the edge cannot be changed immediately, or to isolate an agent-side
problem, set `BEAMD_TRANSPORT=tcp` for the agent and run `beamd reload`. Either
control independently restores the old data path; using both is optional.

If correctness fails on both transports, roll back the edge image/binary and
the local binary to the previous tag. Roll back both sides together; do not add
mixed-version compatibility code solely for rollback.

## 19. Definition of done

**Part A is done when:**

- [x] every A1 checklist item is complete;
- [x] an absent `BEAMD_YAMUX_STREAM_WINDOW_BYTES` produces an effective
  4 MiB window on edge and agent;
- [x] every accepted and rejected environment value behaves as specified;
- [x] the edge controls downloads and the agent controls uploads, verified
  with asymmetric values and checksums;
- [x] no unrelated yamux setting or Part B architecture changed;
- [x] startup logs expose the effective value and repository/e2e/package tests
  pass;
- [x] the edge-first rollout and agent reload succeeded; the operator accepted
  the missing dedicated 16 MiB/memory artifact as historical evidence debt,
  with the stricter validation retained in B4.

The G1 measurement is deliberately separate. A1 may be complete and released
even if G1 concludes that Part B is unnecessary.

**Part B is complete only when:**

- [x] a recorded, operator-approved G1 result justifies Part B;
- [ ] every B1–B4 checklist item is complete;
- [x] neither `internal/client` nor `internal/edge` imports yamux or quic-go
  directly;
- [x] the npm shim remains a launcher and contains no transport code;
- [ ] hosted/session accounts select QUIC through `auto` in production while
  self-hosted/token, missing-kind, and standalone clients still default to TCP;
- [ ] the hosted edge explicitly enables UDP 443 while the compiled/default
  self-hosted edge leaves QUIC disabled;
- [x] both the edge-wide and agent-local rollback controls are rehearsed;
- [x] TCP/yamux fallback has a verified 4 MiB default window;
- [x] all HTTP, streaming, WebSocket, reconnect, cancellation, and shutdown tests
  pass over both transports;
- [x] raw-handshake, session, stream, and memory limits are enforced and
  observable;
- [x] `beamd check` can force either transport;
- [x] `beamd status` reports the active transport;
- [ ] Docker and host configuration expose/tune UDP;
- [ ] unit, race, vet, e2e, and netem qualification pass;
- [ ] QUIC stays within every regression budget and materially beats tuned
  yamux on every qualifying lossy profile/direction that reproduces A2;
- [ ] production validation shows no 256 KiB/RTT throughput plateau and no
  concurrency-dependent tail collapse on the QUIC path.

## 20. References

- [quic-go v0.60.0 release](https://github.com/quic-go/quic-go/releases/tag/v0.60.0)
- [quic-go v0.60.0 API](https://pkg.go.dev/github.com/quic-go/quic-go@v0.60.0)
- [quic-go flow-control guidance](https://quic-go.net/docs/quic/flowcontrol/)
- [quic-go stream lifecycle](https://quic-go.net/docs/quic/streams/)
- [quic-go server and listener lifecycle](https://quic-go.net/docs/quic/server/)
- [quic-go UDP buffer guidance](https://quic-go.net/docs/quic/optimizations/)
