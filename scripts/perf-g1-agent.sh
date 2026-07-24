#!/usr/bin/env bash
#
# perf-g1-agent.sh — container entrypoint for the real-edge G1 measurement
# (transport-performance-spec §16 G1).
#
# Runs INSIDE a privileged Linux container that acts as the beamd AGENT against
# a REAL remote edge. It shapes the agent's egress to the edge (the download
# data path, agent -> edge) with netem BEFORE beamd dials — so TCP's RTT/RTO are
# calibrated under the impaired path from the first packet, not pre-trained on a
# clean path and then yanked (the methodology bug the earlier synthetic harness
# had). The public perfclient -> edge leg runs on the host and is never shaped.
#
# Env: EDGE_ADDR (host:port of the real edge), TOKEN, DELAY_MS, LOSS_PCT,
#      RATE_MBIT, [BACKEND_PORT=9000], [WINDOW=4194304], [DEV=eth0], [NAME=blob].
set -euo pipefail
: "${EDGE_ADDR:?}" "${TOKEN:?}" "${DELAY_MS:?}" "${LOSS_PCT:?}" "${RATE_MBIT:?}"
BEAMD=${BEAMD:-/perf/bin/beamd}
PERFSERVER=${PERFSERVER:-/perf/bin/perfserver}
BACKEND_PORT=${BACKEND_PORT:-9000}
WINDOW=${WINDOW:-4194304}
DEV=${DEV:-eth0}
NAME=${NAME:-blob}

# 1) Shape agent->edge egress BEFORE dialing. This is the download data path;
#    the return (ACK) path and the host's public leg are unshaped, so the
#    impairment is isolated to the tunnel's forward direction where A2's
#    loss-recovery dynamics live.
LOSS_CORR=${LOSS_CORR:-} # optional netem loss correlation % (bursty loss)
tc qdisc del dev "$DEV" root 2>/dev/null || true
if [ "$LOSS_PCT" = "0" ]; then
  tc qdisc add dev "$DEV" root netem delay "${DELAY_MS}ms" rate "${RATE_MBIT}mbit"
elif [ -n "$LOSS_CORR" ] && [ "$LOSS_CORR" != "0" ]; then
  tc qdisc add dev "$DEV" root netem delay "${DELAY_MS}ms" loss "${LOSS_PCT}%" "${LOSS_CORR}%" rate "${RATE_MBIT}mbit"
else
  tc qdisc add dev "$DEV" root netem delay "${DELAY_MS}ms" loss "${LOSS_PCT}%" rate "${RATE_MBIT}mbit"
fi
echo "shaped $DEV before dial: $(tc qdisc show dev "$DEV")"

# 2) deterministic backend on loopback (unshaped; local to the agent).
BEAMD_YAMUX_STREAM_WINDOW_BYTES="$WINDOW" "$PERFSERVER" --addr "127.0.0.1:$BACKEND_PORT" &
sleep 0.5

# 3) connect to the REAL edge over the (now-shaped) path and serve the tunnel.
cat > /tmp/client.yaml <<YAML
server: $EDGE_ADDR
token: $TOKEN
insecure_skip_verify: true
agent_socket: /tmp/agent.sock
YAML
echo "dialing real edge $EDGE_ADDR (shaped), registering $NAME -> :$BACKEND_PORT ..."
exec env HOME=/tmp BEAMD_YAMUX_STREAM_WINDOW_BYTES="$WINDOW" \
  "$BEAMD" open "$BACKEND_PORT" --as "$NAME" --config /tmp/client.yaml
