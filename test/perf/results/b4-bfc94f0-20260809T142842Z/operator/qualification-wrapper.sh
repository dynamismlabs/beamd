#!/usr/bin/env bash
set -Eeuo pipefail

TARGET_UNIT=beamd-b4-targeted-mixed-bfc94f0.service
TARGET_OUTDIR=/var/lib/beamd-b4/evidence/targeted-mixed-bfc94f0-20260809T142842Z
OUTDIR=/var/lib/beamd-b4/evidence/b4-bfc94f0-20260809T142842Z
CONSOLE=/var/lib/beamd-b4/qualification-bfc94f0-20260809T142842Z.console.log
CHECKOUT=/opt/beamd-b4-bfc94f0
BINDIR=/var/lib/beamd-b4/bundle-bfc94f0
COMMIT=bfc94f03163fbfb4d83f46e166ceef5dccde12e1

restore_host() {
  systemctl start beamd.service || true
  systemctl start apt-daily.timer apt-daily-upgrade.timer || true
}
trap restore_host EXIT INT TERM

{
  echo "chain_started_utc=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "waiting_for=$TARGET_UNIT"
  echo "commit=$COMMIT"
  echo "outdir=$OUTDIR"
} >"$CONSOLE"

while true; do
  target_state=$(systemctl show "$TARGET_UNIT" -p ActiveState --value)
  case "$target_state" in
    active|activating|deactivating) sleep 15 ;;
    *) break ;;
  esac
done

target_result=$(systemctl show "$TARGET_UNIT" -p Result --value)
target_status=$(systemctl show "$TARGET_UNIT" -p ExecMainStatus --value)
{
  echo "target_result=$target_result"
  echo "target_status=$target_status"
} >>"$CONSOLE"
test "$target_result" = success
test "$target_status" = 0

python3 - "$TARGET_OUTDIR" "$COMMIT" >>"$CONSOLE" <<'PY'
import json
import pathlib
import sys

root = pathlib.Path(sys.argv[1])
commit = sys.argv[2]
summary = json.loads((root / "targeted-summary.json").read_text(encoding="utf-8"))
metadata = json.loads((root / "metadata.json").read_text(encoding="utf-8"))
manifest = json.loads((root / "manifest.json").read_text(encoding="utf-8"))
integrity = json.loads(
    (root / "targeted-integrity.json").read_text(encoding="utf-8")
)

expected = {
    "passed": True,
    "workload": "mixed",
    "profile": "high-rtt-lossy",
    "seed": 101,
    "direction": "upload",
    "transport_order": ["quic", "tcp"],
    "warmups_per_case": 8,
    "iterations_per_case": 50,
    "bulk_streams": 6,
    "bulk_bytes": 8_388_608,
    "bulk_ramp_seconds": 5,
    "collection_status": 0,
    "raw_failures": [],
    "issues": [],
}
for field, value in expected.items():
    if summary.get(field) != value:
        raise SystemExit(
            f"targeted summary {field}={summary.get(field)!r}, want {value!r}"
        )

stages = summary.get("stage_results")
expected_stages = [("beamd", "quic"), ("beamd", "tcp")]
if not isinstance(stages, list) or [
    (stage.get("fixture"), stage.get("transport")) for stage in stages
] != expected_stages:
    raise SystemExit(f"targeted summary has unexpected stages: {stages!r}")
if any(
    stage.get("outcome") != "passed" or stage.get("exit_status") != 0
    for stage in stages
):
    raise SystemExit(f"targeted summary contains a failed stage: {stages!r}")
if len(summary.get("records", [])) != 8:
    raise SystemExit("targeted summary does not contain all eight mixed records")
if len(summary.get("bulk_live", [])) != 6:
    raise SystemExit("targeted summary does not contain all six bulk snapshots")

target_case = metadata.get("target_case")
expected_target_case = {
    "workload": "mixed",
    "profile": "high-rtt-lossy",
    "seed": 101,
    "direction": "upload",
    "transport_order": "quic,tcp",
    "warmups": 8,
    "interactive_bytes": [4096, 66560],
    "interactive_concurrency": 1,
    "iterations": 50,
    "bulk_streams": 6,
    "bulk_bytes": 8_388_608,
    "bulk_ramp_seconds": 5,
}
if target_case != expected_target_case:
    raise SystemExit(f"targeted metadata case mismatch: {target_case!r}")
if manifest.get("commit") != commit or metadata.get("beamd_commit") != commit:
    raise SystemExit("targeted candidate commit mismatch")
if any(
    integrity.get(field) != 0
    for field in (
        "immutable_inputs_status",
        "traffic_control_status",
        "recorded_assets_status",
    )
):
    raise SystemExit(f"targeted integrity failure: {integrity!r}")
if (root / "raw-failures.jsonl").exists():
    raise SystemExit("targeted evidence unexpectedly contains raw-failures.jsonl")
print("targeted_mixed_gate=passed")
PY

test "$(git -C "$CHECKOUT" rev-parse HEAD)" = "$COMMIT"
test -z "$(git -C "$CHECKOUT" status --porcelain --untracked-files=all)"
test ! -e "$OUTDIR"
test -z "$(ip netns list)"

systemctl stop apt-daily.timer apt-daily-upgrade.timer
systemctl stop beamd.service

{
  echo "qualification_started_utc=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "normal_services_paused=true"
} >>"$CONSOLE"

set +e
env \
  MODE=qualification \
  OUTDIR="$OUTDIR" \
  BINDIR="$BINDIR" \
  TC_BIN=/opt/beamd-b4-tools/iproute2-6.8.0-bin/tc \
  GOMEMLIMIT=1400MiB \
  "$CHECKOUT/scripts/perf-netem.sh" run >>"$CONSOLE" 2>&1
status=$?
set -e

{
  echo "finished_utc=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "harness_status=$status"
  echo "exit_status=$status"
} >>"$CONSOLE"
exit "$status"
