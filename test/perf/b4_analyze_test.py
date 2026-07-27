#!/usr/bin/env python3

import contextlib
import importlib.util
import io
import json
import pathlib
import tempfile
import unittest


MODULE_PATH = pathlib.Path(__file__).with_name("b4_analyze.py")
SPEC = importlib.util.spec_from_file_location("b4_analyze", MODULE_PATH)
B4 = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
SPEC.loader.exec_module(B4)


def record(size, elapsed_ms, *, seed, aggregate=None, samples=1):
    return {
        "_source": "synthetic",
        "_seed": seed,
        "size": size,
        "samples": [
            {"elapsed_ms": elapsed_ms, "ttfb_ms": 100.0}
            for _ in range(samples)
        ],
        "aggregate_throughput_bps": (
            aggregate if aggregate is not None else size / (elapsed_ms / 1000)
        ),
    }


class B4AnalyzerTest(unittest.TestCase):
    def metadata(self, seeds):
        return {
            "netem_seeds": seeds,
            "seed_orders": {
                str(seed): ("quic,tcp" if index % 2 == 0 else "tcp,quic")
                for index, seed in enumerate(seeds)
            },
            "workload": {"bulk_streams": 6},
        }

    def test_timer_ladder_requires_three_recurring_adjacent_rungs(self):
        recurring, _ = B4.recurring_backoff([500.0, 1000.0, 2000.0] * 3)
        self.assertTrue(recurring)
        recurring, _ = B4.recurring_backoff([100.0] * 100 + [1000.0, 2000.0])
        self.assertFalse(recurring)

    def test_qualification_requires_container_limit_metadata(self):
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory) / "metadata.json"
            path.write_text("{}\n", encoding="utf-8")
            with self.assertRaisesRegex(B4.EvidenceError, "container_limits"):
                B4.load_metadata(path.parent)

    def test_qualification_host_shape_boundaries(self):
        B4.validate_host_shape(
            {"cpu_count": 2, "ram_bytes": 2_000_000_000}
        )
        B4.validate_host_shape(
            {"cpu_count": 2, "ram_bytes": 2_063_216_640}
        )

        invalid = (
            ({"cpu_count": 1, "ram_bytes": 2_000_000_000}, "cpu_count"),
            ({"cpu_count": True, "ram_bytes": 2_000_000_000}, "cpu_count"),
            ({"ram_bytes": 2_000_000_000}, "cpu_count"),
            ({"cpu_count": 2, "ram_bytes": 1_999_999_999}, "ram_bytes"),
            ({"cpu_count": 2, "ram_bytes": True}, "ram_bytes"),
            ({"cpu_count": 2}, "ram_bytes"),
        )
        for metadata, field in invalid:
            with self.subTest(metadata=metadata):
                with self.assertRaisesRegex(B4.EvidenceError, field):
                    B4.validate_host_shape(metadata)

    def test_complete_synthetic_matrix_passes(self):
        seeds = [101, 202, 303]
        records = {}
        for profile in B4.PROFILES:
            for seed_index, seed in enumerate(seeds):
                order = "quic,tcp" if seed_index % 2 == 0 else "tcp,quic"
                for direction in B4.DIRECTIONS:
                    for transport in B4.TRANSPORTS:
                        for fixture in ("beamd", "direct"):
                            for size in B4.SIZES:
                                key = (
                                    fixture,
                                    "protocol",
                                    transport,
                                    profile,
                                    seed,
                                    direction,
                                    size,
                                    1,
                                    None,
                                )
                                records[key] = record(size, 100.0, seed=seed)
                        eight_key = (
                            "beamd",
                            "protocol",
                            transport,
                            profile,
                            seed,
                            direction,
                            16 << 20,
                            8,
                            None,
                        )
                        records[eight_key] = record(
                            16 << 20,
                            100.0,
                            seed=seed,
                            aggregate=(16 << 20) / 0.1,
                        )
                        for size in B4.INTERACTIVE_SIZES:
                            for condition in ("baseline", "underload"):
                                elapsed = 100.0
                                if (
                                    profile in B4.LOSSY
                                    and size == 4 << 10
                                    and condition == "underload"
                                ):
                                    elapsed = 2000.0 if transport == "tcp" else 500.0
                                key = (
                                    "beamd",
                                    "mixed",
                                    transport,
                                    profile,
                                    seed,
                                    direction,
                                    size,
                                    1,
                                    condition,
                                )
                                records[key] = record(size, elapsed, seed=seed)
                        for key, value in records.items():
                            if key[4] == seed and key[3] == profile and key[5] == direction:
                                value["order"] = order

        metadata = self.metadata(seeds)
        B4.require_matrix(records, metadata)
        with contextlib.redirect_stdout(io.StringIO()):
            gates = B4.evaluate(records)
        self.assertTrue(gates.passed)

        records[("unexpected",)] = record(1, 1.0, seed=seeds[0])
        with self.assertRaises(B4.EvidenceError):
            B4.require_matrix(records, metadata)

    def test_seed_blocks_are_summarized_before_aggregation(self):
        fast = record(1000, 100.0, seed=101, samples=100)
        slow = record(1000, 1000.0, seed=202, samples=1)
        self.assertEqual(B4.median_throughput([fast, slow]), 5500.0)

        numerators = [
            record(1000, 100.0, seed=101),
            record(1000, 200.0, seed=202),
        ]
        denominators = [
            record(1000, 200.0, seed=101),
            record(1000, 400.0, seed=202),
        ]
        self.assertEqual(
            B4.paired_ratios(
                numerators, denominators, B4.block_median_throughput
            ),
            [2.0, 2.0],
        )

    def test_bulk_evidence_fails_closed_when_any_snapshot_is_not_six_streams(self):
        seeds = [101, 202, 303]
        metadata = self.metadata(seeds)
        rows = []
        timestamp = 1_000_000_000
        for profile in B4.PROFILES:
            for seed in seeds:
                order = metadata["seed_orders"][str(seed)]
                for direction in B4.DIRECTIONS:
                    for transport in B4.TRANSPORTS:
                        order_index = order.split(",").index(transport) + 1
                        for stage_index, stage in enumerate(
                            ("ramp", "after-4k", "after-65k")
                        ):
                            updated = timestamp + stage_index * 100
                            rows.append(
                                {
                                    "active": 6,
                                    "started": 6 + stage_index,
                                    "completed": stage_index,
                                    "errors": 0,
                                    "corrupt": 0,
                                    "updated_unix_nano": updated,
                                    "captured_unix_nano": updated + 1,
                                    "stage": stage,
                                    "profile": profile,
                                    "seed": seed,
                                    "dir": direction,
                                    "transport": transport,
                                    "order": order,
                                    "order_index": order_index,
                                }
                            )
                        timestamp += 1000

        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory) / "bulk-live.jsonl"
            path.write_text(
                "".join(json.dumps(row) + "\n" for row in rows),
                encoding="utf-8",
            )
            B4.validate_bulk_evidence(path.parent, metadata)
            rows[-1]["active"] = 5
            path.write_text(
                "".join(json.dumps(row) + "\n" for row in rows),
                encoding="utf-8",
            )
            with self.assertRaises(B4.EvidenceError):
                B4.validate_bulk_evidence(path.parent, metadata)

    def test_direct_records_require_production_direction_and_order(self):
        samples = [
            {"i": index, "elapsed_ms": 100.0, "ttfb_ms": 50.0, "ok": True}
            for index in range(50)
        ]
        raw = {
            "fixture": "direct",
            "workload": "protocol",
            "transport": "tcp",
            "profile": "clean",
            "dir": "download",
            "wire_direction": "agent-to-edge",
            "data_stream_initiator": "edge",
            "seed": 101,
            "order": "quic,tcp",
            "order_index": 2,
            "size": 36,
            "concurrency": 1,
            "warmups": 5,
            "iterations": 50,
            "errors": 0,
            "corrupt": 0,
            "handshake_included": False,
            "handshake_ms": 10.0,
            "samples": samples,
            "elapsed_ms": {
                "p50": 100.0,
                "p95": 100.0,
                "p99": 100.0,
                "max": 100.0,
            },
            "ttfb_ms": {"p50": 50.0, "p95": 50.0, "p99": 50.0, "max": 50.0},
            "median_throughput_bps": 360.0,
            "aggregate_throughput_bps": 360.0,
            "wall_s": 5.0,
        }
        B4.parse_record(raw, "synthetic", "protocol")

        wrong_order = dict(raw, order_index=1)
        with self.assertRaises(B4.EvidenceError):
            B4.parse_record(wrong_order, "synthetic", "protocol")
        wrong_direction = dict(raw, wire_direction="edge-to-agent")
        with self.assertRaises(B4.EvidenceError):
            B4.parse_record(wrong_direction, "synthetic", "protocol")
        wrong_aggregate = dict(raw, aggregate_throughput_bps=1.0)
        with self.assertRaises(B4.EvidenceError):
            B4.parse_record(wrong_aggregate, "synthetic", "protocol")

    def test_every_qdisc_snapshot_is_validated_independently(self):
        seed = 101

        def section(delay=75):
            return (
                "edge-to-agent:\n"
                f"qdisc netem 1: root limit 1000 delay {delay}ms "
                "loss random 1% seed 101 rate 100Mbit\n"
                "agent-to-edge:\n"
                f"qdisc netem 2: root limit 1000 delay {delay}ms "
                "loss random 1% seed 1000104 rate 100Mbit\n"
            )

        text = f"profile=lossy seed={seed}\n{section()}"
        for direction in B4.DIRECTIONS:
            for transport in B4.TRANSPORTS:
                text += (
                    f"\nafter direction={direction} transport={transport}\n"
                    f"{section()}"
                )
        profile = {"one_way_delay_ms": 75, "loss_percent": 1, "rate_mbit": 100}
        B4.validate_qdisc_artifact(
            text,
            artifact="synthetic",
            profile="lossy",
            seed=seed,
            profile_values=profile,
        )

        changed_snapshot = text.replace(
            "after direction=upload transport=quic\nedge-to-agent:\n"
            "qdisc netem 1: root limit 1000 delay 75ms",
            "after direction=upload transport=quic\nedge-to-agent:\n"
            "qdisc netem 1: root limit 1000 delay 999ms",
        )
        with self.assertRaises(B4.EvidenceError):
            B4.validate_qdisc_artifact(
                changed_snapshot,
                artifact="synthetic",
                profile="lossy",
                seed=seed,
                profile_values=profile,
            )


if __name__ == "__main__":
    unittest.main()
