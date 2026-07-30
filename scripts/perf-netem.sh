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
# available for harness development, and a targeted mode rechecks one exact
# protocol case without claiming a B4 verdict. Neither can pass the B4 analyzer.
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
ACTION=${1:-}
BINDIR=${BINDIR:-"$ROOT/bin/perf-b4"}
MODE=${MODE:-qualification}
IMAGE_TAG=${IMAGE_TAG:-local}
MIN_QUALIFICATION_CPUS=2
MIN_QUALIFICATION_RAM_BYTES=2000000000

usage() {
  cat <<'EOF'
Usage:
  scripts/perf-netem.sh build
  sudo scripts/perf-netem.sh run
  scripts/perf-netem.sh analyze RESULTS_DIR

Environment:
  BINDIR=/path           binary bundle (default: bin/perf-b4)
  OUTDIR=/path           new evidence directory (run only)
  MODE=qualification     full, fail-closed run (default)
  MODE=smoke             reduced harness check; never valid B4 evidence
  MODE=targeted          one protocol case over direct + beamd TCP/QUIC
  TC_BIN=/absolute/path  tc with deterministic netem seed support
  NETEM_SEEDS="101 202 303"
  TARGET_PROFILE=lossy               (targeted only)
  TARGET_SEED=101                     (targeted only)
  TARGET_DIRECTION=download          (targeted only)
  TARGET_SIZE_BYTES=104857600         (targeted only)
  TARGET_TRANSPORTS="tcp quic"        (targeted only)
  TARGET_BEAMD_CONCURRENCY=1          (targeted only; 1..64)
  TARGET_WARMUPS=5                    (targeted only)
  TARGET_ITERATIONS=                  (targeted only; matrix default when empty)
  GOMEMLIMIT=1400MiB

Qualification prerequisites: Linux, root for `run`, iproute2 (ip/tc) with
deterministic `netem seed` support, ethtool, openssl, curl, Python 3.10+, at
least 2 online CPUs and 2 GB-class usable RAM, and a clean checkout for
`build`. Fixture beamd processes use a harness-created empty HOME rather than
the operator or service account HOME.
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
if [ "$MODE" != qualification ] && [ "$MODE" != smoke ] && [ "$MODE" != targeted ]; then
  echo "MODE must be qualification, targeted, or smoke" >&2
  exit 2
fi
if [ "$(uname -s)" != Linux ]; then
  echo "run requires Linux network namespaces and netem" >&2
  exit 2
fi
if [ "$(id -u)" -ne 0 ]; then
  echo "run requires root; build the bundle first, then use sudo" >&2
  exit 2
fi
for command in ip ethtool openssl curl python3 git nproc; do
  command -v "$command" >/dev/null 2>&1 || {
    echo "missing required command: $command" >&2
    exit 2
  }
done
python3 -c 'import sys; sys.exit(0 if sys.version_info >= (3, 10) else 2)' || {
  echo "Python 3.10 or newer is required" >&2
  exit 2
}
TC_SOURCE_BIN=${TC_BIN:-}
if [ -z "$TC_SOURCE_BIN" ]; then
  TC_SOURCE_BIN=$(command -v tc 2>/dev/null || true)
fi
case "$TC_SOURCE_BIN" in
  /*) ;;
  *)
    echo "TC_BIN must resolve to an absolute tc executable; found: ${TC_SOURCE_BIN:-missing}" >&2
    exit 2
    ;;
esac
[ -f "$TC_SOURCE_BIN" ] && [ -x "$TC_SOURCE_BIN" ] || {
  echo "TC_BIN is not a regular executable: $TC_SOURCE_BIN" >&2
  exit 2
}
TC_SOURCE_SHA256=$(sha256_file "$TC_SOURCE_BIN")
TC_SOURCE_VERSION=$("$TC_SOURCE_BIN" -V 2>&1)
TC_SOURCE_NETEM_HELP=$(
  "$TC_SOURCE_BIN" qdisc add dev lo root netem help 2>&1 || true
)
if ! grep -Eq '(^|[[:space:]])seed([[:space:]]|$)' <<<"$TC_SOURCE_NETEM_HELP"; then
  echo "$TC_SOURCE_BIN does not support deterministic 'tc netem seed'; use iproute2 6.6+ or an equivalent backport" >&2
  exit 2
fi
[ "$(sha256_file "$TC_SOURCE_BIN")" = "$TC_SOURCE_SHA256" ] || {
  echo "source tc binary changed during capability preflight: $TC_SOURCE_BIN" >&2
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
if [ "$MODE" != smoke ] && [ "$MANIFEST_DIRTY" != false ]; then
  echo "$MODE evidence requires a clean bundle manifest" >&2
  exit 2
fi
if [ "$MODE" != smoke ] && [ "$CURRENT_DIRTY" != false ]; then
  echo "$MODE evidence requires a clean checkout so harness and analyzer are immutable" >&2
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
  if [ "$MODE" != smoke ] && [ -n "$(source_status)" ]; then
    echo "checkout changed during $MODE evidence collection" >&2
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

CPU_COUNT=$(nproc)
RAM_BYTES=$(
  awk '
    /MemTotal/ {
      printf "%.0f\n", $2 * 1024
      found = 1
      exit
    }
    END {
      if (!found) {
        exit 1
      }
    }
  ' /proc/meminfo
)
[[ "$CPU_COUNT" =~ ^[1-9][0-9]*$ ]] || {
  echo "could not determine a positive online CPU count: $CPU_COUNT" >&2
  exit 2
}
[[ "$RAM_BYTES" =~ ^[1-9][0-9]*$ ]] || {
  echo "could not determine positive Linux MemTotal bytes: $RAM_BYTES" >&2
  exit 2
}
if [ "$MODE" != smoke ]; then
  [ "$CPU_COUNT" -ge "$MIN_QUALIFICATION_CPUS" ] || {
    echo "qualification requires at least $MIN_QUALIFICATION_CPUS online CPUs; found $CPU_COUNT" >&2
    exit 2
  }
  [ "$RAM_BYTES" -ge "$MIN_QUALIFICATION_RAM_BYTES" ] || {
    echo "qualification requires Linux MemTotal >= $MIN_QUALIFICATION_RAM_BYTES bytes; found $RAM_BYTES" >&2
    exit 2
  }
fi

RUN_STAMP=$(date -u +%Y%m%dT%H%M%SZ)
OUTDIR=${OUTDIR:-"$ROOT/test/perf/results/b4-$RUN_STAMP"}
ROOT_CANONICAL=$(python3 -c 'import os,sys; print(os.path.realpath(sys.argv[1]))' "$ROOT")
OUTDIR=$(python3 -c 'import os,sys; print(os.path.realpath(sys.argv[1]))' "$OUTDIR")
SAFE_RESULTS_ROOT="$ROOT_CANONICAL/test/perf/results"
case "$OUTDIR" in
  "$ROOT_CANONICAL"|"$ROOT_CANONICAL"/*)
    case "$OUTDIR" in
      "$SAFE_RESULTS_ROOT"/b4-*) ;;
      *)
        echo "OUTDIR inside the checkout must be under test/perf/results/b4-*: $OUTDIR" >&2
        exit 2
        ;;
    esac
    ;;
esac
if [ -e "$OUTDIR" ]; then
  echo "OUTDIR already exists; evidence is never overwritten: $OUTDIR" >&2
  exit 2
fi
mkdir -p \
  "$OUTDIR/logs" \
  "$OUTDIR/qdisc" \
  "$OUTDIR/effective-config" \
  "$OUTDIR/traffic-control"
cp "$BINDIR/b4_analyze.py" "$OUTDIR/b4_analyze.py"
cp "$ROOT/scripts/perf-netem.sh" "$OUTDIR/perf-netem.sh"
if [ "$MODE" = targeted ]; then
  cp "$BINDIR/manifest.json" "$OUTDIR/manifest.json"
fi
TC_BIN=$(cd "$OUTDIR/traffic-control" && pwd -P)/tc
cp "$TC_SOURCE_BIN" "$TC_BIN"
TC_SHA256=$(sha256_file "$TC_BIN")
[ "$TC_SHA256" = "$TC_SOURCE_SHA256" ] || {
  echo "recorded tc binary hash does not match the runtime binary" >&2
  exit 2
}
TC_VERSION=$("$TC_BIN" -V 2>&1)
[ "$TC_VERSION" = "$TC_SOURCE_VERSION" ] || {
  echo "recorded tc version does not match the source binary" >&2
  exit 2
}
TC_NETEM_HELP=$("$TC_BIN" qdisc add dev lo root netem help 2>&1 || true)
if ! grep -Eq '(^|[[:space:]])seed([[:space:]]|$)' <<<"$TC_NETEM_HELP"; then
  echo "recorded tc binary lost deterministic netem seed support" >&2
  exit 2
fi
verify_traffic_control() {
  [ -f "$TC_BIN" ] && [ -x "$TC_BIN" ] || {
    echo "recorded tc binary is no longer a regular executable: $TC_BIN" >&2
    return 2
  }
  [ "$(sha256_file "$TC_BIN")" = "$TC_SHA256" ] || {
    echo "recorded tc binary changed during qualification: $TC_BIN" >&2
    return 2
  }
}
verify_traffic_control
WORK=$(mktemp -d)
BEAMD_HOME="$WORK/home"
mkdir -p "$BEAMD_HOME/.beamd"
chmod 0700 "$BEAMD_HOME" "$BEAMD_HOME/.beamd"
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
EDGE_PID=
PERFSERVER_PID=
DIRECT_TCP_PID=
DIRECT_QUIC_PID=

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
ip netns exec "$EDGE_NS" env HOME="$BEAMD_HOME" \
  GOMEMLIMIT="$GOMEMLIMIT" BEAMD_DISABLE_QUIC=false \
  BEAMD_YAMUX_STREAM_WINDOW_BYTES=4194304 \
  "$BINDIR/beamd" serve --config "$WORK/edge.yaml" \
  >"$OUTDIR/logs/edge.log" 2>&1 &
EDGE_PID=$!
PIDS+=("$EDGE_PID")
ip netns exec "$AGENT_NS" "$BINDIR/perfserver" --addr 127.0.0.1:9000 \
  >"$OUTDIR/logs/perfserver.log" 2>&1 &
PERFSERVER_PID=$!
PIDS+=("$PERFSERVER_PID")
ip netns exec "$EDGE_NS" "$BINDIR/directserver" --transport tcp \
  --addr "$DIRECT_TCP_ADDR" --cert "$WORK/direct.crt" --key "$WORK/direct.key" \
  >"$OUTDIR/logs/direct-tcp.log" 2>&1 &
DIRECT_TCP_PID=$!
PIDS+=("$DIRECT_TCP_PID")
ip netns exec "$EDGE_NS" "$BINDIR/directserver" --transport quic \
  --addr "$DIRECT_QUIC_ADDR" --cert "$WORK/direct.crt" --key "$WORK/direct.key" \
  >"$OUTDIR/logs/direct-quic.log" 2>&1 &
DIRECT_QUIC_PID=$!
PIDS+=("$DIRECT_QUIC_PID")

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

TARGET_BEAMD_CONCURRENCY_VALUE=1
TARGET_WARMUPS_VALUE=5
TARGET_ITERATIONS_VALUE=
if [ "$MODE" = qualification ]; then
  PROFILES=(clean lossy high-rtt-clean high-rtt-lossy)
  read -r -a SEEDS <<< "${NETEM_SEEDS:-101 202 303}"
  [ "${#SEEDS[@]}" -ge 3 ] || { echo "qualification needs at least three seeds" >&2; exit 2; }
  DIRECTIONS=(download upload)
  PROTOCOL_SIZES=(36 259072 263168 1048576 16777216 104857600)
  MIX_N=50
  MIX_WARMUP=8
  PROTOCOL_WARMUP=5
  PROTOCOL_MULTI_SIZE=16777216
  BULK_SIZE=8388608
  BULK_N=100000
  BULK_RAMP=5
elif [ "$MODE" = targeted ]; then
  PROFILES=("${TARGET_PROFILE:-lossy}")
  SEEDS=("${TARGET_SEED:-101}")
  DIRECTIONS=("${TARGET_DIRECTION:-download}")
  PROTOCOL_SIZES=("${TARGET_SIZE_BYTES:-104857600}")
  TARGET_BEAMD_CONCURRENCY_VALUE=${TARGET_BEAMD_CONCURRENCY:-1}
  TARGET_WARMUPS_VALUE=${TARGET_WARMUPS:-5}
  TARGET_ITERATIONS_VALUE=${TARGET_ITERATIONS:-}
  read -r -a TARGET_ORDERED_TRANSPORTS <<< "${TARGET_TRANSPORTS:-tcp quic}"
  [ "${#TARGET_ORDERED_TRANSPORTS[@]}" -eq 2 ] ||
    { echo "targeted mode requires exactly two TARGET_TRANSPORTS" >&2; exit 2; }
  [ "${TARGET_ORDERED_TRANSPORTS[0]}" != "${TARGET_ORDERED_TRANSPORTS[1]}" ] ||
    { echo "targeted transports must be distinct" >&2; exit 2; }
  for target_transport in "${TARGET_ORDERED_TRANSPORTS[@]}"; do
    case "$target_transport" in
      tcp|quic) ;;
      *) echo "invalid targeted transport: $target_transport" >&2; exit 2 ;;
    esac
  done
  case "${PROFILES[0]}" in
    clean|lossy|high-rtt-clean|high-rtt-lossy) ;;
    *) echo "invalid TARGET_PROFILE: ${PROFILES[0]}" >&2; exit 2 ;;
  esac
  case "${DIRECTIONS[0]}" in
    download|upload) ;;
    *) echo "invalid TARGET_DIRECTION: ${DIRECTIONS[0]}" >&2; exit 2 ;;
  esac
  [[ "${PROTOCOL_SIZES[0]}" =~ ^[1-9][0-9]*$ ]] ||
    { echo "TARGET_SIZE_BYTES must be a positive integer" >&2; exit 2; }
  [[ "$TARGET_BEAMD_CONCURRENCY_VALUE" =~ ^[1-9][0-9]*$ ]] &&
    [ "$TARGET_BEAMD_CONCURRENCY_VALUE" -le 64 ] ||
    { echo "TARGET_BEAMD_CONCURRENCY must be an integer from 1 through 64" >&2; exit 2; }
  [[ "$TARGET_WARMUPS_VALUE" =~ ^[0-9]+$ ]] ||
    { echo "TARGET_WARMUPS must be a non-negative integer" >&2; exit 2; }
  if [ -n "$TARGET_ITERATIONS_VALUE" ]; then
    [[ "$TARGET_ITERATIONS_VALUE" =~ ^[1-9][0-9]*$ ]] ||
      { echo "TARGET_ITERATIONS must be a positive integer when set" >&2; exit 2; }
  fi
  MIX_N=0
  MIX_WARMUP=0
  PROTOCOL_WARMUP=$TARGET_WARMUPS_VALUE
  PROTOCOL_MULTI_SIZE=0
  BULK_SIZE=0
  BULK_N=0
  BULK_RAMP=0
else
  PROFILES=(clean lossy)
  SEEDS=(101)
  DIRECTIONS=(download upload)
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
    ip netns exec "$EDGE_NS" "$TC_BIN" qdisc replace dev "$EDGE_IF" root \
      netem limit 1000 seed "$edge_seed" delay "${delay}ms" rate "${rate}mbit"
    ip netns exec "$AGENT_NS" "$TC_BIN" qdisc replace dev "$AGENT_IF" root \
      netem limit 1000 seed "$agent_seed" delay "${delay}ms" rate "${rate}mbit"
  else
    ip netns exec "$EDGE_NS" "$TC_BIN" qdisc replace dev "$EDGE_IF" root \
      netem limit 1000 seed "$edge_seed" delay "${delay}ms" \
      loss random "${loss}%" rate "${rate}mbit"
    ip netns exec "$AGENT_NS" "$TC_BIN" qdisc replace dev "$AGENT_IF" root \
      netem limit 1000 seed "$agent_seed" delay "${delay}ms" \
      loss random "${loss}%" rate "${rate}mbit"
  fi
  {
    echo "profile=$profile seed=$seed"
    echo "edge-to-agent:"
    ip netns exec "$EDGE_NS" "$TC_BIN" -s qdisc show dev "$EDGE_IF"
    echo "agent-to-edge:"
    ip netns exec "$AGENT_NS" "$TC_BIN" -s qdisc show dev "$AGENT_IF"
  } > "$OUTDIR/qdisc/$profile-$seed.txt"
}

record_qdisc_snapshot() {
  local profile=$1 seed=$2 direction=$3 transport=$4
  {
    echo
    echo "after direction=$direction transport=$transport"
    echo "edge-to-agent:"
    ip netns exec "$EDGE_NS" "$TC_BIN" -s qdisc show dev "$EDGE_IF" ||
      return $?
    echo "agent-to-edge:"
    ip netns exec "$AGENT_NS" "$TC_BIN" -s qdisc show dev "$AGENT_IF" ||
      return $?
  } >> "$OUTDIR/qdisc/$profile-$seed.txt"
}

tag_record() {
  local fixture=$1 workload=$2 seed=$3 order=$4 order_index=$5 warmups=$6 condition=${7:-}
  local failures_path=$OUTDIR/raw-failures.jsonl
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
    with open(sys.argv[8], "a", encoding="utf-8") as handle:
        json.dump(record, handle, separators=(",", ":"))
        handle.write("\n")
    raise SystemExit(
        "measurement produced request errors or corruption: "
        + json.dumps(record, separators=(",", ":"))
    )
if not isinstance(samples, list) or any(
    not isinstance(sample, dict) or sample.get("ok") is not True or sample.get("err")
    for sample in samples
):
    with open(sys.argv[8], "a", encoding="utf-8") as handle:
        json.dump(record, handle, separators=(",", ":"))
        handle.write("\n")
    raise SystemExit(
        "measurement produced an unsuccessful or missing raw sample: "
        + json.dumps(record, separators=(",", ":"))
    )
json.dump(record, sys.stdout, separators=(",", ":"))
sys.stdout.write("\n")
' "$fixture" "$workload" "$seed" "$order" "$order_index" "$warmups" "$condition" \
    "$failures_path"
}

record_targeted_stage() {
  local fixture=$1 transport=$2 outcome=$3 exit_status=$4 order_index=$5
  local detail=${6:-}
  python3 - "$OUTDIR/targeted-status.jsonl" "$fixture" "$transport" "$outcome" \
    "$exit_status" "$detail" "$profile" "$seed" "$direction" \
    "${PROTOCOL_SIZES[0]}" "$order" "$order_index" "$PROTOCOL_WARMUP" <<'PY'
import json
import sys

path = sys.argv[1]
record = {
    "schema_version": 1,
    "fixture": sys.argv[2],
    "workload": "protocol",
    "transport": sys.argv[3],
    "outcome": sys.argv[4],
    "exit_status": int(sys.argv[5]),
    "detail": sys.argv[6],
    "profile": sys.argv[7],
    "seed": int(sys.argv[8]),
    "dir": sys.argv[9],
    "size": int(sys.argv[10]),
    "order": sys.argv[11],
    "order_index": int(sys.argv[12]),
    "warmups": int(sys.argv[13]),
}
with open(path, "a", encoding="utf-8") as handle:
    json.dump(record, handle, separators=(",", ":"))
    handle.write("\n")
PY
}

capture_targeted_memory() {
  local fixture=$1 transport=$2 role=$3 pid=$4 order_index=$5
  python3 - "$OUTDIR/process-memory.jsonl" "$fixture" "$transport" "$role" \
    "$pid" "$profile" "$seed" "$direction" "${PROTOCOL_SIZES[0]}" \
    "$order" "$order_index" <<'PY'
import datetime
import hashlib
import json
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
pid_text = sys.argv[5]
record = {
    "schema_version": 1,
    "fixture": sys.argv[2],
    "transport": sys.argv[3],
    "role": sys.argv[4],
    "pid": int(pid_text) if pid_text.isdigit() else None,
    "profile": sys.argv[6],
    "seed": int(sys.argv[7]),
    "dir": sys.argv[8],
    "size": int(sys.argv[9]),
    "order": sys.argv[10],
    "order_index": int(sys.argv[11]),
    "captured_utc": datetime.datetime.now(datetime.timezone.utc).isoformat(),
    "available": False,
}
if not pid_text.isdigit():
    record["error"] = "pid unavailable"
else:
    proc = pathlib.Path("/proc") / pid_text
    try:
        fields = {}
        for line in (proc / "status").read_text(encoding="utf-8").splitlines():
            if ":" not in line:
                continue
            key, value = line.split(":", 1)
            fields[key] = value.strip()
        for field in ("VmRSS", "VmHWM"):
            parts = fields.get(field, "").split()
            if len(parts) != 2 or parts[1] != "kB":
                raise ValueError(f"{field} unavailable")
            record[field.lower() + "_bytes"] = int(parts[0]) * 1024
        record["comm"] = (proc / "comm").read_text(encoding="utf-8").strip()
        executable = (proc / "exe").resolve(strict=True)
        record["exe"] = str(executable)
        digest = hashlib.sha256()
        with executable.open("rb") as handle:
            for chunk in iter(lambda: handle.read(1024 * 1024), b""):
                digest.update(chunk)
        record["binary_sha256"] = digest.hexdigest()
        record["available"] = True
    except (OSError, ValueError) as error:
        record["error"] = str(error)
with path.open("a", encoding="utf-8") as handle:
    json.dump(record, handle, separators=(",", ":"))
    handle.write("\n")
PY
}

iterations_for() {
  local profile=$1 size=$2
  if [ "$MODE" = smoke ]; then echo 2; return; fi
  if [ "$MODE" = targeted ] && [ -n "$TARGET_ITERATIONS_VALUE" ]; then
    echo "$TARGET_ITERATIONS_VALUE"
    return
  fi
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
  local size n status
  local -a fail_fast_args=()
  if [ "$MODE" = targeted ]; then
    fail_fast_args=(--fail-fast)
  fi
  for size in "${PROTOCOL_SIZES[@]}"; do
    n=$(iterations_for "$profile" "$size")
    if ip netns exec "$AGENT_NS" "$BINDIR/directclient" \
        --transport "$transport" --addr "$(direct_addr "$transport")" \
        --server-name direct.perf.local --ca "$WORK/direct.crt" --insecure \
        --dir "$direction" --size "$size" --n "$n" --warmup "$PROTOCOL_WARMUP" \
        --profile "$profile" --timeout 20m "${fail_fast_args[@]}" |
        tag_record direct protocol "$seed" "$order" "$order_index" "$PROTOCOL_WARMUP" \
        >> "$OUTDIR/raw-direct.jsonl"; then
      :
    else
      status=$?
      return "$status"
    fi
  done
}

write_client_config() {
  local transport=$1
  if cat > "$WORK/client-$transport.yaml" <<EOF
server: $EDGE_ADDR
token: $TOKEN
insecure_skip_verify: true
transport: $transport
agent_socket: $WORK/agent-$transport.sock
EOF
  then
    :
  else
    return $?
  fi
  cp "$WORK/client-$transport.yaml" "$OUTDIR/effective-config/client-$transport.yaml" ||
    return $?
}

start_agent() {
  local profile=$1 seed=$2 direction=$3 transport=$4
  if ! write_client_config "$transport"; then
    echo "could not write forced-$transport client configuration" >&2
    return 2
  fi
  local check_path="$OUTDIR/check-$profile-$seed-$direction-$transport.json"
  if ! ip netns exec "$AGENT_NS" env HOME="$BEAMD_HOME" \
    BEAMD_TRANSPORT="$transport" \
    BEAMD_YAMUX_STREAM_WINDOW_BYTES=4194304 \
      "$BINDIR/beamd" check --json --transport "$transport" \
      --config "$WORK/client-$transport.yaml" > "$check_path"; then
    echo "forced-$transport preflight failed: $check_path" >&2
    return 2
  fi
  if ! python3 - "$check_path" "$transport" <<'PY'
import json
import sys
record = json.load(open(sys.argv[1]))
if record.get("ok") is not True or record.get("transport") != sys.argv[2]:
    raise SystemExit(f"preflight did not select forced transport: {record}")
PY
  then
    echo "forced-$transport preflight selected an unexpected transport" >&2
    return 2
  fi
  ip netns exec "$AGENT_NS" env HOME="$BEAMD_HOME" \
    BEAMD_TRANSPORT="$transport" \
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
    return 2
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
  local size n status concurrency=1
  local -a fail_fast_args=()
  if [ "$MODE" = targeted ]; then
    fail_fast_args=(--fail-fast)
    concurrency=$TARGET_BEAMD_CONCURRENCY_VALUE
  fi
  for size in "${PROTOCOL_SIZES[@]}"; do
    n=$(iterations_for "$profile" "$size")
    if ip netns exec "$EDGE_NS" "$BINDIR/perfclient" \
        --url "https://$HOST:443" --resolve "$HOST:$EDGE_IP" --insecure \
        --size "$size" --dir "$direction" --n "$n" --warmup "$PROTOCOL_WARMUP" \
        --concurrency "$concurrency" --profile "$profile" --transport "$transport" \
        --timeout 20m --raw "${fail_fast_args[@]}" |
        tag_record beamd protocol "$seed" "$order" "$order_index" "$PROTOCOL_WARMUP" \
        >> "$OUTDIR/raw-protocol.jsonl"; then
      :
    else
      status=$?
      return "$status"
    fi
  done

  if [ "$MODE" = targeted ]; then
    return 0
  fi

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
TARGETED_RUN_STATUS=0
if [ "$MODE" = targeted ]; then
  : > "$OUTDIR/targeted-status.jsonl"
  : > "$OUTDIR/process-memory.jsonl"
fi

for profile in "${PROFILES[@]}"; do
  for seed_index in "${!SEEDS[@]}"; do
    seed=${SEEDS[$seed_index]}
    apply_netem "$profile" "$seed"
    if [ "$MODE" = targeted ]; then
      ORDERED_TRANSPORTS=("${TARGET_ORDERED_TRANSPORTS[@]}")
      order=$(IFS=,; echo "${ORDERED_TRANSPORTS[*]}")
    elif [ $((seed_index % 2)) -eq 0 ]; then
      order=quic,tcp
      ORDERED_TRANSPORTS=(quic tcp)
    else
      order=tcp,quic
      ORDERED_TRANSPORTS=(tcp quic)
    fi
    for direction in "${DIRECTIONS[@]}"; do
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
        if [ "$MODE" = targeted ]; then
          if run_direct_matrix \
              "$profile" "$seed" "$direction" "$transport" "$order" "$order_index"; then
            record_targeted_stage direct "$transport" passed 0 "$order_index"
          else
            case_status=$?
            TARGETED_RUN_STATUS=1
            record_targeted_stage direct "$transport" failed "$case_status" \
              "$order_index" "direct measurement failed; see console and raw-failures.jsonl when present"
          fi
          direct_pid=$DIRECT_QUIC_PID
          if [ "$transport" = tcp ]; then
            direct_pid=$DIRECT_TCP_PID
          fi
          capture_targeted_memory direct "$transport" edge "$EDGE_PID" "$order_index"
          capture_targeted_memory direct "$transport" direct-server \
            "$direct_pid" "$order_index"

          if start_agent "$profile" "$seed" "$direction" "$transport"; then
            if run_beamd_protocol \
                "$profile" "$seed" "$direction" "$transport" "$order" "$order_index"; then
              record_targeted_stage beamd "$transport" passed 0 "$order_index"
            else
              case_status=$?
              TARGETED_RUN_STATUS=1
              record_targeted_stage beamd "$transport" failed "$case_status" \
                "$order_index" "beamd measurement failed; see console, agent/edge logs, and raw-failures.jsonl"
            fi
          else
            case_status=$?
            TARGETED_RUN_STATUS=1
            record_targeted_stage beamd "$transport" skipped "$case_status" \
              "$order_index" "forced transport preflight or tunnel health failed"
          fi
          capture_targeted_memory beamd "$transport" edge "$EDGE_PID" "$order_index"
          capture_targeted_memory beamd "$transport" agent \
            "${AGENT_PID:-}" "$order_index"

          if record_qdisc_snapshot "$profile" "$seed" "$direction" "$transport"; then
            :
          else
            case_status=$?
            TARGETED_RUN_STATUS=1
            record_targeted_stage qdisc "$transport" failed "$case_status" \
              "$order_index" "could not record post-case qdisc counters"
          fi
        else
          run_direct_matrix "$profile" "$seed" "$direction" "$transport" "$order" "$order_index"
          start_agent "$profile" "$seed" "$direction" "$transport"
          run_beamd_protocol "$profile" "$seed" "$direction" "$transport" "$order" "$order_index"
          run_mixed "$profile" "$seed" "$direction" "$transport" "$order" "$order_index"
          record_qdisc_snapshot "$profile" "$seed" "$direction" "$transport"
        fi
        stop_agent
      done
    done
  done
done

write_final_qdisc() {
  {
    echo "## edge/$EDGE_IF"
    ip netns exec "$EDGE_NS" "$TC_BIN" -s qdisc show dev "$EDGE_IF" ||
      return $?
    echo "## agent/$AGENT_IF"
    ip netns exec "$AGENT_NS" "$TC_BIN" -s qdisc show dev "$AGENT_IF" ||
      return $?
  } > "$OUTDIR/qdisc-final.txt"
}

if [ "$MODE" = targeted ]; then
  if write_final_qdisc; then
    :
  else
    case_status=$?
    TARGETED_RUN_STATUS=1
    record_targeted_stage qdisc all failed "$case_status" 0 \
      "could not record final qdisc counters"
  fi
else
  write_final_qdisc
fi

# Re-verify the checkout, harness, analyzer, and every executable after the
# long collection phase. The final verdict never runs from mutable source.
verify_recorded_assets() {
  [ "$(sha256_file "$OUTDIR/b4_analyze.py")" = \
    "$(sha256_file "$BINDIR/b4_analyze.py")" ] || {
    echo "recorded analyzer changed during evidence collection" >&2
    return 2
  }
  [ "$(sha256_file "$OUTDIR/perf-netem.sh")" = \
    "$(sha256_file "$ROOT/scripts/perf-netem.sh")" ] || {
    echo "recorded harness changed during evidence collection" >&2
    return 2
  }
  if [ "$MODE" = targeted ]; then
    [ "$(sha256_file "$OUTDIR/manifest.json")" = \
      "$(sha256_file "$BINDIR/manifest.json")" ] || {
      echo "recorded manifest changed during targeted evidence collection" >&2
      return 2
    }
  fi
}
if [ "$MODE" != targeted ]; then
  verify_immutable_inputs
  verify_traffic_control
  verify_recorded_assets
fi

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
TARGETED=False
[ "$MODE" = targeted ] && TARGETED=True
TARGET_TRANSPORT_ORDER=tcp,quic
if [ "$MODE" = targeted ]; then
  TARGET_TRANSPORT_ORDER=$(IFS=,; echo "${TARGET_ORDERED_TRANSPORTS[*]}")
fi
GIT_DIRTY=False
[ "$MANIFEST_DIRTY" = true ] && GIT_DIRTY=True
python3 - "$OUTDIR/metadata.json" <<PY
import json
import pathlib
import platform

path = pathlib.Path(r"""$OUTDIR/metadata.json""")
metadata = {
    "schema_version": 1,
    "mode": "$MODE",
    "qualification": $QUALIFICATION,
    "targeted": $TARGETED,
    "started_utc": "$RUN_STAMP",
    "completed_utc": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
    "beamd_commit": "$COMMIT",
    "git_dirty": $GIT_DIRTY,
    "binary_manifest": json.load(open(r"""$BINDIR/manifest.json""")),
    "recorded_analyzer": "b4_analyze.py",
    "recorded_harness": "perf-netem.sh",
    "recorded_manifest": "manifest.json" if $TARGETED else None,
    "process_memory": "process-memory.jsonl" if $TARGETED else None,
    "targeted_integrity": "targeted-integrity.json" if $TARGETED else None,
    "go_version": r"""$GO_VERSION""",
    "quic_go_version": "$QUIC_VERSION",
    "yamux_version": "$YAMUX_VERSION",
    "kernel": platform.release(),
    "os": platform.platform(),
    "cpu": r"""$CPU""",
    "cpu_count": int("$CPU_COUNT"),
    "ram_bytes": int("$RAM_BYTES"),
    "resource_limits": r"""$RESOURCE_LIMITS""",
    "container_limits": r"""$CONTAINER_LIMITS""",
    "runtime_environment": {
        "beamd_home_isolated": True,
        "beamd_home_inherited": False,
    },
    "traffic_control": {
        "binary": r"""$TC_BIN""",
        "source_binary": r"""$TC_SOURCE_BIN""",
        "recorded_binary": "traffic-control/tc",
        "version": r"""$TC_VERSION""",
        "sha256": "$TC_SHA256",
        "deterministic_seed_supported": True,
    },
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
    "seed_orders": (
        {
            str(seed): "$TARGET_TRANSPORT_ORDER"
            for seed in [int(value) for value in "$SEEDS_CSV".split(",")]
        }
        if $TARGETED
        else {
            str(seed): ("quic,tcp" if index % 2 == 0 else "tcp,quic")
            for index, seed in enumerate(
                [int(value) for value in "$SEEDS_CSV".split(",")]
            )
        }
    ),
    "transport_orders": ["quic,tcp", "tcp,quic"] if $QUALIFICATION else ["quic,tcp"],
    "target_case": (
        {
            "profile": "${PROFILES[0]}",
            "seed": int("${SEEDS[0]}"),
            "direction": "${DIRECTIONS[0]}",
            "size_bytes": int("${PROTOCOL_SIZES[0]}"),
            "transport_order": "$TARGET_TRANSPORT_ORDER",
            "beamd_concurrency": int("$TARGET_BEAMD_CONCURRENCY_VALUE"),
            "warmups": int("$PROTOCOL_WARMUP"),
            "iterations": int("$(iterations_for "${PROFILES[0]}" "${PROTOCOL_SIZES[0]}")"),
        }
        if $TARGETED
        else None
    ),
    "qdisc_artifacts": "$QDISC_ARTIFACTS".split(","),
    "handshake_included": False,
}
if $TARGETED:
    metadata["transport_orders"] = ["$TARGET_TRANSPORT_ORDER"]
path.write_text(json.dumps(metadata, indent=2) + "\n")
PY

if [ "$MODE" = targeted ]; then
  if verify_immutable_inputs; then
    targeted_immutable_status=0
  else
    targeted_immutable_status=$?
    TARGETED_RUN_STATUS=1
  fi
  if verify_traffic_control; then
    targeted_tc_status=0
  else
    targeted_tc_status=$?
    TARGETED_RUN_STATUS=1
  fi
  if verify_recorded_assets; then
    targeted_recorded_assets_status=0
  else
    targeted_recorded_assets_status=$?
    TARGETED_RUN_STATUS=1
  fi
  python3 - "$OUTDIR/targeted-integrity.json" "$targeted_immutable_status" \
    "$targeted_tc_status" "$targeted_recorded_assets_status" <<'PY'
import datetime
import json
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
path.write_text(
    json.dumps(
        {
            "checked_utc": datetime.datetime.now(
                datetime.timezone.utc
            ).isoformat(),
            "immutable_inputs_status": int(sys.argv[2]),
            "traffic_control_status": int(sys.argv[3]),
            "recorded_assets_status": int(sys.argv[4]),
        },
        indent=2,
    )
    + "\n",
    encoding="utf-8",
)
PY
  target_iterations=$(iterations_for "${PROFILES[0]}" "${PROTOCOL_SIZES[0]}")
  if python3 - "$OUTDIR" "${PROFILES[0]}" "${SEEDS[0]}" "${DIRECTIONS[0]}" \
      "${PROTOCOL_SIZES[0]}" "$TARGET_TRANSPORT_ORDER" "$target_iterations" \
      "$PROTOCOL_WARMUP" "$TARGET_BEAMD_CONCURRENCY_VALUE" \
      "$TARGETED_RUN_STATUS" <<'PY'
import base64
import datetime
import hashlib
import json
import math
import pathlib
import re
import sys

root = pathlib.Path(sys.argv[1])
profile = sys.argv[2]
seed = int(sys.argv[3])
direction = sys.argv[4]
size = int(sys.argv[5])
transports = sys.argv[6].split(",")
iterations = int(sys.argv[7])
warmups = int(sys.argv[8])
beamd_concurrency = int(sys.argv[9])
collection_status = int(sys.argv[10])
issues = []

def check(condition, message):
    if not condition:
        issues.append(message)

def load(name, *, required=True):
    path = root / name
    if not path.exists():
        if required:
            issues.append(f"{name}: missing")
        return []
    records = []
    for line_number, line in enumerate(
        path.read_text(encoding="utf-8").splitlines(), start=1
    ):
        if not line.strip():
            continue
        try:
            record = json.loads(line)
        except json.JSONDecodeError as error:
            issues.append(f"{name}:{line_number}: invalid JSON: {error}")
            continue
        if not isinstance(record, dict):
            issues.append(f"{name}:{line_number}: JSON record is not an object")
            continue
        records.append(record)
    return records

def load_object(name):
    path = root / name
    if not path.is_file() or path.is_symlink():
        issues.append(f"{name}: missing, non-regular, or symlink")
        return {}
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as error:
        issues.append(f"{name}: invalid JSON: {error}")
        return {}
    if not isinstance(value, dict):
        issues.append(f"{name}: JSON value is not an object")
        return {}
    return value

def safe_artifact(relative, label):
    if (
        not isinstance(relative, str)
        or not relative
        or pathlib.PurePosixPath(relative).is_absolute()
        or ".." in pathlib.PurePosixPath(relative).parts
    ):
        issues.append(f"{label}: unsafe relative path {relative!r}")
        return None
    current = root
    for part in pathlib.PurePosixPath(relative).parts:
        current = current / part
        if current.is_symlink():
            issues.append(f"{label}: symlink component {current}")
            return None
    try:
        resolved = current.resolve(strict=True)
        resolved.relative_to(root.resolve(strict=True))
    except (OSError, ValueError) as error:
        issues.append(f"{label}: path is unavailable or escapes evidence root: {error}")
        return None
    if not resolved.is_file():
        issues.append(f"{label}: not a regular file")
        return None
    return resolved

def sha256(path):
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()

def integer(value):
    return isinstance(value, int) and not isinstance(value, bool)

def finite_number(value):
    return (
        isinstance(value, (int, float))
        and not isinstance(value, bool)
        and math.isfinite(value)
    )

def parse_simple_yaml(path, label):
    values = {}
    section = None
    try:
        lines = path.read_text(encoding="utf-8").splitlines()
    except (OSError, UnicodeDecodeError) as error:
        issues.append(f"{label}: unreadable: {error}")
        return values
    for line_number, raw in enumerate(lines, start=1):
        if not raw.strip() or raw.lstrip().startswith("#"):
            continue
        indent = len(raw) - len(raw.lstrip())
        if ":" not in raw:
            issues.append(f"{label}:{line_number}: expected key/value")
            continue
        key, value = raw.strip().split(":", 1)
        value = value.strip()
        if indent == 0:
            section = key if value == "" else None
            full_key = key
        elif section:
            full_key = f"{section}.{key}"
        else:
            issues.append(f"{label}:{line_number}: unexpected indentation")
            continue
        if value == "":
            continue
        if full_key in values:
            issues.append(f"{label}: duplicate key {full_key}")
        if len(value) >= 2 and value[0] == value[-1] and value[0] in "\"'":
            value = value[1:-1]
        values[full_key] = value
    return values

order_text = ",".join(transports)
expected_bytes_semantics = (
    "response body bytes read"
    if direction == "download"
    else "request body bytes consumed when Client.Do returned"
)
expected_direct_wire_direction = (
    "agent-to-edge" if direction == "download" else "edge-to-agent"
)
metadata = load_object("metadata.json")
manifest = load_object("manifest.json")
integrity = load_object("targeted-integrity.json")

expected_target_case = {
    "profile": profile,
    "seed": seed,
    "direction": direction,
    "size_bytes": size,
    "transport_order": order_text,
    "beamd_concurrency": beamd_concurrency,
    "warmups": warmups,
    "iterations": iterations,
}
metadata_expected = {
    "schema_version": 1,
    "mode": "targeted",
    "qualification": False,
    "targeted": True,
    "git_dirty": False,
    "handshake_included": False,
    "netem_seeds": [seed],
    "seed_orders": {str(seed): order_text},
    "transport_orders": [order_text],
    "target_case": expected_target_case,
    "recorded_analyzer": "b4_analyze.py",
    "recorded_harness": "perf-netem.sh",
    "recorded_manifest": "manifest.json",
    "process_memory": "process-memory.jsonl",
    "targeted_integrity": "targeted-integrity.json",
    "runtime_environment": {
        "beamd_home_isolated": True,
        "beamd_home_inherited": False,
    },
    "public_leg_shaped": False,
    "directions_shaped": ["edge-to-agent", "agent-to-edge"],
    "netem_queue_limit_packets": 1000,
}
for field, expected in metadata_expected.items():
    if metadata.get(field) != expected:
        issues.append(
            f"metadata.json: {field}={metadata.get(field)!r}, want {expected!r}"
        )
check(
    metadata.get("topology")
    == "edge namespace/public client <-> shaped veth <-> agent namespace/backend",
    "metadata.json: unexpected topology",
)
check(
    integer(metadata.get("cpu_count")) and metadata.get("cpu_count") >= 2,
    "metadata.json: cpu_count must be an integer >= 2",
)
check(
    integer(metadata.get("ram_bytes")) and metadata.get("ram_bytes") >= 2_000_000_000,
    "metadata.json: ram_bytes must be an integer >= 2000000000",
)
try:
    started = datetime.datetime.strptime(
        str(metadata.get("started_utc", "")), "%Y%m%dT%H%M%SZ"
    ).replace(tzinfo=datetime.timezone.utc)
    completed = datetime.datetime.fromisoformat(
        str(metadata.get("completed_utc", "")).replace("Z", "+00:00")
    )
    check(completed >= started, "metadata.json: completed_utc precedes started_utc")
except ValueError as error:
    issues.append(f"metadata.json: invalid timestamps: {error}")

digest_pattern = re.compile(r"^[0-9a-f]{64}$")
commit = manifest.get("commit")
check(
    isinstance(commit, str) and re.fullmatch(r"[0-9a-f]{40}", commit) is not None,
    "manifest.json: commit must be a full lowercase SHA-1",
)
check(manifest.get("schema_version") == 1, "manifest.json: schema_version must be 1")
check(manifest.get("dirty") is False, "manifest.json: dirty must be false")
check(
    metadata.get("beamd_commit") == commit,
    "metadata.json: beamd_commit does not match recorded manifest",
)
check(
    metadata.get("binary_manifest") == manifest,
    "metadata.json: embedded binary_manifest differs from manifest.json",
)
check(
    manifest.get("go_version") == metadata.get("go_version"),
    "metadata.json: go_version differs from manifest",
)
expected_binary_names = {
    "beamd", "perfclient", "perfserver", "directclient", "directserver"
}
binary_hashes = manifest.get("binaries")
check(
    isinstance(binary_hashes, dict) and set(binary_hashes) == expected_binary_names,
    "manifest.json: unexpected binary hash inventory",
)
if isinstance(binary_hashes, dict):
    for name, digest in binary_hashes.items():
        check(
            isinstance(digest, str) and digest_pattern.fullmatch(digest) is not None,
            f"manifest.json: invalid {name} binary digest",
        )
expected_asset_names = {
    "b4_analyze.py", "test/perf/b4_analyze.py", "scripts/perf-netem.sh"
}
asset_hashes = manifest.get("assets")
check(
    isinstance(asset_hashes, dict) and set(asset_hashes) == expected_asset_names,
    "manifest.json: unexpected asset hash inventory",
)
if isinstance(asset_hashes, dict):
    for name, digest in asset_hashes.items():
        check(
            isinstance(digest, str) and digest_pattern.fullmatch(digest) is not None,
            f"manifest.json: invalid {name} asset digest",
        )
    check(
        asset_hashes.get("b4_analyze.py")
        == asset_hashes.get("test/perf/b4_analyze.py"),
        "manifest.json: analyzer asset digests differ",
    )

recorded_harness = safe_artifact(
    metadata.get("recorded_harness"), "recorded harness"
)
recorded_analyzer = safe_artifact(
    metadata.get("recorded_analyzer"), "recorded analyzer"
)
recorded_manifest = safe_artifact(
    metadata.get("recorded_manifest"), "recorded manifest"
)
if recorded_harness is not None and isinstance(asset_hashes, dict):
    check(
        sha256(recorded_harness) == asset_hashes.get("scripts/perf-netem.sh"),
        "recorded harness hash differs from manifest",
    )
if recorded_analyzer is not None and isinstance(asset_hashes, dict):
    analyzer_digest = sha256(recorded_analyzer)
    check(
        analyzer_digest == asset_hashes.get("b4_analyze.py"),
        "recorded analyzer hash differs from manifest",
    )
    check(
        analyzer_digest == asset_hashes.get("test/perf/b4_analyze.py"),
        "recorded analyzer hash differs from source analyzer asset",
    )
if recorded_manifest is not None:
    check(
        load_object(metadata.get("recorded_manifest")) == manifest,
        "recorded manifest changed while validating targeted evidence",
    )

traffic = metadata.get("traffic_control")
if not isinstance(traffic, dict):
    issues.append("metadata.json: traffic_control must be an object")
    traffic = {}
check(
    traffic.get("recorded_binary") == "traffic-control/tc",
    "metadata.json: unexpected recorded tc path",
)
check(
    traffic.get("deterministic_seed_supported") is True,
    "metadata.json: deterministic tc seed support was not recorded",
)
check(
    isinstance(traffic.get("binary"), str)
    and pathlib.Path(traffic.get("binary")).is_absolute(),
    "metadata.json: tc binary path must be absolute",
)
check(
    isinstance(traffic.get("source_binary"), str)
    and pathlib.Path(traffic.get("source_binary")).is_absolute(),
    "metadata.json: tc source path must be absolute",
)
recorded_tc = safe_artifact(traffic.get("recorded_binary"), "recorded tc")
if recorded_tc is not None:
    check(0 < recorded_tc.stat().st_size <= 50 * 1024 * 1024, "recorded tc size invalid")
    check(
        sha256(recorded_tc) == traffic.get("sha256"),
        "recorded tc hash differs from metadata",
    )
check(
    isinstance(traffic.get("sha256"), str)
    and digest_pattern.fullmatch(traffic.get("sha256", "")) is not None,
    "metadata.json: invalid tc sha256",
)

integrity_expected = {
    "immutable_inputs_status": 0,
    "traffic_control_status": 0,
    "recorded_assets_status": 0,
}
for field, expected in integrity_expected.items():
    if integrity.get(field) != expected:
        issues.append(
            f"targeted-integrity.json: {field}={integrity.get(field)!r}, want 0"
        )
try:
    datetime.datetime.fromisoformat(
        str(integrity.get("checked_utc", "")).replace("Z", "+00:00")
    )
except ValueError as error:
    issues.append(f"targeted-integrity.json: invalid checked_utc: {error}")

effective = metadata.get("effective_config")
expected_effective = {
    "edge": "effective-config/edge.yaml",
    "tcp_client": "effective-config/client-tcp.yaml",
    "quic_client": "effective-config/client-quic.yaml",
    "gomemlimit": "1400MiB",
    "yamux_stream_window_bytes": 4194304,
}
check(effective == expected_effective, "metadata.json: unexpected effective_config")
edge_config_path = safe_artifact(expected_effective["edge"], "edge config")
if edge_config_path is not None:
    edge_config = parse_simple_yaml(edge_config_path, "edge config")
    expected_edge_config = {
        "base_domain": "perf.local",
        "url_shape": "hyphen",
        "listen_https": "10.231.0.1:443",
        "listen_quic": "10.231.0.1:443",
        "disable_quic": "false",
        "acme_email": "perf@example.com",
        "acme_ca": "off",
        "dns_provider": "stub",
        "max_streams_per_session": "64",
        "max_streams_total": "128",
        "max_pre_auth_sessions": "32",
        "max_sessions_total": "8",
        "max_request_body_bytes": "134217728",
        "request_log.enabled": "false",
    }
    for field, expected in expected_edge_config.items():
        if edge_config.get(field) != expected:
            issues.append(
                f"edge config: {field}={edge_config.get(field)!r}, want {expected!r}"
            )
    check(
        set(edge_config)
        == set(expected_edge_config) | {"token_store", "data_dir"},
        "edge config: unexpected key inventory",
    )
    check(
        edge_config.get("token_store", "").startswith("file:/tmp/")
        and edge_config.get("token_store", "").endswith("/tokens.json"),
        "edge config: unexpected token_store",
    )
    check(
        edge_config.get("data_dir", "").startswith("/tmp/")
        and edge_config.get("data_dir", "").endswith("/edge-data"),
        "edge config: unexpected data_dir",
    )

for transport in transports:
    label = f"{transport} client config"
    relative = f"effective-config/client-{transport}.yaml"
    client_path = safe_artifact(relative, label)
    if client_path is None:
        continue
    client_config = parse_simple_yaml(client_path, label)
    expected_client = {
        "server": "10.231.0.1:443",
        "token": "PERFTOKEN",
        "insecure_skip_verify": "true",
        "transport": transport,
    }
    for field, expected in expected_client.items():
        if client_config.get(field) != expected:
            issues.append(
                f"{label}: {field}={client_config.get(field)!r}, want {expected!r}"
            )
    check(
        set(client_config) == set(expected_client) | {"agent_socket"},
        f"{label}: unexpected key inventory",
    )
    check(
        client_config.get("agent_socket", "").startswith("/tmp/")
        and client_config.get("agent_socket", "").endswith(
            f"/agent-{transport}.sock"
        ),
        f"{label}: unexpected agent_socket",
    )

direct_fixture = metadata.get("direct_fixture")
if not isinstance(direct_fixture, dict):
    issues.append("metadata.json: direct_fixture must be an object")
    direct_fixture = {}
certificate = safe_artifact(
    direct_fixture.get("certificate"), "direct fixture certificate"
)
if certificate is not None:
    try:
        pem_lines = [
            line.strip()
            for line in certificate.read_text(encoding="ascii").splitlines()
            if line and not line.startswith("-----")
        ]
        certificate_digest = hashlib.sha256(
            base64.b64decode("".join(pem_lines), validate=True)
        ).hexdigest()
        check(
            certificate_digest == direct_fixture.get("certificate_sha256"),
            "direct fixture certificate fingerprint mismatch",
        )
    except (OSError, UnicodeDecodeError, ValueError) as error:
        issues.append(f"direct fixture certificate: invalid PEM: {error}")

offload_path = safe_artifact("interface-offload.txt", "interface offload")
if offload_path is not None:
    offload_text = offload_path.read_text(encoding="utf-8").lower()
    for setting in (
        "generic-receive-offload: off",
        "generic-segmentation-offload: off",
        "tcp-segmentation-offload: off",
    ):
        check(
            offload_text.count(setting) >= 2,
            f"interface-offload.txt: expected {setting!r} for both interfaces",
        )

for transport in transports:
    check_name = f"check-{profile}-{seed}-{direction}-{transport}.json"
    check_record = load_object(check_name)
    expected_check = {
        "ok": True,
        "server": "10.231.0.1:443",
        "slug": "perf",
        "baseDomain": "perf.local",
        "transport": transport,
    }
    for field, expected in expected_check.items():
        if check_record.get(field) != expected:
            issues.append(
                f"{check_name}: {field}={check_record.get(field)!r}, want {expected!r}"
            )
    check(
        integer(check_record.get("handshakeMs"))
        and check_record.get("handshakeMs") >= 0,
        f"{check_name}: handshakeMs must be a non-negative integer",
    )
    check("error" not in check_record, f"{check_name}: unexpected error field")

    health_name = f"health-{profile}-{seed}-{direction}-{transport}.json"
    health = load_object(health_name)
    expected_health = {
        "profile": profile,
        "transport": transport,
        "size": 36,
        "dir": direction,
        "concurrency": 1,
        "iterations": 1,
        "errors": 0,
        "corrupt": 0,
        "bytes_semantics": expected_bytes_semantics,
    }
    for field, expected in expected_health.items():
        if health.get(field) != expected:
            issues.append(
                f"{health_name}: {field}={health.get(field)!r}, want {expected!r}"
            )
    health_samples = health.get("samples")
    check(
        isinstance(health_samples, list) and len(health_samples) == 1,
        f"{health_name}: expected one raw sample",
    )
    if isinstance(health_samples, list) and len(health_samples) == 1:
        sample = health_samples[0]
        check(isinstance(sample, dict), f"{health_name}: sample is not an object")
        if isinstance(sample, dict):
            check(sample.get("i") == 0, f"{health_name}: sample index must be 0")
            check(sample.get("ok") is True, f"{health_name}: sample did not pass")
            check("err" not in sample, f"{health_name}: sample retained an error")
            check(sample.get("bytes") == 36, f"{health_name}: sample bytes must be 36")
            check(
                finite_number(sample.get("elapsed_ms"))
                and sample.get("elapsed_ms") > 0,
                f"{health_name}: invalid elapsed_ms",
            )
            check(
                finite_number(sample.get("ttfb_ms"))
                and sample.get("ttfb_ms") >= 0,
                f"{health_name}: invalid ttfb_ms",
            )

memory_records = load("process-memory.jsonl")
expected_memory_order = []
for transport in transports:
    expected_memory_order.extend(
        [
            ("direct", transport, "edge"),
            ("direct", transport, "direct-server"),
            ("beamd", transport, "edge"),
            ("beamd", transport, "agent"),
        ]
    )
actual_memory_order = [
    (record.get("fixture"), record.get("transport"), record.get("role"))
    for record in memory_records
]
check(
    actual_memory_order == expected_memory_order,
    f"process-memory.jsonl: order/cardinality {actual_memory_order}, "
    f"want {expected_memory_order}",
)
for record in memory_records:
    fixture = record.get("fixture")
    transport = record.get("transport")
    role = record.get("role")
    expected_index = transports.index(transport) + 1 if transport in transports else 0
    expected_memory = {
        "schema_version": 1,
        "profile": profile,
        "seed": seed,
        "dir": direction,
        "size": size,
        "order": order_text,
        "order_index": expected_index,
    }
    for field, expected in expected_memory.items():
        if record.get(field) != expected:
            issues.append(
                f"process-memory.jsonl: {fixture}/{transport}/{role} "
                f"{field}={record.get(field)!r}, want {expected!r}"
            )
    try:
        datetime.datetime.fromisoformat(
            str(record.get("captured_utc", "")).replace("Z", "+00:00")
        )
    except ValueError as error:
        issues.append(
            f"process-memory.jsonl: {fixture}/{transport}/{role} "
            f"invalid captured_utc: {error}"
        )
    required = role in ("edge", "agent")
    if required:
        check(
            record.get("available") is True,
            f"process-memory.jsonl: required {fixture}/{transport}/{role} "
            f"snapshot unavailable: {record.get('error', '')}",
        )
    if record.get("available") is True:
        rss = record.get("vmrss_bytes")
        high_water = record.get("vmhwm_bytes")
        check(
            integer(record.get("pid")) and record.get("pid") > 1,
            f"process-memory.jsonl: invalid pid for {fixture}/{transport}/{role}",
        )
        check(
            integer(rss) and rss > 0,
            f"process-memory.jsonl: invalid VmRSS for {fixture}/{transport}/{role}",
        )
        check(
            integer(high_water)
            and integer(rss)
            and high_water >= rss,
            f"process-memory.jsonl: invalid VmHWM for {fixture}/{transport}/{role}",
        )
        check(
            isinstance(record.get("comm"), str) and bool(record.get("comm")),
            f"process-memory.jsonl: missing comm for {fixture}/{transport}/{role}",
        )
        expected_binary = "directserver" if role == "direct-server" else "beamd"
        if isinstance(binary_hashes, dict):
            check(
                record.get("binary_sha256") == binary_hashes.get(expected_binary),
                f"process-memory.jsonl: binary hash mismatch for "
                f"{fixture}/{transport}/{role}",
            )
        check(
            isinstance(record.get("exe"), str)
            and pathlib.Path(record.get("exe")).name == expected_binary,
            f"process-memory.jsonl: executable mismatch for "
            f"{fixture}/{transport}/{role}",
        )

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
check(
    metadata.get("netem_profiles") == expected_profiles,
    "metadata.json: unexpected netem_profiles",
)
selected_profile = expected_profiles.get(profile)
if selected_profile is None:
    issues.append(f"targeted qdisc: unsupported profile {profile!r}")
    selected_profile = {"one_way_delay_ms": 0, "loss_percent": 0, "rate_mbit": 0}

def validate_netem_line(line, label, expected_seed):
    lowered = line.lower()
    check(
        re.search(r"\blimit\s+1000\b", lowered) is not None,
        f"{label}: missing limit 1000",
    )
    delay = selected_profile["one_way_delay_ms"]
    check(
        re.search(rf"\bdelay\s+{delay}(?:\.0+)?ms\b", lowered) is not None,
        f"{label}: unexpected delay",
    )
    rate = selected_profile["rate_mbit"]
    check(
        re.search(rf"\brate\s+{rate}(?:\.0+)?mbit\b", lowered) is not None,
        f"{label}: unexpected rate",
    )
    loss_match = re.search(
        r"\bloss(?:\s+random)?\s+([0-9]+(?:\.[0-9]+)?)%", lowered
    )
    expected_loss = float(selected_profile["loss_percent"])
    if expected_loss == 0:
        check(loss_match is None, f"{label}: unexpected loss setting")
    else:
        check(loss_match is not None, f"{label}: missing loss setting")
        if loss_match is not None:
            check(
                float(loss_match.group(1)) == expected_loss,
                f"{label}: unexpected loss percentage",
            )
    check(
        re.search(rf"\bseed\s+{expected_seed}\b", lowered) is not None,
        f"{label}: unexpected or missing seed",
    )

def validate_qdisc_snapshot(lines, label):
    edge_positions = [index for index, line in enumerate(lines) if line == "edge-to-agent:"]
    agent_positions = [
        index for index, line in enumerate(lines) if line == "agent-to-edge:"
    ]
    if len(edge_positions) != 1 or len(agent_positions) != 1:
        issues.append(
            f"{label}: expected exactly one edge-to-agent and agent-to-edge section"
        )
        return
    edge_index, agent_index = edge_positions[0], agent_positions[0]
    if edge_index >= agent_index:
        issues.append(f"{label}: qdisc sides are out of order")
        return
    sides = (
        ("edge-to-agent", lines[edge_index + 1 : agent_index], seed),
        ("agent-to-edge", lines[agent_index + 1 :], seed + 1_000_003),
    )
    for side, section, expected_seed in sides:
        netem = [line for line in section if line.lower().startswith("qdisc netem ")]
        if len(netem) != 1:
            issues.append(f"{label}/{side}: expected exactly one qdisc netem line")
            continue
        validate_netem_line(netem[0], f"{label}/{side}", expected_seed)

qdisc_name = f"{profile}-{seed}.txt"
check(
    metadata.get("qdisc_artifacts") == [qdisc_name],
    "metadata.json: qdisc_artifacts does not match targeted case",
)
qdisc_dir = root / "qdisc"
if not qdisc_dir.is_dir() or qdisc_dir.is_symlink():
    issues.append("qdisc: missing directory or symlink")
else:
    qdisc_inventory = sorted(path.name for path in qdisc_dir.iterdir())
    check(
        qdisc_inventory == [qdisc_name],
        f"qdisc: artifact inventory {qdisc_inventory}, want {[qdisc_name]}",
    )
qdisc_path = safe_artifact(f"qdisc/{qdisc_name}", "targeted qdisc")
if qdisc_path is not None:
    qdisc_lines = qdisc_path.read_text(encoding="utf-8").splitlines()
    expected_header = f"profile={profile} seed={seed}"
    check(
        bool(qdisc_lines) and qdisc_lines[0] == expected_header,
        f"targeted qdisc: missing exact header {expected_header!r}",
    )
    marker_positions = [
        (index, line)
        for index, line in enumerate(qdisc_lines)
        if line.startswith("after direction=")
    ]
    expected_markers = [
        f"after direction={direction} transport={transport}"
        for transport in transports
    ]
    check(
        [line for _, line in marker_positions] == expected_markers,
        "targeted qdisc: post-case markers are missing, duplicated, or out of order",
    )
    boundaries = [index for index, _ in marker_positions] + [len(qdisc_lines)]
    initial_end = boundaries[0] if boundaries else len(qdisc_lines)
    validate_qdisc_snapshot(qdisc_lines[1:initial_end], "targeted qdisc/initial")
    for marker_index, (start, marker) in enumerate(marker_positions):
        end = boundaries[marker_index + 1]
        validate_qdisc_snapshot(
            qdisc_lines[start + 1 : end], f"targeted qdisc/{marker}"
        )

final_qdisc_path = safe_artifact("qdisc-final.txt", "final qdisc")
if final_qdisc_path is not None:
    final_lines = final_qdisc_path.read_text(encoding="utf-8").splitlines()
    edge_headers = [
        index for index, line in enumerate(final_lines) if line.startswith("## edge/")
    ]
    agent_headers = [
        index for index, line in enumerate(final_lines) if line.startswith("## agent/")
    ]
    if (
        len(edge_headers) != 1
        or len(agent_headers) != 1
        or edge_headers[0] >= agent_headers[0]
    ):
        issues.append("qdisc-final.txt: invalid edge/agent section cardinality")
    else:
        synthetic = (
            ["edge-to-agent:"]
            + final_lines[edge_headers[0] + 1 : agent_headers[0]]
            + ["agent-to-edge:"]
            + final_lines[agent_headers[0] + 1 :]
        )
        validate_qdisc_snapshot(synthetic, "qdisc-final.txt")

stage_results = load("targeted-status.jsonl")
if collection_status != 0:
    issues.append(f"targeted collection status was {collection_status}")
expected_stage_order = [
    (fixture, transport)
    for transport in transports
    for fixture in ("direct", "beamd")
]
actual_stage_order = [
    (record.get("fixture"), record.get("transport"))
    for record in stage_results
    if record.get("fixture") in ("direct", "beamd")
]
check(
    actual_stage_order == expected_stage_order,
    f"targeted-status.jsonl: stage order {actual_stage_order}, "
    f"want {expected_stage_order}",
)
expected_stage_keys = {
    (fixture, transport)
    for fixture in ("direct", "beamd")
    for transport in transports
}
seen_stage_keys = set()
for record in stage_results:
    fixture = record.get("fixture")
    transport = record.get("transport")
    outcome = record.get("outcome")
    exit_status = record.get("exit_status")
    check(
        record.get("schema_version") == 1,
        f"targeted-status.jsonl: {fixture}/{transport} schema_version must be 1",
    )
    if outcome != "passed":
        issues.append(
            f"stage {fixture}/{transport}: {outcome}: {record.get('detail', '')}"
        )
    if outcome == "passed" and exit_status != 0:
        issues.append(
            f"targeted-status.jsonl: passed stage {fixture}/{transport} "
            f"has exit_status {exit_status}"
        )
    if outcome == "passed" and record.get("detail") != "":
        issues.append(
            f"targeted-status.jsonl: passed stage {fixture}/{transport} "
            "must have empty detail"
        )
    if fixture not in ("direct", "beamd"):
        continue
    key = (fixture, transport)
    if key in seen_stage_keys:
        issues.append(f"targeted-status.jsonl: duplicate stage {fixture}/{transport}")
    seen_stage_keys.add(key)
    index = transports.index(transport) + 1 if transport in transports else 0
    expected_stage = {
        "workload": "protocol",
        "profile": profile,
        "seed": seed,
        "dir": direction,
        "size": size,
        "order": ",".join(transports),
        "order_index": index,
        "warmups": warmups,
    }
    mismatches = {
        field: (record.get(field), value)
        for field, value in expected_stage.items()
        if record.get(field) != value
    }
    if transport not in transports:
        mismatches["transport"] = (transport, transports)
    if mismatches:
        issues.append(
            f"targeted-status.jsonl: {fixture}/{transport} mismatch: {mismatches}"
        )
for fixture, transport in sorted(expected_stage_keys - seen_stage_keys):
    issues.append(f"targeted-status.jsonl: missing stage {fixture}/{transport}")

def validate_success_sample(sample, label, expected_index, *, require_bytes):
    if not isinstance(sample, dict):
        issues.append(f"{label}: sample {expected_index} is not an object")
        return
    check(sample.get("i") == expected_index, f"{label}: sample index mismatch")
    check(sample.get("ok") is True, f"{label}: sample did not pass")
    check("err" not in sample, f"{label}: successful sample retained err")
    check(
        finite_number(sample.get("elapsed_ms")) and sample.get("elapsed_ms") > 0,
        f"{label}: invalid elapsed_ms",
    )
    check(
        finite_number(sample.get("ttfb_ms")) and sample.get("ttfb_ms") >= 0,
        f"{label}: invalid ttfb_ms",
    )
    if require_bytes:
        check(
            integer(sample.get("bytes")) and sample.get("bytes") == size,
            f"{label}: successful sample bytes must equal {size}",
        )

def validate_success_fail_fast(record, label, *, require_bytes):
    expected_fail_fast = {
        "fail_fast": True,
        "requested_warmups": warmups,
        "attempted_warmups": warmups,
        "attempted_iterations": iterations,
        "stopped_on_failure": False,
    }
    for field, expected in expected_fail_fast.items():
        if record.get(field) != expected:
            issues.append(
                f"{label}: {field}={record.get(field)!r}, want {expected!r}"
            )
    check("failure_phase" not in record, f"{label}: unexpected failure_phase")
    check("failure" not in record, f"{label}: unexpected failure")
    warmup_samples = record.get("warmup_samples")
    check(
        isinstance(warmup_samples, list) and len(warmup_samples) == warmups,
        f"{label}: expected {warmups} warmup_samples",
    )
    if isinstance(warmup_samples, list):
        for index, sample in enumerate(warmup_samples):
            validate_success_sample(
                sample,
                f"{label}/warmup",
                index,
                require_bytes=require_bytes,
            )

records_by_fixture = {}
for name, fixture in (
    ("raw-direct.jsonl", "direct"),
    ("raw-protocol.jsonl", "beamd"),
):
    records = load(name)
    if len(records) != len(transports):
        issues.append(f"{name}: got {len(records)} records, want {len(transports)}")
    actual_order = [record.get("transport") for record in records]
    if actual_order != transports:
        issues.append(f"{name}: transport order {actual_order}, want {transports}")
    for index, transport in enumerate(transports, start=1):
        matches = [record for record in records if record.get("transport") == transport]
        if len(matches) != 1:
            issues.append(
                f"{name}: got {len(matches)} {transport} records, want exactly one"
            )
            continue
        record = matches[0]
        samples = record.get("samples")
        expected_concurrency = beamd_concurrency if fixture == "beamd" else 1
        expected = {
            "fixture": fixture,
            "workload": "protocol",
            "profile": profile,
            "seed": seed,
            "dir": direction,
            "size": size,
            "transport": transport,
            "order": ",".join(transports),
            "order_index": index,
            "iterations": iterations,
            "warmups": warmups,
            "concurrency": expected_concurrency,
            "handshake_included": False,
            "fail_fast": True,
            "requested_warmups": warmups,
            "attempted_warmups": warmups,
            "attempted_iterations": iterations,
            "stopped_on_failure": False,
            "errors": 0,
            "corrupt": 0,
        }
        mismatches = {
            key: (record.get(key), value)
            for key, value in expected.items()
            if record.get(key) != value
        }
        if mismatches:
            issues.append(f"{name}: {transport} record mismatch: {mismatches}")
        require_bytes = fixture == "beamd"
        if not isinstance(samples, list) or len(samples) != iterations:
            issues.append(f"{name}: {transport} has incomplete or unsuccessful samples")
        elif isinstance(samples, list):
            for sample_index, sample in enumerate(samples):
                validate_success_sample(
                    sample,
                    f"{name}/{transport}",
                    sample_index,
                    require_bytes=require_bytes,
                )
        validate_success_fail_fast(
            record, f"{name}/{transport}", require_bytes=require_bytes
        )
        if fixture == "beamd":
            check(
                record.get("bytes_semantics") == expected_bytes_semantics,
                f"{name}/{transport}: unexpected bytes_semantics",
            )
        else:
            check(
                record.get("wire_direction") == expected_direct_wire_direction,
                f"{name}/{transport}: unexpected wire_direction",
            )
            check(
                record.get("data_stream_initiator") == "edge",
                f"{name}/{transport}: unexpected data_stream_initiator",
            )
            check(
                finite_number(record.get("handshake_ms"))
                and record.get("handshake_ms") >= 0,
                f"{name}/{transport}: invalid handshake_ms",
            )
    records_by_fixture[fixture] = records

check(load("raw-mixed.jsonl") == [], "raw-mixed.jsonl must be empty in targeted mode")
check(load("bulk-live.jsonl") == [], "bulk-live.jsonl must be empty in targeted mode")

raw_failures = load("raw-failures.jsonl", required=False)
if raw_failures:
    issues.append(
        f"raw-failures.jsonl retained {len(raw_failures)} failed measurement record(s)"
    )
for failure_index, record in enumerate(raw_failures):
    label = f"raw-failures.jsonl/{failure_index}"
    fixture = record.get("fixture")
    failure_transport = record.get("transport")
    check(fixture in ("direct", "beamd"), f"{label}: invalid fixture")
    check(failure_transport in transports, f"{label}: invalid transport")
    check(record.get("fail_fast") is True, f"{label}: fail_fast must be true")
    check(
        record.get("requested_warmups") == warmups,
        f"{label}: requested_warmups must be {warmups}",
    )
    attempted_warmups = record.get("attempted_warmups")
    attempted_iterations = record.get("attempted_iterations")
    check(
        integer(attempted_warmups) and 1 <= attempted_warmups <= warmups,
        f"{label}: invalid attempted_warmups",
    )
    check(
        integer(attempted_iterations) and 0 <= attempted_iterations <= iterations,
        f"{label}: invalid attempted_iterations",
    )
    check(
        record.get("stopped_on_failure") is True,
        f"{label}: stopped_on_failure must be true",
    )
    failure_phase = record.get("failure_phase")
    check(
        failure_phase in ("warmup", "measurement"),
        f"{label}: invalid failure_phase",
    )
    warmup_samples = record.get("warmup_samples")
    samples = record.get("samples")
    check(
        isinstance(warmup_samples, list)
        and integer(attempted_warmups)
        and len(warmup_samples) == attempted_warmups,
        f"{label}: warmup sample count mismatch",
    )
    check(
        isinstance(samples, list)
        and integer(attempted_iterations)
        and len(samples) == attempted_iterations,
        f"{label}: measurement sample count mismatch",
    )
    failed_samples = warmup_samples if failure_phase == "warmup" else samples
    if isinstance(failed_samples, list) and failed_samples:
        failed_candidates = [
            sample
            for sample in failed_samples
            if isinstance(sample, dict)
            and (
                sample.get("ok") is not True
                or bool(sample.get("err"))
                or sample.get("corrupt") is True
            )
        ]
        check(
            bool(failed_candidates),
            f"{label}: fail-fast phase retained no failed sample",
        )
        check(
            record.get("failure") in failed_candidates,
            f"{label}: failure does not match a failed phase sample",
        )
    if failure_phase == "warmup":
        check(attempted_iterations == 0, f"{label}: warmup failure attempted measurements")
    elif failure_phase == "measurement":
        check(attempted_warmups == warmups, f"{label}: measurement began before all warmups")
        check(
            integer(attempted_iterations) and attempted_iterations >= 1,
            f"{label}: measurement failure attempted no measurements",
        )
    check(
        integer(record.get("errors"))
        and integer(record.get("corrupt"))
        and record.get("errors") + record.get("corrupt") >= 1,
        f"{label}: failed record has no error/corruption count",
    )
    if fixture == "beamd":
        check(
            record.get("bytes_semantics") == expected_bytes_semantics,
            f"{label}: unexpected bytes_semantics",
        )
        for phase_name, phase_samples in (
            ("warmup", warmup_samples),
            ("measurement", samples),
        ):
            if not isinstance(phase_samples, list):
                continue
            for sample_index, sample in enumerate(phase_samples):
                if not isinstance(sample, dict):
                    issues.append(
                        f"{label}/{phase_name}/{sample_index}: sample is not an object"
                    )
                    continue
                check(
                    integer(sample.get("bytes"))
                    and 0 <= sample.get("bytes") <= size,
                    f"{label}/{phase_name}/{sample_index}: invalid partial bytes",
                )
                check(
                    finite_number(sample.get("elapsed_ms"))
                    and sample.get("elapsed_ms") > 0,
                    f"{label}/{phase_name}/{sample_index}: invalid elapsed_ms",
                )
passed = not issues
(root / "targeted-summary.json").write_text(
    json.dumps(
        {
            "passed": passed,
            "profile": profile,
            "seed": seed,
            "direction": direction,
            "size_bytes": size,
            "transport_order": transports,
            "beamd_concurrency": beamd_concurrency,
            "warmups_per_fixture_transport": warmups,
            "iterations_per_fixture_transport": iterations,
            "collection_status": collection_status,
            "stage_results": stage_results,
            "raw_failures": raw_failures,
            "records": records_by_fixture,
            "issues": issues,
        },
        indent=2,
    )
    + "\n",
    encoding="utf-8",
)
if not passed:
    for issue in issues:
        print(f"targeted verification: {issue}", file=sys.stderr)
    raise SystemExit(1)
PY
  then
    targeted_verify_status=0
  else
    targeted_verify_status=$?
  fi
  if [ "$TARGETED_RUN_STATUS" -ne 0 ] || [ "$targeted_verify_status" -ne 0 ]; then
    echo "TARGETED CHECK FAILED — non-qualification evidence retained in $OUTDIR" >&2
    exit 1
  fi
  echo "TARGETED CHECK COMPLETE — non-qualification evidence in $OUTDIR"
  exit 0
fi

if [ "$MODE" = smoke ]; then
  verify_immutable_inputs
  verify_traffic_control
  [ "$(sha256_file "$OUTDIR/traffic-control/tc")" = "$TC_SHA256" ] || {
    echo "recorded tc binary changed while producing smoke evidence" >&2
    exit 2
  }
  echo "SMOKE COMPLETE — non-qualification evidence in $OUTDIR"
  exit 0
fi

verify_immutable_inputs
verify_traffic_control
python3 "$BINDIR/b4_analyze.py" "$OUTDIR" --summary "$OUTDIR/summary.json" |
  tee "$OUTDIR/analysis.txt"
verify_immutable_inputs
verify_traffic_control
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
[ "$(sha256_file "$OUTDIR/traffic-control/tc")" = "$TC_SHA256" ] || {
  echo "recorded tc binary changed while producing the verdict" >&2
  exit 2
}
echo "QUALIFICATION COMPLETE — evidence in $OUTDIR"
