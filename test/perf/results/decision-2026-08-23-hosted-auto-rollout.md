# Hosted `auto` rollout decision — 2026-08-23

## Decision

Authorize a controlled rollout of the completed transport implementation to
the hosted staging and single-user production edges. Both edges will explicitly
enable QUIC while retaining TCP. Hosted session clients will run in `auto`,
which prefers QUIC and falls back to tuned TCP for classified QUIC connection
failures. Forced `quic` remains diagnostic, not a production client mode.

This is an explicit product-policy change under option 3 of
`decision-2026-08-18-b4-qualification.md`. It unblocks the production-link
pilot without claiming that B4 passed and without weakening or rewriting the
retained analyzer or evidence.

## Evidence accepted

- Candidate `bfc94f03163fbfb4d83f46e166ceef5dccde12e1` completed all 48
  qualification blocks: 816 unique cases, every sample present, zero request
  errors, and zero corruption.
- The primary product objective passed decisively. QUIC reduced paired
  under-load p95 latency by 95.3–97.2% on every qualifying lossy direction.
- All dual-transport correctness, direct-baseline, lossy-tail,
  parallel-stream, timer-ladder, reconnect, cancellation, shutdown, and
  rollback checks passed.
- The remaining failures reproduce in the direct fixtures and are below
  beamd's proxy/session layer.

## Known limitations accepted for this rollout

- A solo response around 256 KiB can take approximately 20% longer over QUIC
  than tuned TCP on clean and high-RTT/clean paths.
- A solo 16 MiB or 100 MiB transfer over the synthetic 500 ms RTT plus 1% loss
  profile can deliver only approximately 65–70% of tuned TCP throughput.

These are retained as known limitations and future optimization targets, not
reclassified as passing results. `auto` is a connection-establishment fallback;
it does not switch an already healthy QUIC session to TCP because a particular
transfer would be faster there.

## Rollout constraints

- Promote and build one immutable commit for the edge and local client.
- Stage first: prove forced TCP, forced QUIC, hosted `auto` selecting QUIC,
  HTTP/streaming, a 16 MiB download and upload with exact integrity,
  WebSocket, reconnect, and the edge-wide/local rollback controls.
- Production second: deploy the same release, repeat real-instance functional
  checks, and observe service metrics, logs, and memory for at least ten
  minutes.
- Keep TCP 443 open, the tuned 4 MiB yamux path intact, and both rollback
  controls permanently available.
- Keep compiled and ordinary self-hosted defaults unchanged: edge QUIC remains
  disabled unless explicitly enabled, and token/standalone clients remain TCP
  unless explicitly configured.
- Roll back the edge to `BEAMD_DISABLE_QUIC=true` if forced QUIC, integrity,
  WebSocket, reconnect, listener health, or sustained resource checks fail.
  An `auto` client must then recover over TCP without a configuration change.

## Rollout result

**Complete on 2026-08-23.** Release
`52148835169de58aea705ed61ebcb6aff6fc4647` was fast-forwarded to remote
`main`, built with patched Go 1.25.13, and deployed to both hosted edges. The
Linux artifact SHA-256 was
`81f343ca6db97a41ff3c694aa811899981a41cd31db86bdce78ffdc3f6ae1b70`.
The prior edge binaries remain recoverable as `90acefa` on staging and
`f901bb5` on production.

Release verification passed the complete Go suite, vet, the serial broad race
suite, focused race stress, `govulncheck` with zero reachable vulnerabilities,
OpenAPI drift, four-platform npm build/package smoke, and all seven independent
GitHub Actions jobs. A race-test handshake-ordering flake found by the first CI
run was synchronized in `5214883`; 100 focused repetitions and the rerun passed.

Both native systemd hosts now:

- serve `5214883` on TCP 443 and UDP 443;
- persist `net.core.rmem_max=7340032` and `net.core.wmem_max=7340032`;
- run the 4 MiB tuned yamux fallback and `GOMEMLIMIT=1400MiB`;
- use an isolated transport environment overlay with
  `BEAMD_DISABLE_QUIC=false`; and
- retain the prior binary plus a one-file `BEAMD_DISABLE_QUIC=true` rollback.

Staging passed forced TCP, forced QUIC, hosted `auto => quic`, HTTP/1.1 and
HTTP/2, forwarded headers, five-event SSE, byte-exact 16 MiB download and
upload, WebSocket echo, edge restart/reconnect/route replay, and a full global
rollback. With UDP disabled, the unchanged `auto` agent selected TCP and the
existing HTTP/WebSocket tunnel remained healthy. The separate local
`BEAMD_TRANSPORT=tcp` override selected configured and active TCP; staging was
then restored to `auto => quic`. Edge RSS was approximately 17 MiB after the
exercise, with no capacity or stream-open errors.

Production passed forced TCP, forced QUIC, hosted `auto => quic`, the same
real-link HTTP/SSE/16 MiB/WebSocket matrix, HTTP/2, and route registration from
the operator's Mac. Its two pre-existing detached routes were restored under
the matching agent. `flow-local` returned HTTP 200; `flow-dev` remained HTTP
502 because no process was listening on its pre-existing local port 42241, a
backend condition also present in pre-deployment logs rather than a transport
failure.

From 2026-08-24 01:54:50 UTC through 02:05:00 UTC, 21/21 samples reported both
public health endpoints healthy on `5214883` and both hosted agents healthy
with `configuredTransport=auto` and `transport=quic`. The encrypted SSH key
cache expired after deployment, so the final production journal/RSS snapshot
could not be collected in that window; staging supplied the host-level
resource/log snapshot, while production public health, authenticated transport,
full transfer/WebSocket, and agent diagnostics remained continuously green.

The operator's global CLI is installed from the matching offline-tested
`0.0.8-dev.5214883` Darwin package. This did not publish or tag a public npm
release.

## Follow-up

Track upstream selectable/pluggable congestion control in quic-go. Re-run the
unchanged B4 analyzer from block one for any future transport candidate that
claims to remove the accepted limitations.
