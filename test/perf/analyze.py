#!/usr/bin/env python3
"""Conservative A2 gate analyzer (transport-performance-spec G1.5).

Fails CLOSED. It exits nonzero with INCONCLUSIVE when the evidence can't support
a decision — missing required cases, too few samples, or ANY transport
errors/corruption — and it will not call an RTO ladder from a single observation
(a ladder needs a repeatable multi-rung TTFB pattern). As a methodology guard,
if the A2 criteria fire on the CLEAN control profile (where there is no loss),
it reports INCONCLUSIVE rather than a real A2 result — that pattern means the
harness is manufacturing artifacts (e.g. shaping applied after establishment)
rather than measuring A2.

Usage: analyze.py <results-dir>
Exit:  0 = definitive verdict (REPRODUCED or NOT); 2 = INCONCLUSIVE.
"""
import sys
import json
import glob
import os

MIN_SOLO_LARGE = 8      # min iterations for a solo large-transfer case
MIN_SMALL = 40          # min iterations for a small-response case
LADDER_RUNGS = [1000.0, 2000.0, 4000.0]
LADDER_TOL = 0.10
LADDER_MIN_PER_RUNG = 2
LADDER_MIN_RUNGS = 2
PRIMARY = "lossy"
REQUIRED = [(16777216, "download", 1), (16777216, "download", 8),
            (36, "download", 1), (36, "download", 8)]


def find(cl, size, d, conc):
    for c in cl:
        if c["size"] == size and c["dir"] == d and c["concurrency"] == conc:
            return c
    return None


def inconclusive(msg):
    print(f"\nINCONCLUSIVE: {msg}")
    print("  -> Cannot make a G1 go/no-go from this data.")
    sys.exit(2)


def ttfb_ladder(case):
    counts = {r: 0 for r in LADDER_RUNGS}
    for s in case.get("samples", []):
        t = s.get("ttfb_ms", 0)
        for r in LADDER_RUNGS:
            if abs(t - r) <= LADDER_TOL * r:
                counts[r] += 1
    rungs = [r for r, n in counts.items() if n >= LADDER_MIN_PER_RUNG]
    return counts, len(rungs) >= LADDER_MIN_RUNGS


def a2_signals(cl):
    sigs = []
    for size, label in [(16777216, "16MiB"), (1048576, "1MiB")]:
        solo, agg = find(cl, size, "download", 1), find(cl, size, "download", 8)
        if solo and agg and agg["aggregate_throughput_bps"] > 0:
            ratio = solo["median_throughput_bps"] / agg["aggregate_throughput_bps"]
            if ratio < 0.70:
                sigs.append(f"{label} solo/agg={ratio * 100:.0f}% (<70%)")
    for size, label in [(36, "36B"), (259072, "253KiB")]:
        solo, c8 = find(cl, size, "download", 1), find(cl, size, "download", 8)
        if solo and c8:
            sp95, c8p95 = solo["elapsed_ms"].get("p95", 0), c8["elapsed_ms"].get("p95", 0)
            if sp95 > 1000 and sp95 > 2 * c8p95:
                sigs.append(f"{label} solo-p95={sp95:.0f}ms (>1s and >2x conc8 {c8p95:.0f}ms)")
        if solo:
            counts, laddered = ttfb_ladder(solo)
            if laddered:
                sigs.append(f"{label} TTFB RTO ladder {counts}")
    return sigs


outdir = sys.argv[1] if len(sys.argv) > 1 else "."
cases = {}
for f in sorted(glob.glob(os.path.join(outdir, "raw-*.jsonl"))):
    for line in open(f):
        line = line.strip()
        if line:
            c = json.loads(line)
            cases.setdefault(c["profile"], []).append(c)

if not cases:
    inconclusive(f"no raw-*.jsonl found in {outdir}")

# 1) data-integrity gate: any errors or corruption anywhere -> inconclusive.
for prof, cl in cases.items():
    for c in cl:
        tag = f"{prof} {c['dir']} {c['size']}B conc={c['concurrency']}"
        if c.get("errors", 0) > 0:
            inconclusive(f"{tag} had {c['errors']} transport errors")
        if c.get("corrupt", 0) > 0:
            inconclusive(f"{tag} had {c['corrupt']} corrupt/checksum-mismatch")

# 2) required cases present with enough samples on the primary profile.
if PRIMARY not in cases:
    inconclusive(f"primary profile '{PRIMARY}' missing")
for (size, d, conc) in REQUIRED:
    c = find(cases[PRIMARY], size, d, conc)
    if c is None:
        inconclusive(f"required case missing on {PRIMARY}: {size}B {d} conc={conc}")
    need = MIN_SOLO_LARGE if size >= (1 << 20) else MIN_SMALL
    got = min(c["iterations"], len(c.get("samples", [c["iterations"]] * c["iterations"])))
    if got < need:
        inconclusive(f"too few samples for {size}B {d} conc={conc} on {PRIMARY}: {got} < {need}")

# 3) tables.
for prof in sorted(cases):
    print(f"\n===== {prof} =====")
    for c in sorted(cases[prof], key=lambda x: (x["dir"], x["size"], x["concurrency"])):
        e = c["elapsed_ms"]
        print(f"  {c['dir']:8} {c['size']:>9}B conc={c['concurrency']} n={c['iterations']:>3} "
              f"p50={e.get('p50', 0):8.1f} p95={e.get('p95', 0):9.1f} p99={e.get('p99', 0):9.1f} "
              f"max={e.get('max', 0):9.1f}ms  solo={c.get('median_throughput_bps', 0) / 1e6:6.2f} "
              f"agg={c.get('aggregate_throughput_bps', 0) / 1e6:6.2f}MB/s")

# 4) methodology guard: A2 must NOT fire on the clean control.
if "clean" in cases:
    csigs = a2_signals(cases["clean"])
    if csigs:
        inconclusive(f"A2 criteria fired on the CLEAN control ({csigs}) — the harness is "
                     "manufacturing artifacts (not a real A2 result). Fix the methodology and re-run.")

# 5) evaluate the lossy profiles.
fired = {}
for prof in ("lossy", "high-rtt-lossy"):
    if prof in cases:
        sigs = a2_signals(cases[prof])
        fired[prof] = sigs
        print(f"\n  [{prof}] A2 signals: {sigs if sigs else 'none'}")

print("\n" + "=" * 60)
if any(fired.values()):
    print("VERDICT: A2 REPRODUCED after A1 on a lossy profile (G1.5 threshold met).")
    print("  -> Part B (QUIC) is justified BY THE MEASUREMENT. Still combine with")
    print("     real-world network-exposure judgment before committing to the build.")
else:
    print("VERDICT: A2 NOT reproduced after A1 under the tested conditions (G1.6).")
    print("  -> Part B not justified by G1. Make no QUIC changes; revisit if real")
    print("     lossy-network pain appears.")
sys.exit(0)
