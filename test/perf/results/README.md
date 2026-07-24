# Transport performance results

Dated artifacts from the measurement gate (transport-performance-spec §16 G1)
and, later, Part B qualification (§15.3). Each run keeps:

- `raw-<profile>.jsonl` — one JSON object per measured case (perfclient output),
- `metadata.txt` — commit, kernel, netem qdiscs, effective config,
- a dated `decision-*.md` — the go/no-go with the numbers it rests on.

Regenerate with `scripts/perf-netem.sh` and analyze with `test/perf/analyze.py`.
