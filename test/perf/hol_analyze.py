#!/usr/bin/env python3
"""Fail-closed analyzer for the mixed-load head-of-line test (perf-hol.sh).

For each tunnel transport, connection profile, and interactive size, compare
latency ALONE (``baseline``) with latency while concurrent bulk traffic shares
the tunnel (``underload``).

Clean-path inflation measures the queueing/bufferbloat floor. Lossy profiles
add TCP cross-stream head-of-line blocking. QUIC is expected to remove the
loss-driven coupling, but it still has connection-level congestion control, so
the clean result remains a qualification guardrail rather than a promised win.

Usage: hol_analyze.py <results-dir>
Exit: 0 = complete, valid evidence (A2 present or not present)
      2 = invalid or incomplete evidence

The analyzer accepts legacy perf-hol JSON where ``transport`` held
``baseline|underload``. Those records came from the TCP-only harness and are
normalized to ``transport=tcp`` plus the corresponding ``condition``.
"""

import glob
import json
import math
import os
import sys


SIZES = ((4096, "4KiB API"), (65536, "65KiB page"))
REQUIRED_PROFILES = ("clean", "wifi", "mobile", "bursty")
LOSSY_PROFILES = frozenset(("wifi", "mobile", "bursty"))
CONDITIONS = frozenset(("baseline", "underload"))
TRANSPORTS = frozenset(("tcp", "quic"))
MIN_SAMPLES = 50
ORDER = ("clean", "home", "wifi", "mobile", "bursty")


def inconclusive(message):
    print(f"\nINCONCLUSIVE: {message}")
    print("  -> Cannot make a mixed-load A2 decision from this data.")
    sys.exit(2)


def nonnegative_int(value, field, source):
    if isinstance(value, bool) or not isinstance(value, int):
        inconclusive(f"{source}: {field} must be an integer")
    if value < 0:
        inconclusive(f"{source}: {field} must be non-negative")
    return value


def positive_stat(case, field, source):
    stats = case.get("elapsed_ms")
    if not isinstance(stats, dict):
        inconclusive(f"{source}: elapsed_ms is missing or invalid")
    value = stats.get(field)
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        inconclusive(f"{source}: elapsed_ms.{field} is missing or invalid")
    if not math.isfinite(value) or value <= 0:
        inconclusive(f"{source}: elapsed_ms.{field} must be finite and positive")
    return float(value)


def load_cases(results_dir):
    files = sorted(glob.glob(os.path.join(results_dir, "raw-*.jsonl")))
    if not files:
        inconclusive(f"no raw-*.jsonl files found in {results_dir}")

    cases = {}
    transports = set()
    for path in files:
        with open(path, encoding="utf-8") as handle:
            for line_number, line in enumerate(handle, 1):
                if not line.strip():
                    continue
                source = f"{path}:{line_number}"
                try:
                    case = json.loads(line)
                except json.JSONDecodeError as err:
                    inconclusive(f"{source}: invalid JSON: {err}")
                if not isinstance(case, dict):
                    inconclusive(f"{source}: record must be a JSON object")

                profile = case.get("profile")
                raw_transport = case.get("transport")
                condition = case.get("condition")

                # Before condition had its own field, perf-hol used transport as
                # the baseline/underload label. That harness was TCP-only.
                if (
                    condition is None
                    and isinstance(raw_transport, str)
                    and raw_transport in CONDITIONS
                ):
                    condition = raw_transport
                    transport = "tcp"
                else:
                    transport = raw_transport

                if not isinstance(profile, str) or not profile:
                    inconclusive(f"{source}: profile is missing or invalid")
                if not isinstance(transport, str) or transport not in TRANSPORTS:
                    inconclusive(
                        f"{source}: transport must be one of {sorted(TRANSPORTS)}, got {transport!r}"
                    )
                if not isinstance(condition, str) or condition not in CONDITIONS:
                    inconclusive(
                        f"{source}: condition must be one of {sorted(CONDITIONS)}, got {condition!r}"
                    )
                if case.get("dir") != "download":
                    inconclusive(f"{source}: mixed-load interactive case must be a download")
                if nonnegative_int(case.get("concurrency"), "concurrency", source) != 1:
                    inconclusive(f"{source}: interactive concurrency must be 1")

                size = nonnegative_int(case.get("size"), "size", source)
                if size not in dict(SIZES):
                    inconclusive(f"{source}: unexpected interactive size {size}")
                iterations = nonnegative_int(case.get("iterations"), "iterations", source)
                if iterations < MIN_SAMPLES:
                    inconclusive(
                        f"{source}: too few iterations ({iterations} < {MIN_SAMPLES})"
                    )
                errors = nonnegative_int(case.get("errors"), "errors", source)
                corrupt = nonnegative_int(case.get("corrupt"), "corrupt", source)
                if errors or corrupt:
                    inconclusive(
                        f"{source}: errors={errors}, corrupt={corrupt}; evidence must be error-free"
                    )

                samples = case.get("samples")
                if not isinstance(samples, list):
                    inconclusive(f"{source}: raw samples are required")
                if len(samples) != iterations:
                    inconclusive(
                        f"{source}: sample count {len(samples)} does not match iterations {iterations}"
                    )
                if len(samples) < MIN_SAMPLES:
                    inconclusive(
                        f"{source}: too few raw samples ({len(samples)} < {MIN_SAMPLES})"
                    )
                for sample_number, sample in enumerate(samples):
                    if not isinstance(sample, dict) or sample.get("ok") is not True or "err" in sample:
                        inconclusive(
                            f"{source}: sample {sample_number} is missing, corrupt, or unsuccessful"
                        )

                for stat in ("p50", "p95", "p99", "max"):
                    positive_stat(case, stat, source)

                key = (transport, profile, condition, size)
                if key in cases:
                    inconclusive(f"{source}: duplicate case {key}")
                cases[key] = case
                transports.add(transport)

    if not transports:
        inconclusive("no tunnel transports found")

    for transport in sorted(transports):
        for profile in REQUIRED_PROFILES:
            for condition in CONDITIONS:
                for size, _ in SIZES:
                    key = (transport, profile, condition, size)
                    if key not in cases:
                        inconclusive(f"required case missing: {key}")

    return cases, transports


def p95(case):
    return float(case["elapsed_ms"]["p95"])


results_dir = sys.argv[1] if len(sys.argv) > 1 else "."
cases, observed_transports = load_cases(results_dir)
rows = []

profiles = sorted(
    {key[1] for key in cases},
    key=lambda profile: ORDER.index(profile) if profile in ORDER else len(ORDER),
)
for transport in sorted(observed_transports):
    for profile in profiles:
        if not any(key[0] == transport and key[1] == profile for key in cases):
            continue
        print(f"\n===== {transport} / {profile} =====")
        for size, label in SIZES:
            baseline = cases.get((transport, profile, "baseline", size))
            underload = cases.get((transport, profile, "underload", size))
            if baseline is None or underload is None:
                # Optional profiles may be present only as complete pairs. A
                # partial optional profile is still invalid, not silently skipped.
                inconclusive(
                    f"incomplete optional case: {(transport, profile, size)}"
                )
            baseline_p95 = p95(baseline)
            underload_p95 = p95(underload)
            inflation = underload_p95 / baseline_p95
            print(
                f"  {label:11}  baseline p50/p95/p99 = "
                f"{baseline['elapsed_ms']['p50']:7.1f}/"
                f"{baseline_p95:8.1f}/"
                f"{baseline['elapsed_ms']['p99']:8.1f}ms"
            )
            print(
                f"  {'':11}  UNDER LOAD           = "
                f"{underload['elapsed_ms']['p50']:7.1f}/"
                f"{underload_p95:8.1f}/"
                f"{underload['elapsed_ms']['p99']:8.1f}ms   "
                f"p95 x{inflation:.1f}"
            )
            rows.append(
                (transport, profile, size, baseline_p95, underload_p95, inflation)
            )

# The primary G1 signal is a 4 KiB request inflating at least 3x and into
# seconds on a lossy profile. Clean and 65 KiB cases remain evidence and
# qualification guardrails, but cannot independently produce a GO verdict.
signals = [
    row
    for row in rows
    if row[1] in LOSSY_PROFILES
    and row[2] == 4096
    and row[5] >= 3
    and row[4] >= 1000
]

print("\n" + "=" * 72)
print("Under-load inflation measures queueing while interactive and bulk traffic")
print("share one tunnel. Clean shows the queueing floor; loss adds TCP cross-stream")
print("head-of-line blocking. QUIC must prove the improvement in B4.\n")
for transport, profile, size, baseline_p95, underload_p95, inflation in rows:
    label = "4KiB" if size == 4096 else "65KiB"
    flag = "  <== primary signal" if (
        profile in LOSSY_PROFILES
        and size == 4096
        and inflation >= 3
        and underload_p95 >= 1000
    ) else ("  <== inflated" if inflation >= 3 else "")
    print(
        f"  {transport:4} {profile:7} {label:6} p95 "
        f"{baseline_p95:7.0f}ms -> {underload_p95:8.0f}ms under load  "
        f"(x{inflation:.1f}){flag}"
    )
print()

if signals:
    worst = max(signals, key=lambda row: row[5])
    print("VERDICT: shared-connection mixed-load A2 IS present.")
    print(
        f"  Lossy-profile 4 KiB p95 inflates up to x{worst[5]:.1f}, "
        f"reaching {worst[4]:.0f}ms."
    )
    print("  This evidence justifies evaluating Part B; B4 must prove QUIC fixes it.")
else:
    print("VERDICT: mixed-load A2 was NOT reproduced by the primary gate.")
    print("  No lossy-profile 4 KiB case inflated >=3x into seconds.")
