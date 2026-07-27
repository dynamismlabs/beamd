#!/usr/bin/env bash
#
# Deterministic B4 transport qualification harness.
#
# build: compile the exact checkout into a manifest-backed binary bundle.
# run:   create two Linux network namespaces, shape only their veth link in
#        both directions, run TCP/QUIC/direct fixtures, then fail closed through
#        test/perf/b4_analyze.py.
#
# Qualification is intentionally privileged and long-running. A smoke mode is
# available for harness development, but its metadata can never pass the B4
# analyzer.
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
ACTION=${1:-}
BINDIR=${BINDIR:-"$ROOT/bin/perf-b4"}
MODE=${MODE:-qualification}
IMAGE_TAG=${IMAGE_TAG:-local}

usage() {
  cat <<'EOF'
Usage:
  scripts/perf-netem.sh build
  sudo -E scripts/perf-netem.sh run
  scripts/perf-netem.sh analyze RESULTS_DIR

Environment:
  BINDIR=/path           binary bundle (default: bin/perf-b4)
  OUTDIR=/path           new evidence directory (run only)
  MODE=qualification     full, fail-closed run (default)
  MODE=smoke             reduced harness check; never valid B4 evidence
  NETEM_SEEDS="101 202 303"
  GOMEMLIMIT=1400MiB

Qualification prerequisites: Linux, root for `run`, iproute2 (ip/tc), ethtool,
openssl, curl, Python 3.10+, and a clean checkout for `build`.
EOF
}

sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

read_limit_file() {
  if [ -r "$1" ]; then
    tr '\n' ' ' < "$1" | sed 's/[[:space:]]*$//'
  else
    printf unavailable
  fi
}

source_status() {
  git -C "$ROOT" status --porcelain --untracked-files=all -- \
    . ':(exclude,glob)test/perf/results/b4-*/**'
}

build_bundle() {
  for command in go git python3; do
    command -v "$command" >/dev/null 2>&1 || {
      echo "missing required command: $command" >&2
      exit 2
    }
  done
  local commit dirty
  commit=$(git -C "$ROOT" rev-parse HEAD)
  dirty=false
  if [ -n "$(source_status)" ]; then
    dirty=true
  fi
  if [ "$dirty" = true ] && [ "${ALLOW_DIRTY_BUILD:-0}" != 1 ]; then
    echo "refusing qualification build from a dirty checkout; commit the implementation first" >&2
    echo "set ALLOW_DIRTY_BUILD=1 only for MODE=smoke development" >&2
    exit 2
  fi
  if [ "$dirty" = true ] && [ "$MODE" != smoke ]; then
    echo "ALLOW_DIRTY_BUILD is permitted only with MODE=smoke" >&2
    exit 2
  fi
  if [ "$(uname -s)" != Linux ] || [ "$(go env GOOS)" != linux ]; then
    echo "build must run natively on the Linux qualification host" >&2
    exit 2
  fi

  mkdir -p "$BINDIR"
  go build -trimpath -ldflags "-s -w -X main.Version=$commit" -o "$BINDIR/beamd" ./cmd/beamd
  go build -trimpath -o "$BINDIR/perfclient" ./test/perf/perfclient
  go build -trimpath -o "$BINDIR/perfserver" ./test/perf/perfserver
  go build -trimpath -o "$BINDIR/directclient" ./test/perf/directclient
  go build -trimpath -o "$BINDIR/directserver" ./test/perf/directserver
  cp "$ROOT/test/perf/b4_analyze.py" "$BINDIR/b4_analyze.py"

  local manifest_tmp
  manifest_tmp=$(mktemp)
  local go_version
  go_version=$(go version)
  python3 - "$BINDIR" "$commit" "$dirty" "$IMAGE_TAG" "$go_version" "$ROOT" > "$manifest_tmp" <<'PY'
import hashlib
import json
import pathlib
import sys

root = pathlib.Path(sys.argv[1])
source_root = pathlib.Path(sys.argv[6])
names = ("beamd", "perfclient", "perfserver", "directclient", "directserver")
hashes = {}
for name in names:
    data = (root / name).read_bytes()
    hashes[name] = hashlib.sha256(data).hexdigest()
json.dump(
    {
        "schema_version": 1,
        "commit": sys.argv[2],
        "dirty": sys.argv[3] == "true",
        "image_tag": sys.argv[4],
        "go_version": sys.argv[5],
        "binaries": hashes,
        "assets": {
            "b4_analyze.py": hashlib.sha256(
                (root / "b4_analyze.py").read_bytes()
            ).hexdigest(),
            "test/perf/b4_analyze.py": hashlib.sha256(
                (source_root / "test/perf/b4_analyze.py").read_bytes()
            ).hexdigest(),
            "scripts/perf-netem.sh": hashlib.sha256(
                (source_root / "scripts/perf-netem.sh").read_bytes()
            ).hexdigest(),
        },
    },
    sys.stdout,
    indent=2,
)
sys.stdout.write("\n")
PY
  mv "$manifest_tmp" "$BINDIR/manifest.json"
  echo "built B4 bundle: $BINDIR"
}

if [ "$ACTION" = build ]; then
  build_bundle
  exit 0
fi
if [ "$ACTION" = analyze ]; then
  [ "$#" -eq 2 ] || { usage >&2; exit 2; }
  command -v python3 >/dev/null 2>&1 || {
    echo "Python 3.10 or newer is required" >&2
    exit 2
  }
  python3 -c 'import sys; sys.exit(0 if sys.version_info >= (3, 10) else 2)' || {
    echo "Python 3.10 or newer is required" >&2
    exit 2
  }
  ANALYZER="$ROOT/test/perf/b4_analyze.py"
  [ ! -f "$2/b4_analyze.py" ] || ANALYZER="$2/b4_analyze.py"
  exec python3 "$ANALYZER" "$2" --summary "$2/summary.json"
fi
if [ "$ACTION" != run ]; then
  usage >&2
  exit 2
fi
if [ "$MODE" != qualification ] && [ "$MODE" != smoke ]; then
  echo "MODE must be qualification or smoke" >&2
  exit 2
fi
if [ "$(uname -s)" != Linux ]; then
  echo "run requires Linux network namespaces and netem" >&2
  exit 2
fi
if [ "$(id -u)" -ne 0 ]; then
  echo "run requires root; build the bundle first, then use sudo -E" >&2
  exit 2
fi
for command in ip tc ethtool openssl curl python3 git; do
  command -v "$command" >/dev/null 2>&1 || {
    echo "missing required command: $command" >&2
    exit 2
  }
done
python3 -c 'import sys; sys.exit(0 if sys.version_info >= (3, 10) else 2)' || {
  echo "Python 3.10 or newer is required" >&2
  exit 2
}
for binary in beamd perfclient perfserver directclient directserver; do
  [ -x "$BINDIR/$binary" ] || {
    echo "missing $BINDIR/$binary; run scripts/perf-netem.sh build first" >&2
    exit 2
  }
done
[ -f "$BINDIR/b4_analyze.py" ] || {
  echo "missing $BINDIR/b4_analyze.py; rebuild the bundle" >&2
  exit 2
}
[ -f "$BINDIR/manifest.json" ] || {
  echo "missing $BINDIR/manifest.json; rebuild the bundle" >&2
  exit 2
}
MANIFEST_SHA=$(sha256_file "$BINDIR/manifest.json")

COMMIT=$(git -C "$ROOT" rev-parse HEAD)
MANIFEST_COMMIT=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["commit"])' "$BINDIR/manifest.json")
MANIFEST_DIRTY=$(python3 -c 'import json,sys; print(str(json.load(open(sys.argv[1]))["dirty"]).lower())' "$BINDIR/manifest.json")
CURRENT_DIRTY=false
if [ -n "$(source_status)" ]; then
  CURRENT_DIRTY=true
fi
if [ "$MANIFEST_COMMIT" != "$COMMIT" ]; then
  echo "binary manifest commit $MANIFEST_COMMIT does not match checkout $COMMIT" >&2
  exit 2
fi
if [ "$MODE" = qualification ] && [ "$MANIFEST_DIRTY" != false ]; then
  echo "qualification requires a clean bundle manifest" >&2
  exit 2
fi
if [ "$MODE" = qualification ] && [ "$CURRENT_DIRTY" != false ]; then
  echo "qualification requires a clean checkout so harness and analyzer are immutable" >&2
  exit 2
fi
verify_immutable_inputs() {
  [ "$(sha256_file "$BINDIR/manifest.json")" = "$MANIFEST_SHA" ] || {
    echo "build manifest changed during qualification" >&2
    return 2
  }
  [ "$(git -C "$ROOT" rev-parse HEAD)" = "$COMMIT" ] || {
    echo "checkout commit changed during qualification" >&2
    return 2
  }
  if [ "$MODE" = qualification ] && [ -n "$(source_status)" ]; then
    echo "checkout changed during qualification" >&2
    return 2
  fi
  local binary expected actual asset source_path
  for binary in beamd perfclient perfserver directclient directserver; do
    expected=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["binaries"][sys.argv[2]])' "$BINDIR/manifest.json" "$binary")
    actual=$(sha256_file "$BINDIR/$binary")
    [ "$expected" = "$actual" ] || {
      echo "$binary hash does not match the build manifest" >&2
      return 2
    }
  done
  for asset in b4_analyze.py test/perf/b4_analyze.py scripts/perf-netem.sh; do
    case "$asset" in
      b4_analyze.py) source_path="$BINDIR/b4_analyze.py" ;;
      *) source_path="$ROOT/$asset" ;;
    esac
    expected=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["assets"][sys.argv[2]])' "$BINDIR/manifest.json" "$asset")
    actual=$(sha256_file "$source_path")
    [ "$expected" = "$actual" ] || {
      echo "$asset hash does not match the build manifest" >&2
      return 2
    }
  done
}
verify_immutable_inputs

RUN_STAMP=$(date -u +%Y%m%dT%H%M%SZ)
OUTDIR=${OUTDIR:-"$ROOT/test/perf/results/b4-$RUN_STAMP"}
if [ -e "$OUTDIR" ]; then
  echo "OUTDIR already exists; evidence is never overwritten: $OUTDIR" >&2
  exit 2
fi
mkdir -p "$OUTDIR/logs" "$OUTDIR/qdisc" "$OUTDIR/effective-config"
cp "$BINDIR/b4_analyze.py" "$OUTDIR/b4_analyze.py"
cp "$ROOT/scripts/perf-netem.sh" "$OUTDIR/perf-netem.sh"
WORK=$(mktemp -d)
SUFFIX=$(printf '%05d' "$$")
EDGE_NS="bp-e-$SUFFIX"
AGENT_NS="bp-a-$SUFFIX"
EDGE_IF="bpe$SUFFIX"
AGENT_IF="bpa$SUFFIX"
EDGE_IP=10.231.0.1
AGENT_IP=10.231.0.2
EDGE_ADDR="$EDGE_IP:443"
DIRECT_TCP_ADDR="$EDGE_IP:9443"
DIRECT_QUIC_ADDR="$EDGE_IP:9443"
BASE=perf.local
SLUG=perf
NAME=blob
HOST="$NAME-$SLUG.$BASE"
TOKEN=PERFTOKEN
PIDS=()
AGENT_PID=
BULK_PID=

cleanup() {
  set +e
  if [ -n "$BULK_PID" ]; then kill "$BULK_PID" 2>/dev/null; fi
  if [ -n "$AGENT_PID" ]; then kill "$AGENT_PID" 2>/dev/null; fi
  if [ "${#PIDS[@]}" -gt 0 ]; then kill "${PIDS[@]}" 2>/dev/null; fi
  wait 2>/dev/null
  case "$EDGE_NS" in bp-e-*) ip netns delete "$EDGE_NS" 2>/dev/null ;; esac
  case "$AGENT_NS" in bp-a-*) ip netns delete "$AGENT_NS" 2>/dev/null ;; esac
  case "$WORK" in /tmp/*|/private/tmp/*) rm -rf "$WORK" ;; esac
}
trap cleanup EXIT INT TERM

ip netns add "$EDGE_NS"
ip netns add "$AGENT_NS"
ip link add "$EDGE_IF" type veth peer name "$AGENT_IF"
ip link set "$EDGE_IF" netns "$EDGE_NS"
ip link set "$AGENT_IF" netns "$AGENT_NS"
ip -n "$EDGE_NS" addr add "$EDGE_IP/24" dev "$EDGE_IF"
ip -n "$AGENT_NS" addr add "$AGENT_IP/24" dev "$AGENT_IF"
ip -n "$EDGE_NS" link set lo up
ip -n "$AGENT_NS" link set lo up
ip -n "$EDGE_NS" link set "$EDGE_IF" up
ip -n "$AGENT_NS" link set "$AGENT_IF" up

# Freeze offloads explicitly. Qualification fails if the host cannot apply the
# requested state; silently comparing different offload behavior is invalid.
ip netns exec "$EDGE_NS" ethtool -K "$EDGE_IF" gro off gso off tso off >/dev/null
ip netns exec "$AGENT_NS" ethtool -K "$AGENT_IF" gro off gso off tso off >/dev/null
{
  echo "## edge/$EDGE_IF"
  ip netns exec "$EDGE_NS" ethtool -k "$EDGE_IF"
  echo "## agent/$AGENT_IF"
  ip netns exec "$AGENT_NS" ethtool -k "$AGENT_IF"
} > "$OUTDIR/interface-offload.txt"

openssl req -x509 -newkey rsa:2048 -nodes -days 2 \
  -subj "/CN=direct.perf.local" \
  -addext "subjectAltName=DNS:direct.perf.local,IP:$EDGE_IP" \
  -keyout "$WORK/direct.key" -out "$WORK/direct.crt" >/dev/null 2>&1
CERT_SHA=$(openssl x509 -in "$WORK/direct.crt" -noout -fingerprint -sha256 |
  awk -F= '{gsub(":", "", $2); print tolower($2)}')
cp "$WORK/direct.crt" "$OUTDIR/direct-fixture.crt"

cat > "$WORK/tokens.json" <<EOF
{"$TOKEN":"$SLUG"}
EOF
cat > "$WORK/edge.yaml" <<EOF
base_domain: $BASE
url_shape: hyphen
listen_https: "$EDGE_ADDR"
listen_quic: "$EDGE_ADDR"
disable_quic: false
acme_email: perf@example.com
acme_ca: "off"
dns_provider: stub
token_store: "file:$WORK/tokens.json"
data_dir: $WORK/edge-data
max_streams_per_session: 64
max_streams_total: 128
max_pre_auth_sessions: 32
max_sessions_total: 8
max_request_body_bytes: 134217728
request_log:
  enabled: false
EOF
cp "$WORK/edge.yaml" "$OUTDIR/effective-config/edge.yaml"

GOMEMLIMIT=${GOMEMLIMIT:-1400MiB}
ip netns exec "$EDGE_NS" env GOMEMLIMIT="$GOMEMLIMIT" BEAMD_DISABLE_QUIC=false \
  BEAMD_YAMUX_STREAM_WINDOW_BYTES=4194304 \
  "$BINDIR/beamd" serve --config "$WORK/edge.yaml" \
  >"$OUTDIR/logs/edge.log" 2>&1 &
PIDS+=("$!")
ip netns exec "$AGENT_NS" "$BINDIR/perfserver" --addr 127.0.0.1:9000 \
  >"$OUTDIR/logs/perfserver.log" 2>&1 &
PIDS+=("$!")
ip netns exec "$EDGE_NS" "$BINDIR/directserver" --transport tcp \
  --addr "$DIRECT_TCP_ADDR" --cert "$WORK/direct.crt" --key "$WORK/direct.key" \
  >"$OUTDIR/logs/direct-tcp.log" 2>&1 &
PIDS+=("$!")
ip netns exec "$EDGE_NS" "$BINDIR/directserver" --transport quic \
  --addr "$DIRECT_QUIC_ADDR" --cert "$WORK/direct.crt" --key "$WORK/direct.key" \
  >"$OUTDIR/logs/direct-quic.log" 2>&1 &
PIDS+=("$!")

ready=
for _ in $(seq 1 80); do
  if ip netns exec "$EDGE_NS" curl -sk --max-time 1 "https://$EDGE_IP/healthz" >/dev/null 2>&1; then
    ready=1
    break
  fi
  sleep 0.25
done
if [ -z "$ready" ]; then
  echo "edge did not become ready; see $OUTDIR/logs/edge.log" >&2
  exit 2
fi

if [ "$MODE" = qualification ]; then
  PROFILES=(clean lossy high-rtt-clean high-rtt-lossy)
  read -r -a SEEDS <<< "${NETEM_SEEDS:-101 202 303}"
  [ "${#SEEDS[@]}" -ge 3 ] || { echo "qualification needs at least three seeds" >&2; exit 2; }
  PROTOCOL_SIZES=(36 259072 263168 1048576 16777216 104857600)
  MIX_N=50
  MIX_WARMUP=8
  PROTOCOL_WARMUP=5
  PROTOCOL_MULTI_SIZE=16777216
  BULK_SIZE=8388608
  BULK_N=100000
  BULK_RAMP=5
else
  PROFILES=(clean lossy)
  SEEDS=(101)
  PROTOCOL_SIZES=(36 259072 1048576)
  MIX_N=2
  MIX_WARMUP=1
  PROTOCOL_WARMUP=1
  PROTOCOL_MULTI_SIZE=4096
  BULK_SIZE=4096
  BULK_N=1000
  BULK_RAMP=1
fi
for seed in "${SEEDS[@]}"; do
  [[ "$seed" =~ ^[1-9][0-9]*$ ]] || { echo "invalid positive integer seed: $seed" >&2; exit 2; }
  [ "$seed" -le 4000000000 ] || { echo "seed is too large for tc netem: $seed" >&2; exit 2; }
done
if [ "$(printf '%s\n' "${SEEDS[@]}" | sort -u | wc -l | tr -d ' ')" -ne "${#SEEDS[@]}" ]; then
  echo "NETEM_SEEDS must be distinct" >&2
  exit 2
fi

profile_values() {
  case "$1" in
    clean) echo "75 0 100" ;;
    lossy) echo "75 1 100" ;;
    high-rtt-clean) echo "250 0 20" ;;
    high-rtt-lossy) echo "250 1 20" ;;
    *) return 2 ;;
  esac
}

apply_netem() {
  local profile=$1 seed=$2 delay loss rate
  read -r delay loss rate <<< "$(profile_values "$profile")"
  local edge_seed=$seed
  local agent_seed=$((seed + 1000003))
  if [ "$loss" = 0 ]; then
    ip netns exec "$EDGE_NS" tc qdisc replace dev "$EDGE_IF" root \
      netem limit 1000 seed "$edge_seed" delay "${delay}ms" rate "${rate}mbit"
    ip netns exec "$AGENT_NS" tc qdisc replace dev "$AGENT_IF" root \
      netem limit 1000 seed "$agent_seed" delay "${delay}ms" rate "${rate}mbit"
  else
    ip netns exec "$EDGE_NS" tc qdisc replace dev "$EDGE_IF" root \
      netem limit 1000 seed "$edge_seed" delay "${delay}ms" \
      loss random "${loss}%" rate "${rate}mbit"
    ip netns exec "$AGENT_NS" tc qdisc replace dev "$AGENT_IF" root \
      netem limit 1000 seed "$agent_seed" delay "${delay}ms" \
      loss random "${loss}%" rate "${rate}mbit"
  fi
  {
    echo "profile=$profile seed=$seed"
    echo "edge-to-agent:"
    ip netns exec "$EDGE_NS" tc -s qdisc show dev "$EDGE_IF"
    echo "agent-to-edge:"
    ip netns exec "$AGENT_NS" tc -s qdisc show dev "$AGENT_IF"
  } > "$OUTDIR/qdisc/$profile-$seed.txt"
}

record_qdisc_snapshot() {
  local profile=$1 seed=$2 direction=$3 transport=$4
  {
    echo
    echo "after direction=$direction transport=$transport"
    echo "edge-to-agent:"
    ip netns exec "$EDGE_NS" tc -s qdisc show dev "$EDGE_IF"
    echo "agent-to-edge:"
    ip netns exec "$AGENT_NS" tc -s qdisc show dev "$AGENT_IF"
  } >> "$OUTDIR/qdisc/$profile-$seed.txt"
}

tag_record() {
  local fixture=$1 workload=$2 seed=$3 order=$4 order_index=$5 warmups=$6 condition=${7:-}
  python3 -c '
import json
import sys
record = json.load(sys.stdin)
record.update({
    "fixture": sys.argv[1],
    "workload": sys.argv[2],
    "seed": int(sys.argv[3]),
    "order": sys.argv[4],
    "order_index": int(sys.argv[5]),
    "warmups": int(sys.argv[6]),
    "handshake_included": False,
})
transport = record.get("transport", "")
record["transport"] = transport.removeprefix("direct-")
if sys.argv[7]:
    record["condition"] = sys.argv[7]
samples = record.get("samples")
if record.get("errors") != 0 or record.get("corrupt") != 0:
    raise SystemExit(
        "measurement produced request errors or corruption: "
        + json.dumps(record, separators=(",", ":"))
    )
if not isinstance(samples, list) or any(
    not isinstance(sample, dict) or sample.get("ok") is not True or sample.get("err")
    for sample in samples
):
    raise SystemExit(
        "measurement produced an unsuccessful or missing raw sample: "
        + json.dumps(record, separators=(",", ":"))
    )
json.dump(record, sys.stdout, separators=(",", ":"))
sys.stdout.write("\n")
' "$fixture" "$workload" "$seed" "$order" "$order_index" "$warmups" "$condition"
}

iterations_for() {
  local profile=$1 size=$2
  if [ "$MODE" = smoke ]; then echo 2; return; fi
  if [ "$profile" = high-rtt-lossy ] && [ "$size" -eq 36 ]; then echo 100; return; fi
  if [ "$size" -le 1048576 ]; then echo 50
  elif [ "$size" -eq 16777216 ]; then echo 20
  else echo 5
  fi
}

direct_addr() {
  if [ "$1" = tcp ]; then echo "$DIRECT_TCP_ADDR"; else echo "$DIRECT_QUIC_ADDR"; fi
}

run_direct_matrix() {
  local profile=$1 seed=$2 direction=$3 transport=$4 order=$5 order_index=$6
  local size n
  for size in "${PROTOCOL_SIZES[@]}"; do
    n=$(iterations_for "$profile" "$size")
    ip netns exec "$AGENT_NS" "$BINDIR/directclient" \
      --transport "$transport" --addr "$(direct_addr "$transport")" \
      --server-name direct.perf.local --ca "$WORK/direct.crt" --insecure \
      --dir "$direction" --size "$size" --n "$n" --warmup "$PROTOCOL_WARMUP" \
      --profile "$profile" --timeout 20m |
      tag_record direct protocol "$seed" "$order" "$order_index" "$PROTOCOL_WARMUP" \
      >> "$OUTDIR/raw-direct.jsonl"
  done
}

write_client_config() {
  local transport=$1
  cat > "$WORK/client-$transport.yaml" <<EOF
server: $EDGE_ADDR
token: $TOKEN
insecure_skip_verify: true
transport: $transport
agent_socket: $WORK/agent-$transport.sock
EOF
  cp "$WORK/client-$transport.yaml" "$OUTDIR/effective-config/client-$transport.yaml"
}

start_agent() {
  local profile=$1 seed=$2 direction=$3 transport=$4
  write_client_config "$transport"
  local check_path="$OUTDIR/check-$profile-$seed-$direction-$transport.json"
  if ! ip netns exec "$AGENT_NS" env BEAMD_TRANSPORT="$transport" \
    BEAMD_YAMUX_STREAM_WINDOW_BYTES=4194304 \
    "$BINDIR/beamd" check --json --transport "$transport" \
    --config "$WORK/client-$transport.yaml" > "$check_path"; then
    echo "forced-$transport preflight failed: $check_path" >&2
    exit 2
  fi
  python3 - "$check_path" "$transport" <<'PY'
import json
import sys
record = json.load(open(sys.argv[1]))
if record.get("ok") is not True or record.get("transport") != sys.argv[2]:
    raise SystemExit(f"preflight did not select forced transport: {record}")
PY
  ip netns exec "$AGENT_NS" env BEAMD_TRANSPORT="$transport" \
    BEAMD_YAMUX_STREAM_WINDOW_BYTES=4194304 \
    "$BINDIR/beamd" open 9000 --as "$NAME" \
    --config "$WORK/client-$transport.yaml" \
    >"$OUTDIR/logs/agent-$profile-$seed-$direction-$transport.log" 2>&1 &
  AGENT_PID=$!

  local healthy=
  local health_path="$OUTDIR/health-$profile-$seed-$direction-$transport.json"
  for _ in $(seq 1 120); do
    if ip netns exec "$EDGE_NS" "$BINDIR/perfclient" \
      --url "https://$HOST:443" --resolve "$HOST:$EDGE_IP" --insecure \
      --size 36 --dir "$direction" --n 1 --warmup 0 --concurrency 1 \
      --profile "$profile" --transport "$transport" --timeout 3s --raw \
      >"$health_path" 2>/dev/null &&
      python3 -c '
import json
import sys
record = json.load(open(sys.argv[1]))
samples = record.get("samples")
ok = (
    record.get("errors") == 0
    and record.get("corrupt") == 0
    and isinstance(samples, list)
    and len(samples) == 1
    and isinstance(samples[0], dict)
    and samples[0].get("ok") is True
    and not samples[0].get("err")
)
raise SystemExit(0 if ok else 1)
' "$health_path"; then
      healthy=1
      break
    fi
    sleep 0.5
  done
  if [ -z "$healthy" ]; then
    echo "forced-$transport tunnel did not become healthy" >&2
    exit 2
  fi
}

stop_agent() {
  set +e
  if [ -n "$BULK_PID" ]; then
    kill "$BULK_PID" 2>/dev/null
    wait "$BULK_PID" 2>/dev/null
    BULK_PID=
  fi
  if [ -n "$AGENT_PID" ]; then
    kill "$AGENT_PID" 2>/dev/null
    wait "$AGENT_PID" 2>/dev/null
    AGENT_PID=
  fi
  sleep 1
  set -e
}

run_beamd_protocol() {
  local profile=$1 seed=$2 direction=$3 transport=$4 order=$5 order_index=$6
  local size n
  for size in "${PROTOCOL_SIZES[@]}"; do
    n=$(iterations_for "$profile" "$size")
    ip netns exec "$EDGE_NS" "$BINDIR/perfclient" \
      --url "https://$HOST:443" --resolve "$HOST:$EDGE_IP" --insecure \
      --size "$size" --dir "$direction" --n "$n" --warmup "$PROTOCOL_WARMUP" \
      --concurrency 1 --profile "$profile" --transport "$transport" \
      --timeout 20m --raw |
      tag_record beamd protocol "$seed" "$order" "$order_index" "$PROTOCOL_WARMUP" \
      >> "$OUTDIR/raw-protocol.jsonl"
  done

  local eight_n=8
  local eight_warmup=8
  [ "$MODE" = smoke ] && eight_n=2
  [ "$MODE" = smoke ] && eight_warmup=2
  ip netns exec "$EDGE_NS" "$BINDIR/perfclient" \
    --url "https://$HOST:443" --resolve "$HOST:$EDGE_IP" --insecure \
    --size "$PROTOCOL_MULTI_SIZE" --dir "$direction" --n "$eight_n" \
    --warmup "$eight_warmup" --concurrency 8 --profile "$profile" \
    --transport "$transport" --timeout 20m --raw |
    tag_record beamd protocol "$seed" "$order" "$order_index" "$eight_warmup" \
    >> "$OUTDIR/raw-protocol.jsonl"
}

interactive_case() {
  local profile=$1 seed=$2 direction=$3 transport=$4 order=$5 order_index=$6
  local size=$7 condition=$8
  ip netns exec "$EDGE_NS" "$BINDIR/perfclient" \
    --url "https://$HOST:443" --resolve "$HOST:$EDGE_IP" --insecure \
    --size "$size" --dir "$direction" --n "$MIX_N" --warmup "$MIX_WARMUP" \
    --concurrency 1 --profile "$profile" --transport "$transport" \
    --timeout 10m --raw |
    tag_record beamd mixed "$seed" "$order" "$order_index" "$MIX_WARMUP" "$condition" \
    >> "$OUTDIR/raw-mixed.jsonl"
}

snapshot_bulk() {
  local stage=$1 profile=$2 seed=$3 direction=$4 transport=$5
  local order=$6 order_index=$7 progress_path=$8
  python3 - "$progress_path" "$OUTDIR/bulk-live.jsonl" \
    "$stage" "$profile" "$seed" "$direction" "$transport" "$order" "$order_index" <<'PY'
import json
import pathlib
import sys
import time

progress_path = pathlib.Path(sys.argv[1])
evidence_path = pathlib.Path(sys.argv[2])
try:
    record = json.loads(progress_path.read_text(encoding="utf-8"))
except (FileNotFoundError, json.JSONDecodeError) as err:
    raise SystemExit(f"bulk progress is missing or invalid: {progress_path}: {err}")

required = ("active", "started", "completed", "errors", "corrupt", "updated_unix_nano")
if not isinstance(record, dict) or any(
    isinstance(record.get(field), bool) or not isinstance(record.get(field), int)
    for field in required
):
    raise SystemExit(f"bulk progress has invalid counters: {record}")
now = time.time_ns()
age_ns = now - record["updated_unix_nano"]
if (
    record["active"] != 6
    or record["started"] < 6
    or record["completed"] < 0
    or record["completed"] > record["started"]
    or record["errors"] != 0
    or record["corrupt"] != 0
    or age_ns < 0
    or age_ns > 5_000_000_000
):
    raise SystemExit(f"six-stream bulk load is not live and error-free: {record}")
record.update(
    {
        "stage": sys.argv[3],
        "profile": sys.argv[4],
        "seed": int(sys.argv[5]),
        "dir": sys.argv[6],
        "transport": sys.argv[7],
        "order": sys.argv[8],
        "order_index": int(sys.argv[9]),
        "captured_unix_nano": now,
    }
)
with evidence_path.open("a", encoding="utf-8") as handle:
    json.dump(record, handle, separators=(",", ":"))
    handle.write("\n")
PY
}

run_mixed() {
  local profile=$1 seed=$2 direction=$3 transport=$4 order=$5 order_index=$6
  local bulk_progress="$OUTDIR/bulk-live-$profile-$seed-$direction-$transport.json"
  interactive_case "$profile" "$seed" "$direction" "$transport" "$order" "$order_index" 4096 baseline
  interactive_case "$profile" "$seed" "$direction" "$transport" "$order" "$order_index" 66560 baseline

  ip netns exec "$EDGE_NS" "$BINDIR/perfclient" \
    --url "https://$HOST:443" --resolve "$HOST:$EDGE_IP" --insecure \
    --size "$BULK_SIZE" --dir "$direction" --n "$BULK_N" --warmup 0 \
    --concurrency 6 --profile "$profile" --transport "$transport" \
    --timeout 20m --progress-file "$bulk_progress" >/dev/null 2>&1 &
  BULK_PID=$!
  for _ in $(seq 1 "$BULK_RAMP"); do
    sleep 1
    if ! kill -0 "$BULK_PID" 2>/dev/null; then
      echo "six-stream bulk load exited during ramp" >&2
      wait "$BULK_PID" 2>/dev/null || true
      BULK_PID=
      exit 2
    fi
  done
  snapshot_bulk ramp "$profile" "$seed" "$direction" "$transport" \
    "$order" "$order_index" "$bulk_progress"
  interactive_case "$profile" "$seed" "$direction" "$transport" "$order" "$order_index" 4096 underload
  if ! kill -0 "$BULK_PID" 2>/dev/null; then
    echo "six-stream bulk load exited during 4 KiB under-load measurement" >&2
    wait "$BULK_PID" 2>/dev/null || true
    BULK_PID=
    exit 2
  fi
  snapshot_bulk after-4k "$profile" "$seed" "$direction" "$transport" \
    "$order" "$order_index" "$bulk_progress"
  interactive_case "$profile" "$seed" "$direction" "$transport" "$order" "$order_index" 66560 underload
  if ! kill -0 "$BULK_PID" 2>/dev/null; then
    echo "six-stream bulk load exited during 65 KiB under-load measurement" >&2
    wait "$BULK_PID" 2>/dev/null || true
    BULK_PID=
    exit 2
  fi
  snapshot_bulk after-65k "$profile" "$seed" "$direction" "$transport" \
    "$order" "$order_index" "$bulk_progress"
  kill "$BULK_PID" 2>/dev/null || true
  wait "$BULK_PID" 2>/dev/null || true
  BULK_PID=
}

: > "$OUTDIR/raw-direct.jsonl"
: > "$OUTDIR/raw-protocol.jsonl"
: > "$OUTDIR/raw-mixed.jsonl"
: > "$OUTDIR/bulk-live.jsonl"

for profile in "${PROFILES[@]}"; do
  for seed_index in "${!SEEDS[@]}"; do
    seed=${SEEDS[$seed_index]}
    apply_netem "$profile" "$seed"
    if [ $((seed_index % 2)) -eq 0 ]; then
      order=quic,tcp
      ORDERED_TRANSPORTS=(quic tcp)
    else
      order=tcp,quic
      ORDERED_TRANSPORTS=(tcp quic)
    fi
    for direction in download upload; do
      for order_zero in "${!ORDERED_TRANSPORTS[@]}"; do
        transport=${ORDERED_TRANSPORTS[$order_zero]}
        order_index=$((order_zero + 1))
        case "$profile" in
          clean) profile_code=c ;;
          lossy) profile_code=l ;;
          high-rtt-clean) profile_code=hc ;;
          high-rtt-lossy) profile_code=hl ;;
        esac
        NAME="b-$profile_code-$seed-${direction:0:1}-${transport:0:1}"
        HOST="$NAME-$SLUG.$BASE"
        echo ">>> $profile seed=$seed direction=$direction transport=$transport order=$order"
        run_direct_matrix "$profile" "$seed" "$direction" "$transport" "$order" "$order_index"
        start_agent "$profile" "$seed" "$direction" "$transport"
        run_beamd_protocol "$profile" "$seed" "$direction" "$transport" "$order" "$order_index"
        run_mixed "$profile" "$seed" "$direction" "$transport" "$order" "$order_index"
        record_qdisc_snapshot "$profile" "$seed" "$direction" "$transport"
        stop_agent
      done
    done
  done
done

{
  echo "## edge/$EDGE_IF"
  ip netns exec "$EDGE_NS" tc -s qdisc show dev "$EDGE_IF"
  echo "## agent/$AGENT_IF"
  ip netns exec "$AGENT_NS" tc -s qdisc show dev "$AGENT_IF"
} > "$OUTDIR/qdisc-final.txt"

# Re-verify the checkout, harness, analyzer, and every executable after the
# long collection phase. The final verdict never runs from mutable source.
verify_immutable_inputs
[ "$(sha256_file "$OUTDIR/b4_analyze.py")" = \
  "$(sha256_file "$BINDIR/b4_analyze.py")" ] || {
  echo "recorded analyzer changed during qualification" >&2
  exit 2
}
[ "$(sha256_file "$OUTDIR/perf-netem.sh")" = \
  "$(sha256_file "$ROOT/scripts/perf-netem.sh")" ] || {
  echo "recorded harness changed during qualification" >&2
  exit 2
}

GO_VERSION=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["go_version"])' "$BINDIR/manifest.json")
QUIC_VERSION=$(awk '$1 == "github.com/quic-go/quic-go" {print $2}' "$ROOT/go.mod")
YAMUX_VERSION=$(awk '$1 == "github.com/hashicorp/yamux" {print $2}' "$ROOT/go.mod")
CPU=$(
  awk -F: '
    /^(model name|Processor|Hardware)[[:space:]]*:/ {
      sub(/^[[:space:]]+/, "", $2)
      if (length($2) > 0) {
        print $2
        exit
      }
    }
  ' /proc/cpuinfo
)
if [ -z "$CPU" ] && command -v lscpu >/dev/null 2>&1; then
  CPU=$(
    lscpu | awk -F: '
      /^Model name[[:space:]]*:/ {
        sub(/^[[:space:]]+/, "", $2)
        if (length($2) > 0 && $2 != "-") {
          print $2
          exit
        }
      }
    '
  )
fi
[ -n "$CPU" ] || CPU="$(uname -m) (model unavailable)"
RAM_BYTES=$(awk '/MemTotal/ {print $2 * 1024}' /proc/meminfo)
RESOURCE_LIMITS=$(ulimit -a | tr '\n' ';')
CONTAINER_KIND=not-detected
if [ -f /.dockerenv ]; then
  CONTAINER_KIND=docker
elif [ -f /run/.containerenv ]; then
  CONTAINER_KIND=podman
elif [ -r /proc/1/cgroup ] &&
  grep -Eiq '(docker|containerd|kubepods|podman|lxc)' /proc/1/cgroup; then
  CONTAINER_KIND=cgroup-marker
fi
CONTAINER_LIMITS=$(
  printf \
    'container=%s; cgroup_v2.cpu.max=%s; cgroup_v2.memory.max=%s; cgroup_v2.memory.swap.max=%s; cgroup_v2.cpuset.cpus.effective=%s' \
    "$CONTAINER_KIND" \
    "$(read_limit_file /sys/fs/cgroup/cpu.max)" \
    "$(read_limit_file /sys/fs/cgroup/memory.max)" \
    "$(read_limit_file /sys/fs/cgroup/memory.swap.max)" \
    "$(read_limit_file /sys/fs/cgroup/cpuset.cpus.effective)"
)
QDISC_ARTIFACTS=$(
  find "$OUTDIR/qdisc" -maxdepth 1 -type f -exec basename {} \; |
    sort | paste -sd, -
)
SEEDS_CSV=$(IFS=,; echo "${SEEDS[*]}")
QUALIFICATION=False
[ "$MODE" = qualification ] && QUALIFICATION=True
GIT_DIRTY=False
[ "$MANIFEST_DIRTY" = true ] && GIT_DIRTY=True
python3 - "$OUTDIR/metadata.json" <<PY
import json
import pathlib
import platform

path = pathlib.Path(r"""$OUTDIR/metadata.json""")
metadata = {
    "schema_version": 1,
    "qualification": $QUALIFICATION,
    "started_utc": "$RUN_STAMP",
    "completed_utc": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
    "beamd_commit": "$COMMIT",
    "git_dirty": $GIT_DIRTY,
    "binary_manifest": json.load(open(r"""$BINDIR/manifest.json""")),
    "recorded_analyzer": "b4_analyze.py",
    "recorded_harness": "perf-netem.sh",
    "go_version": r"""$GO_VERSION""",
    "quic_go_version": "$QUIC_VERSION",
    "yamux_version": "$YAMUX_VERSION",
    "kernel": platform.release(),
    "os": platform.platform(),
    "cpu": r"""$CPU""",
    "ram_bytes": int("$RAM_BYTES"),
    "resource_limits": r"""$RESOURCE_LIMITS""",
    "container_limits": r"""$CONTAINER_LIMITS""",
    "interface_offload": "interface-offload.txt",
    "effective_config": {
        "edge": "effective-config/edge.yaml",
        "tcp_client": "effective-config/client-tcp.yaml",
        "quic_client": "effective-config/client-quic.yaml",
        "gomemlimit": "$GOMEMLIMIT",
        "yamux_stream_window_bytes": 4194304,
    },
    "direct_fixture": {
        "alpn": "beamd-perf-direct/1",
        "tls_version": "TLS 1.3",
        "certificate_sha256": "$CERT_SHA",
        "certificate": "direct-fixture.crt",
        "trust": "certificate validation disabled for direct and beamd measurement clients",
        "long_lived_connection": True,
        "handshake_recorded_separately": True,
        "connection_roles": "agent namespace client dials edge namespace server",
        "control_stream_initiator": "agent",
        "data_stream_initiator": "edge",
        "direction_mapping": {
            "download": "agent client uploads to edge server",
            "upload": "agent client downloads from edge server",
        },
        "quic_flow_control": {
            "initial_stream": 4194304,
            "max_stream": 16777216,
            "initial_connection": 16777216,
            "max_connection": 67108864,
            "server_max_incoming_streams": 1,
            "client_max_incoming_streams": 64,
            "keepalive_period_ms": 0,
        },
    },
    "workload": {
        "bulk_streams": 6,
        "bulk_bytes": int("$BULK_SIZE"),
        "ramp_seconds": int("$BULK_RAMP"),
        "interactive_bytes": [4096, 66560],
        "interactive_warmups": int("$MIX_WARMUP"),
        "interactive_samples": int("$MIX_N"),
    },
    "topology": "edge namespace/public client <-> shaped veth <-> agent namespace/backend",
    "public_leg_shaped": False,
    "directions_shaped": ["edge-to-agent", "agent-to-edge"],
    "netem_queue_limit_packets": 1000,
    "netem_profiles": {
        "clean": {"one_way_delay_ms": 75, "loss_percent": 0, "rate_mbit": 100},
        "lossy": {"one_way_delay_ms": 75, "loss_percent": 1, "rate_mbit": 100},
        "high-rtt-clean": {"one_way_delay_ms": 250, "loss_percent": 0, "rate_mbit": 20},
        "high-rtt-lossy": {"one_way_delay_ms": 250, "loss_percent": 1, "rate_mbit": 20},
    },
    "netem_seeds": [int(value) for value in "$SEEDS_CSV".split(",")],
    "seed_orders": {
        str(seed): ("quic,tcp" if index % 2 == 0 else "tcp,quic")
        for index, seed in enumerate(
            [int(value) for value in "$SEEDS_CSV".split(",")]
        )
    },
    "transport_orders": ["quic,tcp", "tcp,quic"] if $QUALIFICATION else ["quic,tcp"],
    "qdisc_artifacts": "$QDISC_ARTIFACTS".split(","),
    "handshake_included": False,
}
path.write_text(json.dumps(metadata, indent=2) + "\n")
PY

if [ "$MODE" = smoke ]; then
  verify_immutable_inputs
  echo "SMOKE COMPLETE — non-qualification evidence in $OUTDIR"
  exit 0
fi

verify_immutable_inputs
python3 "$BINDIR/b4_analyze.py" "$OUTDIR" --summary "$OUTDIR/summary.json" |
  tee "$OUTDIR/analysis.txt"
verify_immutable_inputs
[ "$(sha256_file "$OUTDIR/b4_analyze.py")" = \
  "$(sha256_file "$BINDIR/b4_analyze.py")" ] || {
  echo "recorded analyzer changed while producing the verdict" >&2
  exit 2
}
[ "$(sha256_file "$OUTDIR/perf-netem.sh")" = \
  "$(sha256_file "$ROOT/scripts/perf-netem.sh")" ] || {
  echo "recorded harness changed while producing the verdict" >&2
  exit 2
}
echo "QUALIFICATION COMPLETE — evidence in $OUTDIR"
