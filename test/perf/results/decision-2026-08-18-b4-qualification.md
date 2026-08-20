# B4 qualification decision — 2026-08-18

## Decision

**Hosted QUIC activation is NO-GO.** Keep the hosted QUIC listener disabled,
do not start the production-link `auto` pilot, and retain TCP as the effective
hosted transport. Compiled and self-hosted defaults remain unchanged. Do not
relax the analyzer gates to turn this result green.

This is a performance-policy no-go, not a correctness failure. Candidate
`bfc94f03163fbfb4d83f46e166ceef5dccde12e1` completed the fresh qualification
from 2026-08-09 15:21:19 UTC through 2026-08-18 19:12:25 UTC. The retained
analyzer validated 816 unique cases across three deterministic seeds with every
sample present and error-free. Raw evidence contains 288 direct, 336 beamd
protocol, 192 mixed-load, and 144 live-bulk records. There were zero request
errors and zero corruptions.

## What passed

- Every beamd-QUIC/direct-QUIC baseline gate passed, placing proxy and session
  overhead within budget.
- Every lossy-tail, solo/eight-stream, high-RTT tiny-TTFB, and timer-ladder gate
  passed in both directions.
- The primary A2 objective passed decisively. QUIC reduced the paired under-load
  p95 versus tuned TCP by 96.7% (lossy download), 97.2% (high-RTT/lossy
  download), 95.3% (lossy upload), and 96.7% (high-RTT/lossy upload), against a
  required minimum reduction of 50%.
- The clean and high-RTT/clean mixed-load regression guards passed.

## What failed

The analyzer reported 17 failed checks, including duplicate head-to-head and
solo-guard representations of the same cases:

- On the clean path, 259,072- and 263,168-byte downloads delivered only
  0.832–0.835x TCP throughput and about 1.20x TCP p95. The 259,072-byte upload
  delivered 0.828x throughput and 1.213x p95. These exceed the 1.10 p95 guard.
- High-RTT/clean showed the same threshold signature: 259,072- and/or
  263,168-byte cases were approximately 1.13x TCP p95.
- At 500 ms RTT plus 1% loss, some 263,168-byte and 1 MiB p95 guards failed.
- In that same high-RTT/loss profile, 16 MiB and 100 MiB QUIC throughput was
  only 0.650–0.703x tuned TCP in both directions, below the required 0.90x.

## Diagnosis

The retained direct fixtures reproduce both signatures, while every
beamd-QUIC/direct-QUIC gate passes. The evidence therefore locates the
remaining performance gap below beamd's proxy, framing, stream-adapter, and
capacity layers.

For the large high-RTT/loss cases, beamd and direct QUIC both settle near the
same throughput, as do beamd and direct TCP. The pinned `quic-go v0.60.0`
source constructs its congestion controller with `use Reno = true`, including
after path migration, and exposes no public controller selector. Current
quic-go documentation describes RFC 9002 congestion control, and the upstream
[pluggable congestion-control issue](https://github.com/quic-go/quic-go/issues/5580)
states that New Reno is the default while its CUBIC implementation is disabled.
The qualification host subsequently confirmed
`net.ipv4.tcp_congestion_control = cubic`. The systematic long-transfer gap
against Linux TCP/CUBIC under 1% random loss is therefore consistent with the
controllers below beamd. This is an evidence-backed inference, not proof that
congestion control is the only contributor.

The clean ~256 KiB signature is separate. Pinned `yamux v0.1.2` initializes
each stream at exactly 256 KiB; beamd's 4 MiB setting is the maximum receive
window reached through growth, not the initial window. Direct TLS/TCP and QUIC
fixtures reproduce the medium-size comparison, and the effect changes as TCP
crosses the initial yamux-window boundary in the protocol path. Switching the
QUIC loss controller would not change loss-free slow start by itself. Making
QUIC send roughly a whole 253 KiB response in its initial flight would require
an unusually aggressive startup/pacing change and is not justified by this
evidence.

## Next authorized decision

There is no small beamd configuration correction that safely makes these gates
pass. The next decision must be explicit:

1. prototype and maintain a private quic-go fork (first enabling/evaluating
   CUBIC or another controller), then separately investigate the clean-startup
   guard and rerun B4 from block one;
2. wait for an upstream selectable/pluggable congestion controller and retest;
3. deliberately change the product performance policy with a documented reason.

Until one path produces a fresh passing B4 verdict—or policy is consciously
changed—the reversible, tested implementation stays available default-off and
hosted traffic stays on tuned TCP/yamux.

## Evidence

- Complete bundle: `b4-bfc94f0-20260809T142842Z/`
- Human analyzer output: `b4-bfc94f0-20260809T142842Z/analysis.txt`
- Machine verdict: `b4-bfc94f0-20260809T142842Z/summary.json`
- Run metadata and binary manifest: `b4-bfc94f0-20260809T142842Z/metadata.json`
- Key hashes: `b4-bfc94f0-20260809T142842Z/SHA256SUMS`

The copied `operator/qualification-console.log` and
`operator/qualification-wrapper.sh` are post-run operator artifacts from the
systemd qualification job. They are retained for audit but were not analyzer
inputs.
