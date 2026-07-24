#!/usr/bin/env bash
#
# perf-g1-local.sh — controlled LOCAL reproduction of the A2 symptom.
#
# Two real beamd processes in two containers on a docker bridge: a real
# `beamd serve` edge, and a shaped agent (netem on the agent->edge leg, applied
# BEFORE the agent dials). perfclient runs on the HOST against the edge's
# published port, so the public leg is unshaped. This is NOT the production WAN
# link — it is a clean, reproducible reproduction to answer "does A2 occur in
# beamd's transport under loss?" If it fires here, confirm on a real edge before
# building Part B.
#
# Requires: docker (privileged), a host perfclient, linux beamd + perfserver.
set -euo pipefail

BINDIR=${BINDIR:?dir with beamd-linux-* + perfserver-linux-*}
BEAMD_BIN=${BEAMD_BIN:-beamd-linux-arm64}
PERFSERVER_BIN=${PERFSERVER_BIN:-perfserver-linux-arm64}
AGENT_ENTRY=${AGENT_ENTRY:?path to perf-g1-agent.sh}
PERFCLIENT=${PERFCLIENT:?host perfclient binary}
IMAGE=${IMAGE:-nicolaka/netshoot}
OUTDIR=${OUTDIR:-./g1-local-out}
PROFILES=${PROFILES:-clean lossy}

NET=perfnet-g1
EDGE=perf-edge-local
AGENT=perf-agent-local
PUB=18443
SLUG=perf; BASE=perf.local; HOST="blob-$SLUG.$BASE"; TOKEN=PERFTOKEN
WORK=$(mktemp -d)
mkdir -p "$OUTDIR"

cleanup() {
  set +e
  docker rm -f "$AGENT" "$EDGE" >/dev/null 2>&1
  docker network rm "$NET" >/dev/null 2>&1
  rm -rf "$WORK"
}
trap cleanup EXIT

profile_params() {
  case "$1" in
    clean) echo "75 0 100" ;;
    lossy) echo "75 1 100" ;;
    high-rtt-clean) echo "250 0 20" ;;
    high-rtt-lossy) echo "250 1 20" ;;
    *) echo "unknown profile $1" >&2; exit 1 ;;
  esac
}

# --- edge config + real edge container ---
cat > "$WORK/edge.yaml" <<YAML
base_domain: $BASE
url_shape: hyphen
listen_https: ":443"
acme_email: perf@example.com
acme_ca: "off"
dns_provider: stub
token_store: "file:/perf/cfg/tokens.json"
data_dir: /tmp/edge-data
request_log:
  enabled: false
YAML
printf '{"%s":"%s"}\n' "$TOKEN" "$SLUG" > "$WORK/tokens.json"

docker network create "$NET" >/dev/null 2>&1 || true
docker rm -f "$EDGE" >/dev/null 2>&1 || true
docker run -d --name "$EDGE" --network "$NET" -p "$PUB:443" \
  -v "$BINDIR:/perf/bin:ro" -v "$WORK:/perf/cfg:ro" \
  --entrypoint "/perf/bin/$BEAMD_BIN" "$IMAGE" serve --config /perf/cfg/edge.yaml >/dev/null
for _ in $(seq 1 40); do
  curl -sk --max-time 2 "https://127.0.0.1:$PUB/healthz" >/dev/null 2>&1 && break
  sleep 0.25
done

{
  echo "# G1 LOCAL controlled reproduction (NOT the production WAN link)"
  echo "date_utc: $(date -u +%FT%TZ)"
  echo "beamd: $("$BINDIR/$BEAMD_BIN" version 2>/dev/null || echo n/a)"
  echo "topology: real beamd edge container + shaped agent container on a docker bridge;"
  echo "  agent->edge leg shaped with netem BEFORE dial; host->edge public leg unshaped."
  echo "profiles: $PROFILES"
} > "$OUTDIR/metadata.txt"

cc() { # size dir n warmup conc raw [timeout]
  "$PERFCLIENT" --url "https://$HOST:$PUB" --resolve "$HOST:127.0.0.1" --insecure \
    --size "$1" --dir "$2" --n "$3" --warmup "$4" --concurrency "$5" \
    --profile "$PROFILE" --transport tcp ${6:+--raw} ${7:+--timeout "$7"} >> "$OUTDIR/raw-$PROFILE.jsonl"
}

for PROFILE in $PROFILES; do
  read -r d l r <<< "$(profile_params "$PROFILE")"
  echo ">>> $PROFILE delay=${d}ms loss=${l}% rate=${r}mbit (RTT ~$((2 * d))ms)"
  : > "$OUTDIR/raw-$PROFILE.jsonl"
  docker rm -f "$AGENT" >/dev/null 2>&1 || true
  docker run -d --privileged --name "$AGENT" --network "$NET" \
    -v "$BINDIR:/perf/bin:ro" -v "$AGENT_ENTRY:/perf-g1-agent.sh:ro" \
    -e EDGE_ADDR="$EDGE:443" -e TOKEN="$TOKEN" \
    -e BEAMD="/perf/bin/$BEAMD_BIN" -e PERFSERVER="/perf/bin/$PERFSERVER_BIN" \
    -e DELAY_MS="$d" -e LOSS_PCT="$l" -e RATE_MBIT="$r" -e NAME=blob \
    --entrypoint bash "$IMAGE" /perf-g1-agent.sh >/dev/null

  ok=""
  for _ in $(seq 1 80); do
    code=$(curl -sk -o /dev/null -w '%{http_code}' --max-time 6 \
      --resolve "$HOST:$PUB:127.0.0.1" "https://$HOST:$PUB/download?n=1" 2>/dev/null || true)
    [ "$code" = "200" ] && { ok=1; break; }
    sleep 1
  done
  if [ -z "$ok" ]; then
    echo "!! tunnel never served ($PROFILE); agent log:" >&2
    docker logs "$AGENT" 2>&1 | tail -18 >&2
    docker rm -f "$AGENT" >/dev/null 2>&1 || true; continue
  fi
  { echo "## $PROFILE"; docker exec "$AGENT" tc qdisc show dev eth0; } >> "$OUTDIR/metadata.txt"

  # Large-transfer size is chosen per profile so the 8-stream case COMPLETES
  # under the loss-limited pipe within the timeout (8 streams share the same
  # loss-bound TCP throughput, so each is ~1/8 speed). timeout raised for the
  # large cases accordingly.
  case "$PROFILE" in
    clean|lossy)
      cc 36 download 100 5 1 raw
      cc 36 download 100 0 8 ""
      cc 259072 download 50 5 1 raw
      cc 259072 download 48 0 8 ""
      cc 8388608 download 10 2 1 raw 480s   # 8 MiB (> A1 window; sustained)
      cc 8388608 download 8 0 8 "" 480s
      ;;
    high-rtt-clean|high-rtt-lossy)
      cc 36 download 60 5 1 raw
      cc 36 download 60 0 8 ""
      cc 259072 download 20 3 1 raw 480s
      cc 259072 download 16 0 8 "" 480s
      cc 524288 download 8 2 1 raw 480s      # 512 KiB (heavy-loss pipe is ~30 KB/s)
      cc 524288 download 8 0 8 "" 480s
      ;;
  esac
  docker rm -f "$AGENT" >/dev/null 2>&1 || true
done

echo "DONE — results in $OUTDIR"
