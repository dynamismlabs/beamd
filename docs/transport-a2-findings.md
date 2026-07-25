# Transport A2 — findings, research, and Part B handoff

**Date:** 2026-07-24 → 25
**Status:** **GO** — build Part B (QUIC) behind a default-off flag. A1 shipped.
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
- **That justifies QUIC** (independent per-stream delivery). Build Part B behind
  a default-off flag; the B4 qualification must prove QUIC collapses the
  under-load interactive tail before the default is flipped.
- **Prior art confirms the direction:** OpenZiti (which powers zrok) hit the same
  wall and built its own UDP transport (westworld3 / dilithium / "Transwarp").
  beamd uses off-the-shelf `quic-go` instead of rolling its own.

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

## 4. Research / prior art — how zrok (OpenZiti) handles this

zrok is built on **OpenZiti**, a mesh-fabric overlay (routers + links +
circuits), not a single agent↔edge tunnel. Ziti does its own end-to-end flow
control at the "xgress" layer, and a link's common/default transport is
**TLS-over-TCP** — so circuits sharing a link face the *same* TCP head-of-line
problem. Ziti even ships both `xgress_transport` (TCP) and `xgress_transport_udp`.

**Their fix validates ours:** OpenZiti built **dilithium**, a Go framework
implementing the **westworld3** protocol — "high-performance WAN protocols over
UDP datagrams… reliable streaming on top of message-oriented infrastructures" —
integrated into Ziti as **"Transwarp."** In plain terms, they wrote their *own
QUIC* to escape TCP-over-a-shared-link. Their own flow-control tuning explicitly
benchmarks against "a straight dilithium tunnel" as the target to beat.

Implications for beamd:
1. **Direction validated** — the leading comparable project independently
   concluded the fix is multiplexed reliability over UDP (= QUIC).
2. **Use `quic-go`, don't roll our own.** Ziti spent years on westworld/dilithium;
   off-the-shelf QUIC is the pragmatic version.
3. **beamd is *more* exposed, not less.** Ziti's mesh can spread circuits across
   multiple links; beamd funnels one app's traffic through *one* tunnel
   connection — it concentrates the HoL problem Ziti partly diffuses.
4. **Rollout model matches** — Ziti keeps TLS/TCP as default and offers the UDP
   transport as an opt-in upgrade, mirroring "QUIC behind a default-off flag,
   prove it, then flip."

Confidence: high on the above; not verified is the *exact current* default
transport in zrok specifically, or precisely how Ziti schedules circuits on a
shared link to mitigate HoL at the fabric layer (would need a source dive).

Sources:
- OpenZiti Transwarp design doc (westworld3/dilithium):
  https://github.com/openziti/ziti/blob/release-next/doc/transwarp_b1/transwarp_b1.md
- openziti/dilithium: https://github.com/openziti/dilithium
- Xgress flow-control tuning (benchmarks vs. dilithium):
  https://github.com/openziti/fabric/issues/244
- OpenZiti architecture (DeepWiki): https://deepwiki.com/openziti/ziti

---

## 5. Methodology & what to trust

The measurement rig is trustworthy because it was corrected after a code review
caught two real bugs (see the git history and `decision-2026-07-24-g1.md`):

- **Shape before dial.** An earlier synthetic harness applied `netem` *after* the
  TCP/yamux connection established, which pre-trained TCP on a clean path and
  manufactured retransmissions. The current harness shapes **before** the agent
  dials.
- **Fail-closed analyzer.** `test/perf/analyze.py` returns INCONCLUSIVE + nonzero
  exit on missing cases, too few samples, or any errors/corruption, and a
  **clean-control guard** invalidates any result where A2 fires with no loss
  (i.e. it would have caught the shape-after-connect artifact).
- **Real beamd processes**, real edge + real agent, separate containers.

Caveats (carry these into B4):
- **Controlled reproduction, not the production WAN.** Mechanism is faithful; a
  real WAN would be similar or worse. An optional remote-edge confirmation
  (`scripts/perf-g1.sh` against a real remote edge) is not yet run.
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

**Pending / optional:**
- Push the local commits (A1 was pushed as `f901bb5`; the perf/spec commits are
  local only — `74579b3`, `d68eb74`, `f9731f9`, `9627e2e`, `76c4b17`).
- Remote-edge G1 confirmation (optional rigor).
- Buffer/prioritization experiment for the no-loss case (optional).
- Housekeeping: a crashed `perf-edge` container + `/root/perf-edge/` remain on
  the OSS box (104.248.61.150); remove when its SSH allows.

---

## 7. Part B execution handoff

Follow `docs/transport-performance-spec.md` §16 **Part B, Changes 1–4**, in order:

1. **Change 1 — abstraction.** Introduce `internal/tunnel` (Session/Stream/
   Listener), move yamux behind it. This is the seam deliberately deferred during
   A1. Keep behavior on TCP until green.
2. **Change 2 — QUIC engine, default OFF.** Add `quic-go`, listener/dial/adapters,
   key persistence. Opt-in only.
3. **Change 3 — selection/flags/diagnostics.** `transport: tcp|quic|auto`
   (default `tcp`), fallback, health/metrics/logs.
4. **Change 4 — deploy + B4 qualification + default flip.**
   - **The gate that matters:** re-run `scripts/perf-hol.sh` over QUIC vs
     tuned-yamux. QUIC must cut the **interactive-latency-under-load p95 by
     ≥ 50%** on a lossy profile, without regressing the clean path or solo
     guardrails. Only then flip the default to `auto`/QUIC.
   - Do **not** flip the default on solo-throughput metrics — those were already
     healthy and are guardrails, not the target.

Reuse the committed harness for B4; it already measures the right thing.
