# Transport performance results

Dated artifacts from the measurement gate (transport-performance-spec §16 G1)
and, later, Part B qualification (§15.3). Each run keeps:

- `raw-<profile>.jsonl` — one JSON object per measured case (perfclient output;
  mixed-load records keep the actual `transport` and a separate
  `condition=baseline|underload`),
- `metadata.txt` — commit, kernel, netem qdiscs, effective config,
- a dated `decision-*.md` — the go/no-go with the numbers it rests on.

Regenerate the solo/parallel G1 evidence with `scripts/perf-g1-local.sh` (or
the optional remote-edge `scripts/perf-g1.sh`) and analyze it with
`test/perf/analyze.py`. Regenerate the mixed-load evidence with
`scripts/perf-hol.sh` and analyze it with `test/perf/hol_analyze.py`. Set
`TUNNEL_TRANSPORT=tcp|quic` for a forced, accurately labeled mixed-load run
(`tcp` is the default).

`hol_analyze.py` also accepts the committed legacy mixed-load JSON, where
`transport` contained `baseline|underload`; those records are normalized as
TCP during analysis. This script/analyzer pair records and validates the G1
workload one transport at a time; it is not by itself the bidirectional B4
head-to-head qualification.

## B4 qualification

Run the B4 suite only on a Linux host with network namespaces, `tc netem`,
ethtool, Python 3.10+, and permission to create privileged networking:

```text
scripts/perf-netem.sh build
sudo -E scripts/perf-netem.sh run
```

The build step writes a hash manifest for all five binaries. Qualification
rejects dirty or mismatched bundles. The run creates a new `b4-<UTC>/`
directory and never overwrites evidence. It records:

- `raw-direct.jsonl`: warmed, long-lived direct TLS/TCP and QUIC fixtures;
- `raw-protocol.jsonl`: beamd-over-TCP and beamd-over-QUIC cases;
- `raw-mixed.jsonl`: frozen baseline/under-load cases in both directions;
- `bulk-live.jsonl`: fail-closed ramp/4 KiB/65 KiB snapshots proving all six
  continuous bulk workers remained live with zero errors or corruption;
- `metadata.json`: commit, binary hashes, versions, limits, offloads, exact
  fixture/workload configuration, seeds, order, and handshake policy;
- `qdisc/`, `effective-config/`, `check-*.json`, and `logs/`: audit inputs;
- `perf-netem.sh` and `b4_analyze.py`: manifest-hashed copies used for the run
  and verdict, revalidated after collection;
- `summary.json` and `analysis.txt`: the fail-closed gate verdict.

`test/perf/b4_analyze.py` returns 0 only for complete passing evidence, 1 for
complete evidence that fails a gate, and 2 for missing/invalid/inconclusive
evidence. `MODE=smoke` is useful for harness development but is permanently
marked non-qualification and cannot pass the analyzer.
