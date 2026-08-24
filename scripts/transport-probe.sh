#!/usr/bin/env bash
# Low-volume, external TCP/QUIC availability + handshake probe.
# Organic request effectiveness is recorded by request_event.transport; this
# probe supplies the controlled, same-machine handshake comparison that organic
# fallback traffic cannot provide.
set -euo pipefail

usage() {
  cat <<'EOF'
usage: transport-probe.sh --server HOST [--server HOST ...] [options]

Options:
  --scope SCOPE   account scope passed to beamd check
  --output FILE   NDJSON output (default: ~/.beamd/transport-probes.ndjson)
  -h, --help      show this help

Environment:
  BEAMD_BIN               beamd executable (default: beamd)
  BEAMD_PROBE_MAX_BYTES   rotate output at this size (default: 10485760)
EOF
}

servers=()
scope=""
output="${HOME}/.beamd/transport-probes.ndjson"
beamd_bin="${BEAMD_BIN:-beamd}"
max_bytes="${BEAMD_PROBE_MAX_BYTES:-10485760}"

while (($#)); do
  case "$1" in
    --server)
      [[ $# -ge 2 ]] || { usage >&2; exit 2; }
      servers+=("$2")
      shift 2
      ;;
    --scope)
      [[ $# -ge 2 ]] || { usage >&2; exit 2; }
      scope="$2"
      shift 2
      ;;
    --output)
      [[ $# -ge 2 ]] || { usage >&2; exit 2; }
      output="$2"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

[[ ${#servers[@]} -gt 0 ]] || { usage >&2; exit 2; }
[[ "$max_bytes" =~ ^[1-9][0-9]*$ ]] || {
  echo "BEAMD_PROBE_MAX_BYTES must be a positive integer" >&2
  exit 2
}
command -v "$beamd_bin" >/dev/null 2>&1 || {
  echo "beamd executable not found: $beamd_bin" >&2
  exit 2
}
command -v jq >/dev/null 2>&1 || {
  echo "jq is required" >&2
  exit 2
}

mkdir -p "$(dirname "$output")"
if [[ -f "$output" ]] && (( $(wc -c < "$output") >= max_bytes )); then
  mv -f "$output" "$output.1"
fi

failed=0
for server in "${servers[@]}"; do
  for transport in tcp quic; do
    checked_at="$(date -u +"%Y-%m-%dT%H:%M:%SZ")"
    args=(check --server "$server" --transport "$transport" --json)
    [[ -z "$scope" ]] || args+=(--scope "$scope")

    stderr_file="$(mktemp "${TMPDIR:-/tmp}/beamd-transport-probe.XXXXXX")"
    set +e
    result="$("$beamd_bin" "${args[@]}" 2>"$stderr_file")"
    exit_code=$?
    set -e
    probe_stderr="$(<"$stderr_file")"
    rm -f "$stderr_file"
    (( exit_code == 0 )) || failed=1

    if ! printf '%s\n' "$result" | jq -ce \
      --arg checkedAt "$checked_at" \
      --arg requestedTransport "$transport" \
      --arg probeServer "$server" \
      --argjson exitCode "$exit_code" \
      '. + {
        checkedAt: $checkedAt,
        requestedTransport: $requestedTransport,
        probeServer: $probeServer,
        exitCode: $exitCode
      }' >> "$output" 2>/dev/null; then
      error="$result"
      if [[ -n "$probe_stderr" ]]; then
        error="${probe_stderr}${result:+$'\n'}${result}"
      fi
      jq -nc \
        --arg checkedAt "$checked_at" \
        --arg requestedTransport "$transport" \
        --arg probeServer "$server" \
        --arg error "$error" \
        --argjson exitCode "$exit_code" \
        '{
          ok: false,
          checkedAt: $checkedAt,
          requestedTransport: $requestedTransport,
          probeServer: $probeServer,
          exitCode: $exitCode,
          error: $error
        }' >> "$output"
      failed=1
    fi
  done
done

exit "$failed"
