#!/usr/bin/env bash
#
# perf-hol.sh — production-scenario / head-of-line-blocking test.
#
# Recreates the real risk in beamd's architecture: many concurrent visitors to
# ONE app all multiplex over ONE edge<->agent tunnel TCP connection. Under loss
# on that leg, TCP in-order delivery means one lost packet stalls EVERY stream
# behind it — so a latency-sensitive request can get stuck behind someone else's
# bulk transfer (head-of-line blocking, the A2 half QUIC most directly fixes).
#
# For each connection profile it measures an interactive request's latency
# ALONE (baseline) and UNDER a bulk load of concurrent "visitors", on the same
# tunnel. The clean control separates HoL blocking (slow only WITH loss) from
# plain bandwidth starvation (slow even without loss — QUIC would not fix that).
#
# Real edge + shaped agent, both real beamd processes; interactive + load
# clients run on the unshaped host. NOT the production WAN link.
set -euo pipefail

BINDIR=${BINDIR:?dir with beamd-linux-* + perfserver-linux-*}
BEAMD_BIN=${BEAMD_BIN:-beamd-linux-arm64}
PERFSERVER_BIN=${PERFSERVER_BIN:-perfserver-linux-arm64}
AGENT_ENTRY=${AGENT_ENTRY:?path to perf-g1-agent.sh}
PERFCLIENT=${PERFCLIENT:?host perfclient binary}
IMAGE=${IMAGE:-nicolaka/netshoot}
OUTDIR=${OUTDIR:-./hol-out}
PROFILES=${PROFILES:-clean wifi mobile bursty}
LOAD_CONC=${LOAD_CONC:-6}          # concurrent bulk "visitors"
LOAD_SIZE=${LOAD_SIZE:-8388608}    # 8 MiB bulk transfers
INTER_N=${INTER_N:-50}             # measured interactive requests per case

NET=perfnet-hol; EDGE=perf-hol-edge; AGENT=perf-hol-agent; PUB=18444
SLUG=perf; BASE=perf.local; HOST="blob-$SLUG.$BASE"; TOKEN=PERFTOKEN
WORK=$(mktemp -d); mkdir -p "$OUTDIR"

cleanup() {
  set +e
  [ -n "${LOAD_PID:-}" ] && kill "$LOAD_PID" 2>/dev/null
  docker rm -f "$AGENT" "$EDGE" >/dev/null 2>&1
  docker network rm "$NET" >/dev/null 2>&1
  rm -rf "$WORK"
}
trap cleanup EXIT

profile_params() { # delay_ms loss_pct rate_mbit loss_corr  (real connection conditions)
  case "$1" in
    clean)  echo "30 0 100 0" ;;    # control: good link, no loss
    home)   echo "25 0.1 100 0" ;;  # home broadband
    wifi)   echo "40 1 30 0" ;;     # cafe/office wifi
    mobile) echo "150 2 10 0" ;;    # 4G / hotspot
    bursty) echo "60 2 15 25" ;;    # congested wifi: 2% loss, 25% correlation (bursty)
    *) echo "unknown profile $1" >&2; exit 1 ;;
  esac
}

# --- one real edge for the whole run ---
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
for _ in $(seq 1 40); do curl -sk --max-time 2 "https://127.0.0.1:$PUB/healthz" >/dev/null 2>&1 && break; sleep 0.25; done

{
  echo "# HoL / production-scenario test (controlled reproduction, NOT the WAN link)"
  echo "date_utc: $(date -u +%FT%TZ)"
  echo "beamd: $("$BINDIR/$BEAMD_BIN" version 2>/dev/null || echo n/a)"
  echo "load: $LOAD_CONC concurrent $((LOAD_SIZE >> 20)) MiB downloads (many visitors) sharing the tunnel"
  echo "interactive: 4 KiB (API) and 65 KiB (page) downloads, measured alone vs under load"
  echo "profiles: $PROFILES"
} > "$OUTDIR/metadata.txt"

# inter <size> <cond-label> : measure the interactive request, tag baseline/underload.
inter() {
  "$PERFCLIENT" --url "https://$HOST:$PUB" --resolve "$HOST:127.0.0.1" --insecure \
    --size "$1" --dir download --n "$INTER_N" --warmup 8 --concurrency 1 --raw \
    --profile "$PROFILE" --transport "$2" --timeout 120s >> "$OUTDIR/raw-$PROFILE.jsonl"
}

for PROFILE in $PROFILES; do
  read -r d l r corr <<< "$(profile_params "$PROFILE")"
  echo ">>> $PROFILE delay=${d}ms loss=${l}%${corr:+/${corr}%corr} rate=${r}mbit"
  : > "$OUTDIR/raw-$PROFILE.jsonl"
  docker rm -f "$AGENT" >/dev/null 2>&1 || true
  docker run -d --privileged --name "$AGENT" --network "$NET" \
    -v "$BINDIR:/perf/bin:ro" -v "$AGENT_ENTRY:/perf-g1-agent.sh:ro" \
    -e EDGE_ADDR="$EDGE:443" -e TOKEN="$TOKEN" \
    -e BEAMD="/perf/bin/$BEAMD_BIN" -e PERFSERVER="/perf/bin/$PERFSERVER_BIN" \
    -e DELAY_MS="$d" -e LOSS_PCT="$l" -e RATE_MBIT="$r" -e LOSS_CORR="$corr" -e NAME=blob \
    --entrypoint bash "$IMAGE" /perf-g1-agent.sh >/dev/null
  ok=""
  for _ in $(seq 1 80); do
    code=$(curl -sk -o /dev/null -w '%{http_code}' --max-time 6 --resolve "$HOST:$PUB:127.0.0.1" "https://$HOST:$PUB/download?n=1" 2>/dev/null || true)
    [ "$code" = "200" ] && { ok=1; break; }; sleep 1
  done
  if [ -z "$ok" ]; then echo "!! tunnel not up ($PROFILE)"; docker logs "$AGENT" 2>&1 | tail -15; docker rm -f "$AGENT"; continue; fi
  { echo "## $PROFILE"; docker exec "$AGENT" tc qdisc show dev eth0; } >> "$OUTDIR/metadata.txt"

  # (1) BASELINE — interactive alone.
  inter 4096 baseline
  inter 65536 baseline

  # (2) UNDER LOAD — start bulk load (many visitors), let it ramp, then measure.
  "$PERFCLIENT" --url "https://$HOST:$PUB" --resolve "$HOST:127.0.0.1" --insecure \
    --size "$LOAD_SIZE" --dir download --n 100000 --warmup 0 --concurrency "$LOAD_CONC" \
    --timeout 600s >/dev/null 2>&1 &
  LOAD_PID=$!
  sleep 5
  inter 4096 underload
  inter 65536 underload
  kill "$LOAD_PID" 2>/dev/null; wait "$LOAD_PID" 2>/dev/null || true; LOAD_PID=""

  docker rm -f "$AGENT" >/dev/null 2>&1 || true
done

echo "DONE — results in $OUTDIR"
