#!/usr/bin/env bash
#
# perf-g1.sh — real-edge G1 measurement orchestrator
# (transport-performance-spec §16 G1: prove the A2 symptom against the REAL edge
# before any Part B / QUIC work).
#
# For each impairment profile it launches a shaped AGENT container (see
# perf-g1-agent.sh) that connects to a REAL remote edge, waits for the tunnel to
# serve, then drives perfclient FROM THE HOST (the unshaped public leg) across
# the size x concurrency matrix and records one JSON object per case. Only the
# agent -> edge leg is impaired; the host -> edge public leg never is.
#
# Requires: docker (privileged), a host perfclient, a reachable real edge, and a
# token/slug for it.
#
#   EDGE_IP=... TOKEN=... SLUG=perf BASE=perf.local EDGE_PORT=8443 \
#   BINDIR=$PWD/bin AGENT_ENTRY=$PWD/scripts/perf-g1-agent.sh \
#   PERFCLIENT=$PWD/bin/perfclient OUTDIR=$PWD/g1-out bash scripts/perf-g1.sh
set -euo pipefail

: "${EDGE_IP:?real edge IP}" "${TOKEN:?edge token}"
EDGE_PORT=${EDGE_PORT:-8443}
SLUG=${SLUG:-perf}
BASE=${BASE:-perf.local}
HOST="blob-$SLUG.$BASE" # hyphen url_shape: <name>-<slug>.<base>
BINDIR=${BINDIR:?dir with linux beamd + perfserver}
AGENT_ENTRY=${AGENT_ENTRY:?path to perf-g1-agent.sh}
PERFCLIENT=${PERFCLIENT:?host perfclient binary}
IMAGE=${IMAGE:-nicolaka/netshoot}
OUTDIR=${OUTDIR:-./g1-out}
PROFILES=${PROFILES:-clean lossy high-rtt-lossy}
CNAME=perf-g1-agent
mkdir -p "$OUTDIR"

profile_params() {
  case "$1" in
    clean) echo "75 0 100" ;;
    lossy) echo "75 1 100" ;;
    high-rtt-clean) echo "250 0 20" ;;
    high-rtt-lossy) echo "250 1 20" ;;
    *) echo "unknown profile $1" >&2; exit 1 ;;
  esac
}

cleanup() { docker rm -f "$CNAME" >/dev/null 2>&1 || true; }
trap cleanup EXIT

{
  echo "# G1 real-edge measurement"
  echo "date_utc: $(date -u +%FT%TZ)"
  echo "edge: $EDGE_IP:$EDGE_PORT host=$HOST  (REAL remote edge over WAN)"
  echo "beamd: $("$BINDIR/beamd" version 2>/dev/null || echo n/a)"
  echo "agent: shaped container; egress netem applied BEFORE dial (download data path)"
  echo "public leg host->edge: UNSHAPED"
  echo "profiles: $PROFILES"
} > "$OUTDIR/metadata.txt"

cc() { # size dir n warmup conc raw
  "$PERFCLIENT" --url "https://$HOST:$EDGE_PORT" --resolve "$HOST:$EDGE_IP" --insecure \
    --size "$1" --dir "$2" --n "$3" --warmup "$4" --concurrency "$5" \
    --profile "$PROFILE" --transport tcp ${6:+--raw} >> "$OUTDIR/raw-$PROFILE.jsonl"
}

for PROFILE in $PROFILES; do
  read -r d l r <<< "$(profile_params "$PROFILE")"
  echo ">>> $PROFILE delay=${d}ms loss=${l}% rate=${r}mbit"
  : > "$OUTDIR/raw-$PROFILE.jsonl"

  cleanup
  docker run -d --privileged --name "$CNAME" \
    -v "$BINDIR:/perf/bin:ro" -v "$AGENT_ENTRY:/perf-g1-agent.sh:ro" \
    -e EDGE_ADDR="$EDGE_IP:$EDGE_PORT" -e TOKEN="$TOKEN" \
    -e DELAY_MS="$d" -e LOSS_PCT="$l" -e RATE_MBIT="$r" -e NAME=blob \
    --entrypoint bash "$IMAGE" /perf-g1-agent.sh >/dev/null

  # wait for the real-edge tunnel to serve (host curl, UNSHAPED public leg).
  ok=""
  for _ in $(seq 1 90); do
    code=$(curl -sk -o /dev/null -w '%{http_code}' --max-time 6 \
      --resolve "$HOST:$EDGE_PORT:$EDGE_IP" "https://$HOST:$EDGE_PORT/download?n=1" 2>/dev/null || true)
    [ "$code" = "200" ] && { ok=1; break; }
    sleep 1
  done
  if [ -z "$ok" ]; then
    echo "!! tunnel never served for $PROFILE (last code=$code); agent log:" >&2
    docker logs "$CNAME" 2>&1 | tail -20 >&2
    cleanup; continue
  fi
  { echo "## $PROFILE"; docker exec "$CNAME" tc qdisc show dev eth0; } >> "$OUTDIR/metadata.txt"

  # download-direction matrix (the A2-critical direction). solo (conc1) vs 8-stream.
  cc 36 download 100 5 1 raw
  cc 36 download 100 0 8 ""
  cc 259072 download 50 5 1 raw
  cc 259072 download 48 0 8 ""
  cc 16777216 download 10 2 1 raw
  cc 16777216 download 16 0 8 ""

  cleanup
done

echo "DONE — results in $OUTDIR"
