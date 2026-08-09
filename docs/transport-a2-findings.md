# Transport A2 — findings, research, and Part B handoff

**Date:** 2026-07-24 → 2026-08-09
**Status:** **APPROVED / GO** — the operator approved implementing Part B
(QUIC) behind a default-off flag. A1 shipped and Part B is implemented. The
first full B4 run exposed a separate TCP/yamux graceful-close regression; its
correction is confirmed on staging. The next full attempt exposed a distinct
concurrency-eight stream-ACK timeout and a discarded-warm-up evidence gap.
Its narrow correction passed the exact targeted staging recheck. The following
fresh matrix cleared both product defects and completed 36 of 48 blocks with
725 error-free records before a healthy 100 MiB direct-QUIC transfer reached
the harness's undersized 20-minute deadline under 500 ms RTT and 1% loss. The
profile-aware deadline recheck completed direct and beamd QUIC plus direct TCP,
then exposed a distinct edge liveness bug: TCP head-of-line blocking delayed
the control heartbeat while a data stream was actively transferring, and the
edge closed it at 60 seconds. Successful authenticated stream I/O now refreshes
session activity without allowing a merely open stalled stream to mask an idle
session; that correction passed its exact targeted staging recheck. The next
fresh matrix completed 39 of 48 blocks with 796 clean records before a new
mixed-load TCP stream SYN remained blocked beyond the shared five-second caller
bound and the adapter closed the healthy session. TCP/yamux now uses a separate
60-second caller-visible open bound below its 75-second internal establishment
timer, and the harness can target the exact frozen mixed workload. Its exact
recheck completed all eight interactive records cleanly but exposed one more
downstream bound: three background TCP streams reached the agent's five-second
tunnel-name prefix-read deadline while the shared session remained alive.
Prefix exchange now keeps QUIC at five seconds and uses 60 seconds for yamux on
both edge and agent; backend dial remains five seconds. Hosted activation
remains gated on the exact recheck, a fresh complete B4
qualification and the production-link pilot; the self-hosted defaults
permanently remain TCP with edge QUIC disabled.
**Audience:** whoever executes Part B (see `docs/transport-performance-spec.md`
§16 Changes 1–4). Read this first; it is the *why* behind the corrected spec.

---

## 1. TL;DR

- **A1 shipped** (yamux per-stream window 256 KiB → 4 MiB, env-only via
  `BEAMD_YAMUX_STREAM_WINDOW_BYTES`). Commit `f901bb5`, live on all three edges
  (staging/OSS/prod). It removed the window-cap defect and delivered the
  real-world "much snappier" improvement.
- **The spec's original A2 hypothesis was measured FALSE.** It claimed *solo
  transfers are slow under loss and parallel traffic hides it.* On a real beamd
  edge under induced loss, solo bulk throughput is ~97% of the 8-stream
  aggregate — no penalty, no 1/2/4 s timeout ladder.
- **The real defect is head-of-line blocking / bufferbloat.** All of an app's
  streams multiplex over **one** edge↔agent TCP connection. Because TCP delivers
  in order, a latency-sensitive request stalls behind concurrent bulk transfers
  (queued in the send buffer even with no loss; blocked behind a lost packet's
  retransmit with loss). Measured: a 4 KiB request inflates **10× (clean) to 38×
  (bursty loss)** under bulk load, reaching **1.7–7.7 s** on wifi/mobile.
- **That justifies QUIC** (independent per-stream delivery). The operator
  approved building Part B behind a default-off flag; the B4 qualification must
  prove QUIC collapses the under-load interactive tail before it is activated
  for hosted/session accounts. It does not change the self-hosted default.
- **Prior art confirms the direction:** OpenZiti (which powers zrok) hit the same
  wall and built its own UDP transport (westworld3 / dilithium / "Transwarp").
  beamd uses off-the-shelf `quic-go` instead of rolling its own.
- **B4 found and confirmed a distinct TCP graceful-close regression.**
  The new five-second yamux `StreamCloseTimeout` can reset an ordinarily closed
  stream before the peer drains its buffered response tail. Restore yamux's
  five-minute default; the same-parameter targeted staging recheck passed all
  five measured 100 MiB TCP downloads and both protocol controls.
- **The restart found a second, concurrency-only yamux timer defect.** yamux's
  `StreamOpenTimeout` waits for the peer's stream ACK and closes the entire
  shared TCP session. Five seconds is unsafe when eight bulk streams and loss
  delay that ACK through head-of-line blocking. The failure began during the
  discarded warm-up batch, then appeared only as eight measured edge 404s
  after the route vanished. At that stage, beamd kept its independent
  five-second caller bound, restored the internal ACK timer to 75 seconds,
  retained warm-up evidence, and passed the exact concurrent targeted recheck
  before another full run. This
  also does not change the A2/QUIC decision.
- **That correction passed, and the next full run found a harness limit rather
  than another product defect.** The run cleared both former failure points and
  retained 725 records with zero errors or corruption through 36 completed
  blocks. In block 37, the first 100 MiB direct-QUIC `high-rtt-lossy` warm-up
  reached the uniform 20-minute deadline. Its preceding 16 MiB samples took
  approximately 3.6–4.5 minutes, making a 100 MiB completion beyond 20 minutes
  expected. Keep the 20-minute default but use a recorded 60-minute bound for
  16 MiB and 100 MiB `high-rtt-lossy` protocol cases; this changes only the hang
  guard and preserves every performance and zero-error gate.
- **The exact deadline recheck then found an application-liveness defect.** Its
  direct-QUIC, beamd-QUIC, and direct-TCP stages completed, but the first
  beamd-TCP 100 MiB warm-up ended with `unexpected EOF` after 9,745,280 bytes
  and 58.6 seconds. The edge closed the session at 60.0007 seconds without a
  control heartbeat even though stream bytes were continuously arriving. On
  TCP/yamux the heartbeat shares the bulk stream's ordering domain, so
  successful authenticated stream I/O must also refresh the session activity
  timer. An open stream by itself remains insufficient, preserving the idle
  close for dead or stalled sessions.
- **The liveness correction passed, and the next full run found a distinct
  caller-visible yamux open bound.** Candidate `372a88f` passed the exact
  100 MiB high-RTT/loss target over direct and beamd QUIC/TCP. The fresh run
  then completed 39 blocks and 796 clean records. In block 40, six concurrent
  8 MiB TCP uploads delayed a new 4 KiB mixed-load stream SYN beyond the shared
  five-second adapter timeout. The adapter deliberately closed yamux to join
  the otherwise uncancelable open, yielding one 502 and seven later 404s after
  route removal. Keep QUIC at five seconds; use 60 seconds for yamux so
  legitimate TCP head-of-line delay fits while the adapter still expires before
  yamux's 75-second internal timer.
- **The exact caller-open recheck passed every interactive case and exposed a
  residual prefix bound.** Immutable candidate `f973b83` completed the four
  baseline/under-load records per transport, including all TCP 4 KiB and 65 KiB
  warm-ups and 50 measured iterations, with zero request errors or corruption.
  The final TCP bulk snapshot nevertheless failed with exactly three background
  errors, matching three agent `invalid name prefix: i/o deadline reached`
  warnings. The session stayed live through the entire TCP stage, so this was
  the name-prefix read deadline after successful stream open, not another open,
  heartbeat, route, kernel, or capacity failure. Keep QUIC prefix setup at five
  seconds, use 60 seconds for yamux prefix write/read on both peers, and retain
  the independent five-second local-backend dial bound.

---

## 2. What we measured (two axes)

Controlled reproduction on **real beamd processes** — a real `beamd serve` edge
and a real shaped agent in two containers, `netem` applied to the agent→edge leg
**before** the agent dials, `perfclient` driving from the unshaped host. Raw data
and analyzer output are committed under `test/perf/results/`.

### Axis 1 — bulk throughput (`g1-local-2026-07-24/`): no problem
Under loss the shared connection is throughput-limited (~0.3 MB/s at 150 ms,
~0.1 MB/s at 500 ms) and a **solo** transfer gets essentially the whole pipe:

| profile | solo 8 MiB | 8-stream aggregate | solo/agg |
| --- | ---: | ---: | ---: |
| clean (control) | 10.29 MB/s | 11.67 MB/s | 88% |
| lossy 150 ms/1% | 0.29 MB/s | 0.30 MB/s | **97%** |
| high-RTT 500 ms/1%/20 Mbit | 0.10 MB/s | 0.09 MB/s | **111%** |

No 1/2/4 s timeout ladder. **The original A2 hypothesis does not reproduce.**

### Axis 2 — interactive latency under mixed load (`hol-2026-07-24/`): SEVERE
A latency-sensitive request measured **alone** vs **under a bulk load** of 6
concurrent "visitors" on the same tunnel:

| connection | 4 KiB alone (p95) | 4 KiB under load (p95) | inflation |
| --- | ---: | ---: | ---: |
| clean (no loss) | 42 ms | 415 ms | 10× |
| wifi (40 ms/1%) | 96 ms | 1.7 s | 18× |
| mobile (150 ms/2%) | 314 ms | 7.7 s | 24× |
| bursty (60 ms/2% corr) | 71 ms | 2.7 s | 38× |

A 4 KiB request needs ~2 ms of bandwidth, so this is **not** bandwidth
starvation — it is the interactive response queued behind bulk bytes on the one
shared connection. It appears even with **no loss** (bufferbloat) and gets
catastrophically worse with loss (head-of-line blocking). This is the defect QUIC
fixes, and it is what a busy multi-user app actually feels.

**Why the two axes differ and both matter:** A2 is a *per-tunnel* property (each
agent has its own TCP connection), so this holds regardless of user count. What
scales with users is the number of concurrent streams on a given app's one
tunnel — which is exactly the mixed-load condition Axis 2 measures.

---

## 3. Why the spec changed (finding → edit)

The original spec defined and gated A2 around the solo-slow hypothesis, so its
gate criteria came back **negative while the real defect was severe** — i.e. as
written, the gate would have produced the wrong "no-go." Corrections:

| Spec section | Was | Now (and why) |
| --- | --- | --- |
| **§2** A2 definition | "solo transfers slow; parallel hides it" | Head-of-line / bufferbloat on the shared connection, with the measured numbers; notes the old hypothesis was disproven. |
| **§3** goals | "a single response must not be slower because it is the *only* active response" | "a latency-sensitive request must not be slower because *other bulk streams* are concurrently active." |
| **§1 / §16 G1** gate | prove A2 by solo small-response + solo large-transfer collapse | **G1.3b** added: the mixed-load head-of-line test (`perf-hol.sh`) is the **primary** signal; solo tests demoted to controls/guardrails (they were negative here). |
| **§15.3** B4 qualification | QUIC must improve *solo* p95/throughput | Primary gate = QUIC must cut **interactive-latency-under-load p95 ≥ 50%**; solo checks are guardrails, not the target. |

Commits: `f9731f9` (HoL test flips gate to GO), `9627e2e` (correct A2 + gates),
`76c4b17` (consistency sweep). Full decision + caveats:
`test/perf/results/decision-2026-07-24-g1.md`.

**Lesson for the executor:** trust the mixed-load / interactive-latency gate as
the A2 truth. The solo-throughput checks are guardrails only.

---

## 4. Research / prior art — related OpenZiti work behind zrok

zrok is built on **OpenZiti**, a mesh-fabric overlay (routers + links +
circuits), not a single agent↔edge tunnel. Ziti does its own end-to-end flow
control at the "xgress" layer and supports TLS-over-TCP paths, where circuits
sharing a link face the *same* TCP head-of-line problem. Ziti also ships
`xgress_transport` (TCP) and `xgress_transport_udp`.

**The relevant prior art points in the same direction:** OpenZiti built
**dilithium**, a Go framework
implementing the **westworld3** protocol — "high-performance WAN protocols over
UDP datagrams… reliable streaming on top of message-oriented infrastructures" —
integrated into Ziti as **"Transwarp."** It is a custom reliable UDP transport
with QUIC-like properties relevant to this problem; it is not QUIC. Their own
flow-control tuning explicitly benchmarks against "a straight dilithium tunnel"
as the target to beat.

Implications for beamd:
1. **Direction supported** — a leading comparable project independently built
   reliable streaming over UDP to avoid limitations of shared TCP paths.
2. **Use `quic-go`, don't roll our own.** Ziti spent years on westworld/dilithium;
   off-the-shelf QUIC is the pragmatic version.
3. **beamd concentrates the risk.** beamd funnels one app's traffic through one
   tunnel connection, so all of that app's streams share the same TCP ordering
   domain.

These sources do **not** establish zrok's exact current default transport or
rollout policy, so no claim about zrok matching beamd's default-off rollout is
made here. Precisely how Ziti schedules circuits across shared links would also
require a deeper source dive.

Sources:
- OpenZiti Transwarp design doc (westworld3/dilithium):
  https://github.com/openziti/ziti/blob/release-next/doc/transwarp_b1/transwarp_b1.md
- openziti/dilithium: https://github.com/openziti/dilithium
- Xgress flow-control tuning (benchmarks vs. dilithium):
  https://github.com/openziti/fabric/issues/244
- OpenZiti architecture (DeepWiki): https://deepwiki.com/openziti/ziti

---

## 5. Methodology & what to trust

The recorded measurements are usable, with an analyzer-hardening caveat (see the
git history and `decision-2026-07-24-g1.md`):

- **Shape before dial.** An earlier synthetic harness applied `netem` *after* the
  TCP/yamux connection established, which pre-trained TCP on a clean path and
  manufactured retransmissions. The current harness shapes **before** the agent
  dials.
- **Committed evidence is complete and clean.** The recorded Axis 1 and Axis 2
  datasets contain their planned profiles/cases and sample counts, with zero
  transport errors and zero checksum corruption. The committed conclusions can
  be reproduced from their raw JSON and analysis output.
- **Fail-closed analyzers.** `test/perf/analyze.py` enforces the Axis 1 gate.
  `test/perf/hol_analyze.py` was hardened on 2026-07-25 to reject missing or
  duplicate cases, insufficient samples, malformed records, errors, corruption,
  and invalid statistics with a nonzero inconclusive result. It retains
  compatibility with the committed legacy Axis 2 JSON while requiring the
  separate `transport` and `condition` fields for new runs.
- **Real beamd processes**, real edge + real agent, separate containers.

Caveats (carry these into B4):
- **Controlled reproduction, not the production WAN.** Mechanism is faithful; a
  real WAN would be similar or worse. A remote-edge G1 confirmation
  (`scripts/perf-g1.sh` against a real remote edge) is optional additional rigor,
  not a prerequisite for the approved default-off implementation. The B4
  production-link pilot remains required before hosted activation.
- **Load-dependent.** 6 concurrent bulk streams = "busy, not extreme"; the effect
  is smaller under lighter load but present whenever bulk shares the tunnel.
- **A1 interaction.** Part of the *no-loss* 10× is send-side queuing that A1's
  4 MiB window enlarged. A cheap partial mitigation (buffer tuning / yamux stream
  prioritization) could reduce the *clean* case but **cannot** fix the loss-driven
  HoL on wifi/mobile — that needs QUIC. Worth an hour's experiment before the
  full build, but not a substitute for Part B.

---

## 6. State: done vs pending

**Done and committed (local; verify `git log`):**
- A1 implemented, tested, deployed to all three edges (`f901bb5`).
- Measurement + HoL harness (`scripts/perf-{g1,g1-agent,g1-local,hol}.sh`,
  `test/perf/perfserver`, `perfclient`, `analyze.py`, `hol_analyze.py`).
- Gate run + evidence (`test/perf/results/{g1-local,hol}-2026-07-24/`).
- Decision record + corrected spec.
- Operator approval to implement Part B with QUIC default-off; B4 still gates
  hosted activation.

**Pending / optional:**
- Deploy one immutable candidate with the locally verified transport-specific
  yamux caller and prefix-setup bounds plus the mixed-target harness, then
  rerun the exact high-rtt-lossy/seed-101/upload frozen mixed workload over QUIC
  then TCP. Only if it passes, restart the full B4 matrix from block one. The
  prior 39-block evidence cannot be resumed into a verdict because its manifest
  binds the old candidate and the analyzer requires one complete immutable
  matrix.
- Push the local commits (A1 was pushed as `f901bb5`; the perf/spec commits are
  local only — `74579b3`, `d68eb74`, `f9731f9`, `9627e2e`, `76c4b17`).
- Remote-edge G1 confirmation (optional additional rigor; not an implementation
  prerequisite).
- Buffer/prioritization experiment for the no-loss case (optional).
- Housekeeping: a crashed `perf-edge` container + `/root/perf-edge/` remain on
  the OSS box (104.248.61.150); remove when its SSH allows.

---

## 7. Part B execution handoff

Implementation is approved. The compiled/self-hosted edge default remains
QUIC-off permanently. The hosted deployment must also remain QUIC-off until
the B4 qualification and production-link pilot pass.

Follow `docs/transport-performance-spec.md` §16 **Part B, Changes 1–4**, in order:

1. **Change 1 — abstraction.** Introduce `internal/tunnel` (Session/Stream/
   Listener), move yamux behind it. This is the seam deliberately deferred during
   A1. Keep behavior on TCP until green.
2. **Change 2 — QUIC engine, default OFF.** Add `quic-go`, listener/dial/adapters,
   key persistence. Opt-in only.
3. **Change 3 — selection/flags/diagnostics.** `transport: tcp|quic|auto`
   with hosted/session accounts defaulting to `auto` and self-hosted/token,
   missing-kind, and standalone clients defaulting to `tcp`; fallback,
   health/metrics/logs.
4. **Change 4 — deploy + B4 qualification + hosted activation.**
   - **The gate that matters:** extend the frozen `scripts/perf-hol.sh`
     workload into the full bidirectional B4 comparator from §15.3, preserving
     separate TCP and QUIC evidence. QUIC must cut the
     **interactive-latency-under-load p95 by ≥ 50%** on every qualifying lossy
     profile/direction, without regressing the clean path or solo
     guardrails. The hosted `session => auto` resolver can be implemented
     beforehand; only then explicitly enable QUIC on the hosted edge. Do not
     change the compiled or self-hosted defaults.
   - Do **not** activate hosted QUIC on solo-throughput metrics — those were
     already healthy and are guardrails, not the target.

Reuse its frozen mixed-load scenario and perf fixtures for B4, but implement
the complete comparator and fail-closed gates required by §15.3.
