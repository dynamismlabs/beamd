#!/usr/bin/env python3
"""Fail-closed B4 transport qualification comparator.

Inputs are produced by scripts/perf-netem.sh:

  metadata.json
  raw-direct.jsonl
  raw-protocol.jsonl
  raw-mixed.jsonl

Exit 0 means every required case is present, internally valid, and every gate
passes. Exit 1 means complete evidence failed one or more performance gates.
Exit 2 means evidence is missing, malformed, dirty, or otherwise inconclusive.
No missing fixture or corrupt request is ever converted into a zero or a pass.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import math
import pathlib
import re
import statistics
import ssl
import sys
from collections import Counter
from typing import Any, Iterable


PROFILES = ("clean", "lossy", "high-rtt-clean", "high-rtt-lossy")
LOSSY = ("lossy", "high-rtt-lossy")
TRANSPORTS = ("tcp", "quic")
DIRECTIONS = ("download", "upload")
SIZES = (36, 253 * 1024, 257 * 1024, 1 << 20, 16 << 20, 100 << 20)
INTERACTIVE_SIZES = (4 << 10, 65 << 10)
SEED_MINIMUM = 3
MIN_QUALIFICATION_CPUS = 2
MIN_QUALIFICATION_RAM_BYTES = 2_000_000_000


class EvidenceError(Exception):
    """The evidence is incomplete or invalid, so no gate decision is possible."""


def finite_number(value: Any, label: str, *, positive: bool = False) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise EvidenceError(f"{label} must be numeric")
    number = float(value)
    if not math.isfinite(number) or (positive and number <= 0):
        qualifier = "finite and positive" if positive else "finite"
        raise EvidenceError(f"{label} must be {qualifier}")
    return number


def integer(value: Any, label: str, *, minimum: int = 0) -> int:
    if isinstance(value, bool) or not isinstance(value, int) or value < minimum:
        raise EvidenceError(f"{label} must be an integer >= {minimum}")
    return value


def validate_host_shape(metadata: dict[str, Any]) -> None:
    integer(
        metadata.get("cpu_count"),
        "metadata cpu_count",
        minimum=MIN_QUALIFICATION_CPUS,
    )
    integer(
        metadata.get("ram_bytes"),
        "metadata ram_bytes",
        minimum=MIN_QUALIFICATION_RAM_BYTES,
    )


def percentile(values: Iterable[float], quantile: float) -> float:
    ordered = sorted(values)
    if not ordered:
        raise EvidenceError("cannot calculate a percentile from no samples")
    position = quantile * (len(ordered) - 1)
    low, high = math.floor(position), math.ceil(position)
    if low == high:
        return ordered[low]
    fraction = position - low
    return ordered[low] * (1 - fraction) + ordered[high] * fraction


def validate_qdisc_artifact(
    text: str,
    *,
    artifact: str,
    profile: str,
    seed: int,
    profile_values: dict[str, int],
) -> None:
    header = f"profile={profile} seed={seed}\n"
    if not text.startswith(header):
        raise EvidenceError(f"qdisc artifact has wrong profile/seed header: {artifact}")
    body = text[len(header) :]
    marker = re.compile(
        r"^after direction=(download|upload) transport=(tcp|quic)$",
        flags=re.MULTILINE,
    )
    matches = list(marker.finditer(body))
    sections: dict[Any, str] = {
        "initial": body[: matches[0].start()] if matches else body
    }
    for index, match in enumerate(matches):
        end = matches[index + 1].start() if index + 1 < len(matches) else len(body)
        sections[(match.group(1), match.group(2))] = body[match.end() : end]
    expected_sections = {
        "initial",
        *{
            (direction, transport)
            for direction in DIRECTIONS
            for transport in TRANSPORTS
        },
    }
    if set(sections) != expected_sections or len(matches) != 4:
        raise EvidenceError(
            f"qdisc artifact lacks exact initial/post-block snapshots: {artifact}"
        )

    def validate_side(snapshot: Any, side: str, side_text: str, side_seed: int) -> None:
        lowered = side_text.lower()
        delay = profile_values["one_way_delay_ms"]
        rate = profile_values["rate_mbit"]
        loss = profile_values["loss_percent"]
        loss_match = re.search(
            r"\bloss(?:\s+random)?\s+([0-9]+(?:\.[0-9]+)?)%",
            lowered,
        )
        if (
            lowered.count("qdisc netem") != 1
            or "limit 1000" not in lowered
            or not re.search(rf"delay\s+{delay}(?:\.0+)?ms", lowered)
            or not re.search(rf"rate\s+{rate}(?:\.0+)?mbit", lowered)
            or not re.search(rf"seed\s+{side_seed}\b", lowered)
            or (
                loss == 0
                and loss_match is not None
            )
            or (
                loss > 0
                and (
                    loss_match is None
                    or float(loss_match.group(1)) != float(loss)
                )
            )
        ):
            raise EvidenceError(
                f"{artifact} snapshot={snapshot!r} side={side} "
                "does not match the frozen qdisc"
            )

    for snapshot, section in sections.items():
        if (
            section.count("edge-to-agent:\n") != 1
            or section.count("agent-to-edge:\n") != 1
        ):
            raise EvidenceError(
                f"{artifact} snapshot={snapshot!r} has malformed side markers"
            )
        edge_marker = section.index("edge-to-agent:\n") + len("edge-to-agent:\n")
        agent_marker = section.index("agent-to-edge:\n")
        if agent_marker <= edge_marker:
            raise EvidenceError(
                f"{artifact} snapshot={snapshot!r} has reversed side markers"
            )
        edge_text = section[edge_marker:agent_marker]
        agent_text = section[
            agent_marker + len("agent-to-edge:\n") :
        ]
        validate_side(snapshot, "edge-to-agent", edge_text, seed)
        validate_side(snapshot, "agent-to-edge", agent_text, seed + 1_000_003)


def load_metadata(root: pathlib.Path) -> dict[str, Any]:
    path = root / "metadata.json"
    try:
        metadata = json.loads(path.read_text(encoding="utf-8"))
    except FileNotFoundError as err:
        raise EvidenceError("metadata.json is missing") from err
    except json.JSONDecodeError as err:
        raise EvidenceError(f"metadata.json is invalid JSON: {err}") from err
    if not isinstance(metadata, dict):
        raise EvidenceError("metadata.json must contain one JSON object")

    required = (
        "schema_version",
        "qualification",
        "beamd_commit",
        "git_dirty",
        "binary_manifest",
        "recorded_analyzer",
        "recorded_harness",
        "go_version",
        "quic_go_version",
        "yamux_version",
        "kernel",
        "os",
        "cpu",
        "cpu_count",
        "ram_bytes",
        "resource_limits",
        "container_limits",
        "interface_offload",
        "effective_config",
        "direct_fixture",
        "workload",
        "topology",
        "public_leg_shaped",
        "directions_shaped",
        "netem_profiles",
        "netem_queue_limit_packets",
        "netem_seeds",
        "seed_orders",
        "transport_orders",
        "qdisc_artifacts",
        "handshake_included",
    )
    missing = [field for field in required if field not in metadata]
    if missing:
        raise EvidenceError(f"metadata.json is missing fields: {', '.join(missing)}")
    if metadata["schema_version"] != 1:
        raise EvidenceError("unsupported metadata schema_version")
    if metadata["qualification"] is not True:
        raise EvidenceError("run is marked as smoke/non-qualification evidence")
    if metadata["git_dirty"] is not False:
        raise EvidenceError("qualification binary came from a dirty worktree")
    if metadata["handshake_included"] is not False:
        raise EvidenceError("handshake time must be excluded from transfer gates")
    if (
        metadata["topology"]
        != "edge namespace/public client <-> shaped veth <-> agent namespace/backend"
        or metadata["public_leg_shaped"] is not False
        or metadata["directions_shaped"] != ["edge-to-agent", "agent-to-edge"]
    ):
        raise EvidenceError(
            "metadata topology must isolate shaping to both directions of the agent leg"
        )
    if not isinstance(metadata["beamd_commit"], str) or len(metadata["beamd_commit"]) < 7:
        raise EvidenceError("beamd_commit is missing or invalid")
    manifest = metadata["binary_manifest"]
    if (
        not isinstance(manifest, dict)
        or manifest.get("commit") != metadata["beamd_commit"]
        or manifest.get("dirty") is not False
        or manifest.get("go_version") != metadata["go_version"]
        or not isinstance(manifest.get("binaries"), dict)
        or set(manifest.get("binaries", {}))
        != {"beamd", "perfclient", "perfserver", "directclient", "directserver"}
        or not isinstance(manifest.get("assets"), dict)
        or set(manifest.get("assets", {}))
        != {
            "b4_analyze.py",
            "test/perf/b4_analyze.py",
            "scripts/perf-netem.sh",
        }
    ):
        raise EvidenceError("binary_manifest is missing, dirty, or disagrees with the commit")
    for name, digest in manifest["binaries"].items():
        try:
            valid_digest = isinstance(digest, str) and len(digest) == 64 and int(digest, 16) >= 0
        except ValueError:
            valid_digest = False
        if not valid_digest:
            raise EvidenceError(f"binary_manifest has invalid SHA-256 for {name}")
    for name, digest in manifest["assets"].items():
        try:
            valid_digest = (
                isinstance(digest, str)
                and len(digest) == 64
                and int(digest, 16) >= 0
            )
        except ValueError:
            valid_digest = False
        if not valid_digest:
            raise EvidenceError(f"binary_manifest has invalid asset SHA-256 for {name}")
    if (
        manifest["assets"]["b4_analyze.py"]
        != manifest["assets"]["test/perf/b4_analyze.py"]
    ):
        raise EvidenceError("bundled analyzer differs from the clean source analyzer")
    recorded_assets = (
        (
            metadata["recorded_analyzer"],
            manifest["assets"]["b4_analyze.py"],
            "analyzer",
        ),
        (
            metadata["recorded_harness"],
            manifest["assets"]["scripts/perf-netem.sh"],
            "harness",
        ),
    )
    for relative, expected_digest, label in recorded_assets:
        if not isinstance(relative, str):
            raise EvidenceError(f"recorded {label} path is invalid")
        artifact_path = root / relative
        if not artifact_path.is_file():
            raise EvidenceError(f"recorded {label} is missing")
        if hashlib.sha256(artifact_path.read_bytes()).hexdigest() != expected_digest:
            raise EvidenceError(f"recorded {label} hash disagrees with build manifest")
    for field in (
        "go_version",
        "quic_go_version",
        "yamux_version",
        "kernel",
        "os",
        "cpu",
        "resource_limits",
        "container_limits",
        "interface_offload",
    ):
        if not isinstance(metadata[field], str) or not metadata[field].strip():
            raise EvidenceError(f"metadata {field} must be a non-empty string")
    validate_host_shape(metadata)
    offload_path = root / metadata["interface_offload"]
    if not offload_path.is_file() or offload_path.stat().st_size == 0:
        raise EvidenceError("recorded interface offload artifact is missing or empty")
    offload_text = offload_path.read_text(encoding="utf-8").lower()
    for setting in (
        "generic-receive-offload: off",
        "generic-segmentation-offload: off",
        "tcp-segmentation-offload: off",
    ):
        if offload_text.count(setting) < 2:
            raise EvidenceError(
                f"both shaped interfaces must record {setting!r}"
            )

    seeds = metadata["netem_seeds"]
    if (
        not isinstance(seeds, list)
        or any(isinstance(seed, bool) or not isinstance(seed, int) for seed in seeds)
    ):
        raise EvidenceError("metadata must contain at least three distinct integer netem seeds")
    if len(set(seeds)) < SEED_MINIMUM or len(set(seeds)) != len(seeds):
        raise EvidenceError("metadata must contain at least three distinct integer netem seeds")
    orders = metadata["transport_orders"]
    if (
        not isinstance(orders, list)
        or any(not isinstance(order, str) for order in orders)
        or len(orders) != 2
        or set(orders) != {"quic,tcp", "tcp,quic"}
    ):
        raise EvidenceError("metadata must record both counterbalanced transport orders")
    expected_profiles = {
        "clean": {"one_way_delay_ms": 75, "loss_percent": 0, "rate_mbit": 100},
        "lossy": {"one_way_delay_ms": 75, "loss_percent": 1, "rate_mbit": 100},
        "high-rtt-clean": {
            "one_way_delay_ms": 250,
            "loss_percent": 0,
            "rate_mbit": 20,
        },
        "high-rtt-lossy": {
            "one_way_delay_ms": 250,
            "loss_percent": 1,
            "rate_mbit": 20,
        },
    }
    if metadata["netem_profiles"] != expected_profiles:
        raise EvidenceError("metadata netem_profiles do not match the frozen B4 profiles")
    if metadata["netem_queue_limit_packets"] != 1000:
        raise EvidenceError("metadata netem_queue_limit_packets must be frozen at 1000")
    seed_orders = metadata["seed_orders"]
    expected_seed_orders = {
        str(seed): ("quic,tcp" if index % 2 == 0 else "tcp,quic")
        for index, seed in enumerate(seeds)
    }
    if seed_orders != expected_seed_orders:
        raise EvidenceError(
            "metadata seed_orders do not match the frozen counterbalance sequence"
        )

    workload = metadata["workload"]
    required_workload = {
        "bulk_streams": 6,
        "bulk_bytes": 8 << 20,
        "ramp_seconds": 5,
        "interactive_bytes": [4 << 10, 65 << 10],
        "interactive_warmups": 8,
        "interactive_samples": 50,
    }
    if not isinstance(workload, dict):
        raise EvidenceError("metadata workload must be an object")
    for field, expected in required_workload.items():
        if workload.get(field) != expected:
            raise EvidenceError(
                f"metadata workload.{field} must be frozen at {expected!r}"
            )
    direct = metadata["direct_fixture"]
    if not isinstance(direct, dict):
        raise EvidenceError("metadata direct_fixture must be an object")
    for field in (
        "alpn",
        "tls_version",
        "certificate",
        "certificate_sha256",
        "quic_flow_control",
    ):
        if not direct.get(field):
            raise EvidenceError(f"metadata direct_fixture.{field} is required")
    if (
        direct["alpn"] != "beamd-perf-direct/1"
        or direct["tls_version"] != "TLS 1.3"
        or direct.get("long_lived_connection") is not True
        or direct.get("handshake_recorded_separately") is not True
    ):
        raise EvidenceError(
            "direct fixture ALPN/TLS/connection/handshake policy is not frozen"
        )
    expected_flow_control = {
        "initial_stream": 4 << 20,
        "max_stream": 16 << 20,
        "initial_connection": 16 << 20,
        "max_connection": 64 << 20,
        "server_max_incoming_streams": 1,
        "client_max_incoming_streams": 64,
        "keepalive_period_ms": 0,
    }
    if direct["quic_flow_control"] != expected_flow_control:
        raise EvidenceError(
            "direct fixture QUIC settings do not match the production flow-control roles"
        )
    expected_direction_mapping = {
        "download": "agent client uploads to edge server",
        "upload": "agent client downloads from edge server",
    }
    if (
        direct.get("connection_roles")
        != "agent namespace client dials edge namespace server"
        or direct.get("control_stream_initiator") != "agent"
        or direct.get("data_stream_initiator") != "edge"
        or direct.get("direction_mapping") != expected_direction_mapping
    ):
        raise EvidenceError(
            "direct fixture endpoint roles or direction mapping do not match production"
        )
    if (
        direct.get("trust")
        != "certificate validation disabled for direct and beamd measurement clients"
    ):
        raise EvidenceError("direct and beamd fixtures must use equivalent TLS trust behavior")
    try:
        valid_cert_digest = (
            isinstance(direct["certificate_sha256"], str)
            and len(direct["certificate_sha256"]) == 64
            and int(direct["certificate_sha256"], 16) >= 0
        )
    except ValueError:
        valid_cert_digest = False
    if not valid_cert_digest:
        raise EvidenceError("direct fixture certificate_sha256 must be a SHA-256 hex digest")
    certificate_path = root / direct["certificate"]
    if not certificate_path.is_file() or certificate_path.stat().st_size == 0:
        raise EvidenceError("recorded direct fixture certificate is missing or empty")
    try:
        certificate_der = ssl.PEM_cert_to_DER_cert(
            certificate_path.read_text(encoding="utf-8")
        )
        actual_certificate_digest = hashlib.sha256(certificate_der).hexdigest()
    except (OSError, ValueError) as err:
        raise EvidenceError("recorded direct fixture certificate is invalid") from err
    if actual_certificate_digest != direct["certificate_sha256"].lower():
        raise EvidenceError("direct fixture certificate hash does not match metadata")

    config = metadata["effective_config"]
    if not isinstance(config, dict):
        raise EvidenceError("metadata effective_config must be an object")
    if (
        config.get("yamux_stream_window_bytes") != 4 << 20
        or not isinstance(config.get("gomemlimit"), str)
        or not config["gomemlimit"].strip()
    ):
        raise EvidenceError("effective window/GOMEMLIMIT metadata is missing or invalid")
    for field in ("edge", "tcp_client", "quic_client"):
        relative = config.get(field)
        if not isinstance(relative, str) or not (root / relative).is_file():
            raise EvidenceError(f"effective configuration artifact missing: {field}")
    edge_config = (root / config["edge"]).read_text(encoding="utf-8")
    for expected_line in (
        'listen_https: "10.231.0.1:443"',
        'listen_quic: "10.231.0.1:443"',
        "disable_quic: false",
        "max_streams_per_session: 64",
        "max_streams_total: 128",
        "max_pre_auth_sessions: 32",
        "max_sessions_total: 8",
    ):
        if expected_line not in edge_config:
            raise EvidenceError(
                f"effective edge configuration does not contain {expected_line!r}"
            )
    for field, transport in (("tcp_client", "tcp"), ("quic_client", "quic")):
        client_config = (root / config[field]).read_text(encoding="utf-8")
        if (
            f"transport: {transport}" not in client_config
            or "server: 10.231.0.1:443" not in client_config
        ):
            raise EvidenceError(
                f"effective {transport} client configuration is not forced"
            )

    artifacts = metadata["qdisc_artifacts"]
    if not isinstance(artifacts, list) or not artifacts:
        raise EvidenceError("qdisc_artifacts must be a non-empty list")
    for artifact in artifacts:
        if (
            not isinstance(artifact, str)
            or not (root / "qdisc" / artifact).is_file()
            or (root / "qdisc" / artifact).stat().st_size == 0
        ):
            raise EvidenceError(f"qdisc artifact missing or empty: {artifact!r}")

    expected_artifacts = {
        f"{profile}-{seed}.txt" for profile in PROFILES for seed in seeds
    }
    if set(artifacts) != expected_artifacts or len(artifacts) != len(expected_artifacts):
        raise EvidenceError("qdisc_artifacts must exactly match profile/seed matrix")

    for profile in PROFILES:
        for seed in seeds:
            expected_qdisc = f"{profile}-{seed}.txt"
            qdisc_text = (root / "qdisc" / expected_qdisc).read_text(
                encoding="utf-8"
            )
            validate_qdisc_artifact(
                qdisc_text,
                artifact=expected_qdisc,
                profile=profile,
                seed=seed,
                profile_values=expected_profiles[profile],
            )
            for direction in DIRECTIONS:
                for transport in TRANSPORTS:
                    check_path = (
                        root
                        / f"check-{profile}-{seed}-{direction}-{transport}.json"
                    )
                    try:
                        check = json.loads(check_path.read_text(encoding="utf-8"))
                    except (FileNotFoundError, json.JSONDecodeError) as err:
                        raise EvidenceError(
                            f"forced-transport preflight missing or invalid: {check_path.name}"
                        ) from err
                    if (
                        not isinstance(check, dict)
                        or check.get("ok") is not True
                        or check.get("transport") != transport
                    ):
                        raise EvidenceError(
                            f"forced-transport preflight failed: {check_path.name}"
                        )
    return metadata


def expected_iterations(workload: str, size: int, concurrency: int) -> int:
    if workload == "mixed":
        return 50
    if concurrency == 8:
        return 8
    if size == 36 and workload == "protocol-high-rtt-lossy":
        return 100
    if size <= 1 << 20:
        return 50
    if size == 16 << 20:
        return 20
    if size == 100 << 20:
        return 5
    raise EvidenceError(f"no sample-count rule for size={size}, concurrency={concurrency}")


def parse_record(
    record: Any, source: str, expected_file_workload: str
) -> tuple[tuple[Any, ...], dict[str, Any]]:
    if not isinstance(record, dict):
        raise EvidenceError(f"{source}: record must be an object")
    workload = record.get("workload")
    if workload != expected_file_workload:
        raise EvidenceError(
            f"{source}: workload must be {expected_file_workload!r}, got {workload!r}"
        )
    fixture = record.get("fixture")
    if fixture not in ("beamd", "direct"):
        raise EvidenceError(f"{source}: fixture must be beamd or direct")
    if workload == "mixed" and fixture != "beamd":
        raise EvidenceError(f"{source}: mixed-load records must use beamd")

    transport = record.get("transport")
    if isinstance(transport, str) and transport.startswith("direct-"):
        transport = transport.removeprefix("direct-")
    if transport not in TRANSPORTS:
        raise EvidenceError(f"{source}: invalid transport {transport!r}")
    profile = record.get("profile")
    if profile not in PROFILES:
        raise EvidenceError(f"{source}: invalid profile {profile!r}")
    direction = record.get("dir")
    if direction not in DIRECTIONS:
        raise EvidenceError(f"{source}: invalid direction {direction!r}")
    seed = integer(record.get("seed"), f"{source}: seed", minimum=1)
    order = record.get("order")
    if order not in ("quic,tcp", "tcp,quic"):
        raise EvidenceError(f"{source}: invalid counterbalance order {order!r}")
    order_index = integer(record.get("order_index"), f"{source}: order_index", minimum=1)
    if order_index not in (1, 2):
        raise EvidenceError(f"{source}: order_index must be 1 or 2")
    expected_order_index = order.split(",").index(transport) + 1
    if order_index != expected_order_index:
        raise EvidenceError(f"{source}: order_index disagrees with order and transport")

    size = integer(record.get("size"), f"{source}: size", minimum=1)
    concurrency = integer(record.get("concurrency"), f"{source}: concurrency", minimum=1)
    warmups = integer(record.get("warmups"), f"{source}: warmups")
    iterations = integer(record.get("iterations"), f"{source}: iterations", minimum=1)
    if fixture == "direct" and concurrency != 1:
        raise EvidenceError(f"{source}: direct fixture must use concurrency=1")
    if workload == "mixed":
        if size not in INTERACTIVE_SIZES or concurrency != 1:
            raise EvidenceError(f"{source}: invalid mixed-load size/concurrency")
        if warmups < 8 or iterations < 50:
            raise EvidenceError(f"{source}: mixed-load case needs 8 warmups and 50 samples")
    else:
        if size not in SIZES:
            raise EvidenceError(f"{source}: unexpected protocol size {size}")
        if concurrency not in (1, 8) or (concurrency == 8 and size != 16 << 20):
            raise EvidenceError(f"{source}: unexpected protocol concurrency")
        count_rule = (
            "protocol-high-rtt-lossy"
            if profile == "high-rtt-lossy" and size == 36 and concurrency == 1
            else "protocol"
        )
        minimum = expected_iterations(count_rule, size, concurrency)
        if iterations < minimum:
            raise EvidenceError(
                f"{source}: needs at least {minimum} samples, got {iterations}"
            )
        if warmups < 5:
            raise EvidenceError(f"{source}: protocol/direct case needs five warmups")

    if integer(record.get("errors"), f"{source}: errors") != 0:
        raise EvidenceError(f"{source}: request errors make the run inconclusive")
    if integer(record.get("corrupt"), f"{source}: corrupt") != 0:
        raise EvidenceError(f"{source}: payload corruption makes the run inconclusive")
    if record.get("handshake_included") is not False:
        raise EvidenceError(f"{source}: handshake_included must be false")

    samples = record.get("samples")
    if not isinstance(samples, list) or len(samples) != iterations:
        raise EvidenceError(f"{source}: raw sample count must equal iterations")
    for index, sample in enumerate(samples):
        if not isinstance(sample, dict):
            raise EvidenceError(f"{source}: sample {index} is not an object")
        if sample.get("ok") is not True or sample.get("err"):
            raise EvidenceError(f"{source}: sample {index} is unsuccessful")
        finite_number(sample.get("elapsed_ms"), f"{source}: sample {index} elapsed", positive=True)
        finite_number(sample.get("ttfb_ms"), f"{source}: sample {index} ttfb")

    quantiles = {"p50": .50, "p95": .95, "p99": .99, "max": 1.0}
    for group in ("elapsed_ms", "ttfb_ms"):
        stats = record.get(group)
        if not isinstance(stats, dict):
            raise EvidenceError(f"{source}: {group} statistics are missing")
        raw_values = [
            finite_number(
                sample[group],
                f"{source}: sample {index} {group}",
                positive=group == "elapsed_ms",
            )
            for index, sample in enumerate(samples)
        ]
        for field, quantile in quantiles.items():
            declared = finite_number(
                stats.get(field), f"{source}: {group}.{field}"
            )
            calculated = percentile(raw_values, quantile)
            if not math.isclose(declared, calculated, rel_tol=1e-9, abs_tol=1e-6):
                raise EvidenceError(
                    f"{source}: {group}.{field} disagrees with raw samples"
                )
    declared_median_throughput = finite_number(
        record.get("median_throughput_bps"),
        f"{source}: median_throughput_bps",
        positive=True,
    )
    expected_median_throughput = float(size) / (
        percentile(
            [
                finite_number(
                    sample["elapsed_ms"],
                    f"{source}: sample elapsed_ms",
                    positive=True,
                )
                for sample in samples
            ],
            .50,
        )
        / 1000
    )
    if not math.isclose(
        declared_median_throughput,
        expected_median_throughput,
        rel_tol=1e-9,
        abs_tol=1e-6,
    ):
        raise EvidenceError(
            f"{source}: median_throughput_bps disagrees with raw samples"
        )
    declared_aggregate_throughput = finite_number(
        record.get("aggregate_throughput_bps"),
        f"{source}: aggregate_throughput_bps",
        positive=True,
    )
    wall_seconds = finite_number(
        record.get("wall_s"), f"{source}: wall_s", positive=True
    )
    expected_aggregate_throughput = float(iterations * size) / wall_seconds
    if not math.isclose(
        declared_aggregate_throughput,
        expected_aggregate_throughput,
        rel_tol=1e-9,
        abs_tol=1e-6,
    ):
        raise EvidenceError(
            f"{source}: aggregate_throughput_bps disagrees with bytes/wall_s"
        )
    if fixture == "direct":
        finite_number(record.get("handshake_ms"), f"{source}: handshake_ms", positive=True)
        expected_wire_direction = (
            "agent-to-edge" if direction == "download" else "edge-to-agent"
        )
        if (
            record.get("wire_direction") != expected_wire_direction
            or record.get("data_stream_initiator") != "edge"
        ):
            raise EvidenceError(
                f"{source}: direct wire direction/initiator does not match "
                f"tunnel direction {direction!r}"
            )
    elif (
        record.get("wire_direction") is not None
        or record.get("data_stream_initiator") is not None
    ):
        raise EvidenceError(
            f"{source}: beamd records must not have direct fixture role fields"
        )

    condition = record.get("condition")
    if workload == "mixed":
        if condition not in ("baseline", "underload"):
            raise EvidenceError(f"{source}: invalid mixed-load condition {condition!r}")
    elif condition is not None:
        raise EvidenceError(f"{source}: protocol records must not have condition")

    key = (
        fixture,
        workload,
        transport,
        profile,
        seed,
        direction,
        size,
        concurrency,
        condition,
    )
    record["_transport"] = transport
    record["_seed"] = seed
    record["_source"] = source
    return key, record


def load_records(root: pathlib.Path) -> dict[tuple[Any, ...], dict[str, Any]]:
    files = (
        ("raw-direct.jsonl", "protocol"),
        ("raw-protocol.jsonl", "protocol"),
        ("raw-mixed.jsonl", "mixed"),
    )
    records: dict[tuple[Any, ...], dict[str, Any]] = {}
    for filename, workload in files:
        path = root / filename
        if not path.is_file() or path.stat().st_size == 0:
            raise EvidenceError(f"{filename} is missing or empty")
        with path.open(encoding="utf-8") as handle:
            for line_number, line in enumerate(handle, 1):
                if not line.strip():
                    continue
                source = f"{filename}:{line_number}"
                try:
                    raw = json.loads(line)
                except json.JSONDecodeError as err:
                    raise EvidenceError(f"{source}: invalid JSON: {err}") from err
                key, record = parse_record(raw, source, workload)
                if key in records:
                    raise EvidenceError(f"{source}: duplicate case {key}")
                records[key] = record
    return records


def validate_bulk_evidence(root: pathlib.Path, metadata: dict[str, Any]) -> None:
    path = root / "bulk-live.jsonl"
    if not path.is_file() or path.stat().st_size == 0:
        raise EvidenceError("bulk-live.jsonl is missing or empty")

    stages = ("ramp", "after-4k", "after-65k")
    expected = {
        (profile, seed, direction, transport, stage)
        for profile in PROFILES
        for seed in metadata["netem_seeds"]
        for direction in DIRECTIONS
        for transport in TRANSPORTS
        for stage in stages
    }
    observed: dict[tuple[Any, ...], dict[str, Any]] = {}
    with path.open(encoding="utf-8") as handle:
        for line_number, line in enumerate(handle, 1):
            if not line.strip():
                continue
            source = f"bulk-live.jsonl:{line_number}"
            try:
                record = json.loads(line)
            except json.JSONDecodeError as err:
                raise EvidenceError(f"{source}: invalid JSON: {err}") from err
            if not isinstance(record, dict):
                raise EvidenceError(f"{source}: record must be an object")
            profile = record.get("profile")
            seed = integer(record.get("seed"), f"{source}: seed", minimum=1)
            direction = record.get("dir")
            transport = record.get("transport")
            stage = record.get("stage")
            key = (profile, seed, direction, transport, stage)
            if key not in expected:
                raise EvidenceError(f"{source}: unexpected bulk evidence block {key}")
            if key in observed:
                raise EvidenceError(f"{source}: duplicate bulk evidence block {key}")
            expected_order = metadata["seed_orders"][str(seed)]
            if record.get("order") != expected_order:
                raise EvidenceError(f"{source}: bulk evidence has wrong transport order")
            expected_order_index = expected_order.split(",").index(transport) + 1
            if (
                integer(
                    record.get("order_index"),
                    f"{source}: order_index",
                    minimum=1,
                )
                != expected_order_index
            ):
                raise EvidenceError(f"{source}: bulk evidence has wrong order_index")
            active = integer(record.get("active"), f"{source}: active")
            started = integer(record.get("started"), f"{source}: started")
            completed = integer(record.get("completed"), f"{source}: completed")
            errors = integer(record.get("errors"), f"{source}: errors")
            corrupt = integer(record.get("corrupt"), f"{source}: corrupt")
            updated = integer(
                record.get("updated_unix_nano"),
                f"{source}: updated_unix_nano",
                minimum=1,
            )
            captured = integer(
                record.get("captured_unix_nano"),
                f"{source}: captured_unix_nano",
                minimum=1,
            )
            if (
                active != metadata["workload"]["bulk_streams"]
                or started < active
                or completed > started
                or errors != 0
                or corrupt != 0
                or captured < updated
                or captured - updated > 5_000_000_000
            ):
                raise EvidenceError(
                    f"{source}: six bulk streams were not live and error-free"
                )
            observed[key] = record

    if set(observed) != expected:
        missing = sorted(expected - set(observed), key=repr)
        raise EvidenceError(
            f"bulk evidence matrix is incomplete: {missing[:3]!r} "
            f"({len(missing)} missing)"
        )
    for profile in PROFILES:
        for seed in metadata["netem_seeds"]:
            for direction in DIRECTIONS:
                for transport in TRANSPORTS:
                    block = [
                        observed[(profile, seed, direction, transport, stage)]
                        for stage in stages
                    ]
                    for previous, current in zip(block, block[1:]):
                        if (
                            current["started"] < previous["started"]
                            or current["completed"] < previous["completed"]
                            or current["updated_unix_nano"]
                            < previous["updated_unix_nano"]
                            or current["captured_unix_nano"]
                            < previous["captured_unix_nano"]
                        ):
                            raise EvidenceError(
                                "bulk evidence counters/timestamps are not monotonic for "
                                f"{profile}/seed={seed}/{direction}/{transport}"
                            )


def require_matrix(
    records: dict[tuple[Any, ...], dict[str, Any]], metadata: dict[str, Any]
) -> list[int]:
    seeds = sorted(set(metadata["netem_seeds"]))
    expected_keys: set[tuple[Any, ...]] = set()
    for profile in PROFILES:
        for seed in seeds:
            for direction in DIRECTIONS:
                for transport in TRANSPORTS:
                    for fixture in ("beamd", "direct"):
                        for size in SIZES:
                            expected_keys.add(
                                (
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
                            )
                    expected_keys.add(
                        (
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
                    )
                    for size in INTERACTIVE_SIZES:
                        for condition in ("baseline", "underload"):
                            expected_keys.add(
                                (
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
                            )

    actual_keys = set(records)
    if actual_keys != expected_keys:
        missing = sorted(expected_keys - actual_keys, key=repr)
        extra = sorted(actual_keys - expected_keys, key=repr)
        detail = []
        if missing:
            detail.append(f"missing={missing[:3]!r} ({len(missing)} total)")
        if extra:
            detail.append(f"extra={extra[:3]!r} ({len(extra)} total)")
        raise EvidenceError("record matrix is not exact: " + "; ".join(detail))

    for key, record in records.items():
        expected_order = metadata["seed_orders"][str(key[4])]
        if record["order"] != expected_order:
            raise EvidenceError(
                f"{key[3]}/{key[5]}/seed={key[4]} does not use "
                f"metadata seed order {expected_order}"
            )

    if len(records) != 17 * len(PROFILES) * len(seeds) * len(DIRECTIONS) * len(TRANSPORTS):
        raise EvidenceError("record matrix cardinality does not match the frozen workload")
    return seeds


def selected(
    records: dict[tuple[Any, ...], dict[str, Any]],
    *,
    fixture: str = "beamd",
    workload: str = "protocol",
    transport: str,
    profile: str,
    direction: str,
    size: int,
    concurrency: int = 1,
    condition: str | None = None,
) -> list[dict[str, Any]]:
    found = [
        record
        for key, record in records.items()
        if key[0] == fixture
        and key[1] == workload
        and key[2] == transport
        and key[3] == profile
        and key[5] == direction
        and key[6] == size
        and key[7] == concurrency
        and key[8] == condition
    ]
    if not found:
        raise EvidenceError(
            "case selection unexpectedly empty: "
            f"{fixture}/{workload}/{transport}/{profile}/{direction}/{size}/"
            f"{concurrency}/{condition}"
        )
    return found


def sample_values(records: list[dict[str, Any]], field: str) -> list[float]:
    return [
        finite_number(sample[field], f"{record['_source']}: sample {field}")
        for record in records
        for sample in record["samples"]
    ]


def block_percentile(record: dict[str, Any], field: str, quantile: float) -> float:
    return percentile(sample_values([record], field), quantile)


def block_median_throughput(record: dict[str, Any]) -> float:
    throughputs = [
        float(record["size"]) / (value / 1000)
        for value in sample_values([record], "elapsed_ms")
    ]
    return statistics.median(throughputs)


def median_throughput(records: list[dict[str, Any]]) -> float:
    # Each record is one deterministic seed/order block. Summarize within the
    # block first, then aggregate block summaries so a long/fast seed cannot
    # contribute a disproportionate number of pooled raw samples.
    return statistics.median(block_median_throughput(record) for record in records)


def aggregate_throughput(records: list[dict[str, Any]]) -> float:
    return statistics.median(float(record["aggregate_throughput_bps"]) for record in records)


def elapsed(records: list[dict[str, Any]], quantile: float) -> float:
    return statistics.median(
        block_percentile(record, "elapsed_ms", quantile) for record in records
    )


def ttfb(records: list[dict[str, Any]], quantile: float) -> float:
    return statistics.median(
        block_percentile(record, "ttfb_ms", quantile) for record in records
    )


class Gates:
    def __init__(self) -> None:
        self.results: list[dict[str, Any]] = []

    def check(self, name: str, passed: bool, detail: str) -> None:
        self.results.append({"name": name, "pass": bool(passed), "detail": detail})
        marker = "PASS" if passed else "FAIL"
        print(f"[{marker}] {name}: {detail}")

    @property
    def passed(self) -> bool:
        return all(result["pass"] for result in self.results)


def ratio(numerator: float, denominator: float) -> float:
    if denominator <= 0:
        raise EvidenceError("gate denominator must be positive")
    return numerator / denominator


def paired_ratios(
    numerators: list[dict[str, Any]],
    denominators: list[dict[str, Any]],
    numerator_metric,
    denominator_metric=None,
) -> list[float]:
    if denominator_metric is None:
        denominator_metric = numerator_metric
    numerator_by_seed = {record["_seed"]: record for record in numerators}
    denominator_by_seed = {record["_seed"]: record for record in denominators}
    if (
        len(numerator_by_seed) != len(numerators)
        or len(denominator_by_seed) != len(denominators)
        or set(numerator_by_seed) != set(denominator_by_seed)
    ):
        raise EvidenceError("gate inputs do not contain one paired record per seed")
    return [
        ratio(
            numerator_metric(numerator_by_seed[seed]),
            denominator_metric(denominator_by_seed[seed]),
        )
        for seed in sorted(numerator_by_seed)
    ]


def within_block_ratios(
    records: list[dict[str, Any]], numerator_metric, denominator_metric
) -> list[float]:
    return [
        ratio(numerator_metric(record), denominator_metric(record))
        for record in sorted(records, key=lambda item: item["_seed"])
    ]


def median_ratio(values: list[float]) -> float:
    if not values:
        raise EvidenceError("cannot aggregate an empty paired ratio")
    return statistics.median(values)


def ratio_detail(value: float, values: list[float]) -> str:
    return f"median={value:.3f}x; block range={min(values):.3f}–{max(values):.3f}x"


def recurring_backoff(samples_ms: list[float]) -> tuple[bool, str]:
    # A recurring exponential ladder has samples near three adjacent
    # half-second power-of-two rungs. Requiring >= max(3, 2% of samples) in
    # every rung prevents one-off scheduler noise from failing qualification.
    rungs = (500.0, 1000.0, 2000.0, 4000.0, 8000.0)
    threshold = max(3, math.ceil(len(samples_ms) * 0.02))
    counts = Counter()
    for value in samples_ms:
        for index, center in enumerate(rungs):
            if center * 0.85 <= value <= center * 1.15:
                counts[index] += 1
                break
    recurring = any(
        all(counts[index + offset] >= threshold for offset in range(3))
        for index in range(len(rungs) - 2)
    )
    rendered = ", ".join(f"{int(rungs[i])}ms={counts[i]}" for i in range(len(rungs)))
    return recurring, f"threshold={threshold}; {rendered}"


def evaluate(records: dict[tuple[Any, ...], dict[str, Any]]) -> Gates:
    gates = Gates()

    def case(
        transport: str,
        profile: str,
        direction: str,
        size: int,
        *,
        fixture: str = "beamd",
        workload: str = "protocol",
        concurrency: int = 1,
        condition: str | None = None,
    ) -> list[dict[str, Any]]:
        return selected(
            records,
            fixture=fixture,
            workload=workload,
            transport=transport,
            profile=profile,
            direction=direction,
            size=size,
            concurrency=concurrency,
            condition=condition,
        )

    for direction in DIRECTIONS:
        for size in (16 << 20, 100 << 20):
            values = paired_ratios(
                case("quic", "clean", direction, size),
                case("quic", "clean", direction, size, fixture="direct"),
                block_median_throughput,
            )
            value = median_ratio(values)
            gates.check(
                f"quic clean direct throughput {direction} {size}",
                value >= 0.80,
                f"{ratio_detail(value, values)}; minimum 0.80",
            )

        for profile in ("lossy", "high-rtt-lossy"):
            beam_case = case("quic", profile, direction, 16 << 20)
            direct_case = case(
                "quic", profile, direction, 16 << 20, fixture="direct"
            )
            throughput_values = paired_ratios(
                beam_case, direct_case, block_median_throughput
            )
            p95_values = paired_ratios(
                beam_case,
                direct_case,
                lambda record: block_percentile(record, "elapsed_ms", .95),
            )
            throughput_ratio = median_ratio(throughput_values)
            p95_ratio = median_ratio(p95_values)
            gates.check(
                f"quic {profile} direct throughput {direction}",
                throughput_ratio >= 0.60,
                f"{ratio_detail(throughput_ratio, throughput_values)}; minimum 0.60",
            )
            gates.check(
                f"quic {profile} direct p95 {direction}",
                p95_ratio <= 2.0,
                f"{ratio_detail(p95_ratio, p95_values)}; maximum 2.0",
            )

        for size in (253 << 10, 257 << 10, 1 << 20):
            records_for_case = case("quic", "lossy", direction, size)
            p95_values = within_block_ratios(
                records_for_case,
                lambda record: block_percentile(record, "elapsed_ms", .95),
                lambda record: block_percentile(record, "elapsed_ms", .50),
            )
            max_values = within_block_ratios(
                records_for_case,
                lambda record: block_percentile(record, "elapsed_ms", 1.0),
                lambda record: block_percentile(record, "elapsed_ms", .50),
            )
            p95_ratio = max(p95_values)
            max_ratio = max(max_values)
            gates.check(
                f"quic lossy tail {direction} {size}",
                p95_ratio <= 3.0 and max_ratio <= 5.0,
                f"worst block p95={p95_ratio:.3f}x median "
                f"(block median={median_ratio(p95_values):.3f}x); "
                f"worst block max={max_ratio:.3f}x median "
                f"(block median={median_ratio(max_values):.3f}x)",
            )

        for profile in ("lossy", "high-rtt-lossy"):
            values = paired_ratios(
                case("quic", profile, direction, 16 << 20),
                case("quic", profile, direction, 16 << 20, concurrency=8),
                block_median_throughput,
                lambda record: float(record["aggregate_throughput_bps"]),
            )
            value = median_ratio(values)
            gates.check(
                f"quic {profile} solo/eight-stream {direction}",
                value >= 0.70,
                f"{ratio_detail(value, values)}; minimum 0.70",
            )

        tiny = case("quic", "high-rtt-lossy", direction, 36)
        p95_values = within_block_ratios(
            tiny,
            lambda record: block_percentile(record, "ttfb_ms", .95),
            lambda record: block_percentile(record, "ttfb_ms", .50),
        )
        p99_values = within_block_ratios(
            tiny,
            lambda record: block_percentile(record, "ttfb_ms", .99),
            lambda record: block_percentile(record, "ttfb_ms", .50),
        )
        p95_ratio = max(p95_values)
        p99_ratio = max(p99_values)
        gates.check(
            f"quic high-rtt-lossy tiny TTFB {direction}",
            p95_ratio <= 3.0 and p99_ratio <= 5.0,
            f"worst block p95={p95_ratio:.3f}x median "
            f"(block median={median_ratio(p95_values):.3f}x); "
            f"worst block p99={p99_ratio:.3f}x median "
            f"(block median={median_ratio(p99_values):.3f}x)",
        )

        for profile in ("clean", "high-rtt-clean"):
            values = paired_ratios(
                case("tcp", profile, direction, 16 << 20),
                case("tcp", profile, direction, 16 << 20, fixture="direct"),
                block_median_throughput,
            )
            value = median_ratio(values)
            gates.check(
                f"tcp {profile} direct throughput {direction}",
                value >= 0.70,
                f"{ratio_detail(value, values)}; minimum 0.70",
            )

        for size in SIZES:
            quic_case = case("quic", "clean", direction, size)
            tcp_case = case("tcp", "clean", direction, size)
            throughput_values = paired_ratios(
                quic_case, tcp_case, block_median_throughput
            )
            p95_values = paired_ratios(
                quic_case,
                tcp_case,
                lambda record: block_percentile(record, "elapsed_ms", .95),
            )
            throughput_ratio = median_ratio(throughput_values)
            p95_ratio = median_ratio(p95_values)
            gates.check(
                f"head-to-head clean {direction} {size}",
                throughput_ratio >= 0.90 and p95_ratio <= 1.10,
                f"throughput {ratio_detail(throughput_ratio, throughput_values)}; "
                f"p95 {ratio_detail(p95_ratio, p95_values)}",
            )

        qualifying = 0
        for profile in LOSSY:
            tcp_base = case(
                "tcp",
                profile,
                direction,
                4 << 10,
                workload="mixed",
                condition="baseline",
            )
            tcp_load = case(
                "tcp",
                profile,
                direction,
                4 << 10,
                workload="mixed",
                condition="underload",
            )
            quic_load = case(
                "quic",
                profile,
                direction,
                4 << 10,
                workload="mixed",
                condition="underload",
            )
            defect_values = paired_ratios(
                tcp_load,
                tcp_base,
                lambda record: block_percentile(record, "elapsed_ms", .95),
            )
            tcp_load_p95_values = [
                block_percentile(record, "elapsed_ms", .95)
                for record in sorted(tcp_load, key=lambda item: item["_seed"])
            ]
            defect_ratio = median_ratio(defect_values)
            tcp_load_p95 = statistics.median(tcp_load_p95_values)
            quic_to_tcp_values = paired_ratios(
                quic_load,
                tcp_load,
                lambda record: block_percentile(record, "elapsed_ms", .95),
            )
            quic_to_tcp = median_ratio(quic_to_tcp_values)
            defect = defect_ratio >= 3.0 and tcp_load_p95 >= 1000
            if defect:
                qualifying += 1
                improvement = 1 - quic_to_tcp
                gates.check(
                    f"A2 primary {profile} {direction}",
                    improvement >= 0.50,
                    f"TCP defect median={defect_ratio:.3f}x baseline, "
                    f"p95={tcp_load_p95:.1f}ms; QUIC cut median paired p95 "
                    f"by {improvement:.1%} (minimum 50%); "
                    f"QUIC/TCP block range={min(quic_to_tcp_values):.3f}–"
                    f"{max(quic_to_tcp_values):.3f}x",
                )
            else:
                regression = quic_to_tcp - 1
                gates.check(
                    f"A2 nonqualifying guard {profile} {direction}",
                    regression <= 0.10,
                    f"TCP defect median={defect_ratio:.3f}x baseline, "
                    f"p95={tcp_load_p95:.1f}ms; QUIC median paired p95 "
                    f"regression={regression:.1%} (maximum 10%); "
                    f"QUIC/TCP block range={min(quic_to_tcp_values):.3f}–"
                    f"{max(quic_to_tcp_values):.3f}x",
                )
        if qualifying < 1:
            raise EvidenceError(
                f"A2 is inconclusive for {direction}: no lossy profile reproduced "
                "the frozen TCP defect"
            )
        gates.check(
            f"A2 qualifying-profile presence {direction}",
            True,
            f"{qualifying} qualifying lossy profile(s); minimum 1",
        )

        for profile in ("clean", "high-rtt-clean"):
            tcp_clean = case(
                "tcp",
                profile,
                direction,
                4 << 10,
                workload="mixed",
                condition="underload",
            )
            quic_clean = case(
                "quic",
                profile,
                direction,
                4 << 10,
                workload="mixed",
                condition="underload",
            )
            clean_values = paired_ratios(
                quic_clean,
                tcp_clean,
                lambda record: block_percentile(record, "elapsed_ms", .95),
            )
            clean_ratio = median_ratio(clean_values)
            gates.check(
                f"A2 {profile} mixed guard {direction}",
                clean_ratio <= 1.10,
                f"{ratio_detail(clean_ratio, clean_values)}; maximum 1.10",
            )

        for profile in PROFILES:
            for size in (36, 253 << 10, 257 << 10, 1 << 20):
                values = paired_ratios(
                    case("quic", profile, direction, size),
                    case("tcp", profile, direction, size),
                    lambda record: block_percentile(record, "elapsed_ms", .95),
                )
                value = median_ratio(values)
                gates.check(
                    f"solo-small guard {profile} {direction} {size}",
                    value <= 1.10,
                    f"{ratio_detail(value, values)}; maximum 1.10",
                )
            for size in (16 << 20, 100 << 20):
                values = paired_ratios(
                    case("quic", profile, direction, size),
                    case("tcp", profile, direction, size),
                    block_median_throughput,
                )
                value = median_ratio(values)
                gates.check(
                    f"solo-large guard {profile} {direction} {size}",
                    value >= 0.90,
                    f"{ratio_detail(value, values)}; minimum 0.90",
                )

        for profile in PROFILES:
            ladder_blocks = []
            recurring = False
            for record in case("quic", profile, direction, 36):
                block_recurring, block_detail = recurring_backoff(
                    sample_values([record], "ttfb_ms")
                )
                recurring = recurring or block_recurring
                ladder_blocks.append(f"seed={record['_seed']}: {block_detail}")
            gates.check(
                f"QUIC timer-backoff ladder {profile} {direction}",
                not recurring,
                "; ".join(ladder_blocks),
            )
    return gates


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("results_dir", type=pathlib.Path)
    parser.add_argument("--summary", type=pathlib.Path)
    args = parser.parse_args()

    try:
        metadata = load_metadata(args.results_dir)
        records = load_records(args.results_dir)
        seeds = require_matrix(records, metadata)
        validate_bulk_evidence(args.results_dir, metadata)
        print(
            f"Validated {len(records)} unique cases across {len(seeds)} seeds; "
            "all samples are present and error-free."
        )
        gates = evaluate(records)
    except (EvidenceError, OSError) as err:
        print(f"INCONCLUSIVE: {err}", file=sys.stderr)
        if args.summary:
            inconclusive = {
                "schema_version": 1,
                "verdict": "inconclusive",
                "error": str(err),
            }
            args.summary.parent.mkdir(parents=True, exist_ok=True)
            args.summary.write_text(
                json.dumps(inconclusive, indent=2) + "\n",
                encoding="utf-8",
            )
        return 2

    summary = {
        "schema_version": 1,
        "verdict": "pass" if gates.passed else "fail",
        "cases": len(records),
        "seeds": seeds,
        "gates": gates.results,
    }
    if args.summary:
        args.summary.parent.mkdir(parents=True, exist_ok=True)
        args.summary.write_text(json.dumps(summary, indent=2) + "\n", encoding="utf-8")
    print(f"\nB4 VERDICT: {summary['verdict'].upper()}")
    return 0 if gates.passed else 1


if __name__ == "__main__":
    sys.exit(main())
