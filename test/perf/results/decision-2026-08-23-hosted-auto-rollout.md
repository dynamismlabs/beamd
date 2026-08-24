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

## Follow-up

Track upstream selectable/pluggable congestion control in quic-go. Re-run the
unchanged B4 analyzer from block one for any future transport candidate that
claims to remove the accepted limitations.
