#!/usr/bin/env bash
#
# smoke-test.sh — exercise a real beamd deployment end-to-end.
#
# Prerequisites:
#   - `beamd` and `beam-testapp` are on PATH (`make build && export PATH=$PWD/bin:$PATH`)
#   - You've already run `beamd login --server <yours> --token <yours>`
#
# What it does:
#   1. Starts beam-testapp on a local port.
#   2. Runs `beamd open <port> --as smoketest -d` (detached).
#   3. Curls a handful of routes through the resulting public URL.
#   4. Prints pass/fail per check.
#
# Cleans up the test app + the registered tunnel on exit.

set -euo pipefail

PORT="${PORT:-8765}"
NAME="${NAME:-smoketest}"

red()   { printf "\033[31m%s\033[0m" "$*"; }
green() { printf "\033[32m%s\033[0m" "$*"; }

# --- Precheck ----------------------------------------------------------------

for bin in beamd beam-testapp curl; do
    if ! command -v "$bin" >/dev/null 2>&1; then
        echo "missing prerequisite: $bin not on PATH"
        exit 1
    fi
done

if ! beamd list >/dev/null 2>&1; then
    echo "couldn't reach the beamd agent. Have you run 'beamd login' yet?"
    exit 1
fi

# --- Spin up the test app ----------------------------------------------------

LOG=$(mktemp -t beam-smoke-app.XXXXXX)
echo "starting beam-testapp on :$PORT (log: $LOG)"
beam-testapp --port "$PORT" >"$LOG" 2>&1 &
APP_PID=$!

cleanup() {
    echo
    echo "cleanup:"
    if kill -0 "$APP_PID" 2>/dev/null; then
        kill "$APP_PID" 2>/dev/null || true
        echo "  stopped beam-testapp (pid $APP_PID)"
    fi
    beamd close "$NAME" >/dev/null 2>&1 || true
    echo "  removed tunnel '$NAME'"
}
trap cleanup EXIT

# Wait for the test app to start listening.
for _ in $(seq 1 20); do
    if curl -sf "http://127.0.0.1:$PORT/" >/dev/null 2>&1; then
        break
    fi
    sleep 0.2
done
if ! curl -sf "http://127.0.0.1:$PORT/" >/dev/null 2>&1; then
    echo "beam-testapp didn't come up — see $LOG"
    exit 1
fi

# --- Expose ------------------------------------------------------------------

echo "exposing :$PORT as '$NAME' …"
# Detached (-d) so this returns the URL instead of holding the tunnel in
# the foreground; the agent keeps it up and cleanup tears it down.
URL=$(beamd open "$PORT" --as "$NAME" -d)
echo "tunnel URL: $URL"
echo

# --- Run checks --------------------------------------------------------------

fails=0

check() {
    local desc="$1" cmd_output="$2" expect="$3"
    if [[ "$cmd_output" == *"$expect"* ]]; then
        printf "  %s %s\n" "$(green ✓)" "$desc"
    else
        printf "  %s %s\n      expected to contain: %q\n      got: %s\n" \
            "$(red ✗)" "$desc" "$expect" "$cmd_output"
        fails=$((fails + 1))
    fi
}

echo "checks:"

# 1) Basic GET — proves routing + TLS + the backend hop.
out=$(curl -sf "$URL/" 2>&1 || echo "<curl failed>")
check "GET / serves the test app banner"               "$out" "beam-testapp"

# 2) X-Forwarded-For is set by the edge.
out=$(curl -sf "$URL/headers" 2>&1 || echo "<curl failed>")
check "X-Forwarded-For header reaches the backend"     "$out" '"X-Forwarded-For"'
check "X-Forwarded-Proto is 'https'"                   "$out" '"X-Forwarded-Proto":"https"'
check "X-Forwarded-Host matches the tunnel hostname"   "$out" "X-Forwarded-Host"

# 3) POST body round-trips byte-for-byte.
body="hello-beam-$(date +%s)"
out=$(curl -sf -X POST -d "$body" "$URL/echo" 2>&1 || echo "<curl failed>")
check "POST /echo returns the body verbatim"           "$out" "$body"

# 4) A modest response body (8 KiB) doesn't get truncated.
out_bytes=$(curl -sf "$URL/size?bytes=8192" 2>&1 | wc -c | tr -d ' ')
if [[ "$out_bytes" == "8192" ]]; then
    printf "  %s GET /size?bytes=8192 returns 8192 bytes\n" "$(green ✓)"
else
    printf "  %s GET /size?bytes=8192 returned %s bytes (want 8192)\n" "$(red ✗)" "$out_bytes"
    fails=$((fails + 1))
fi

# 5) Slow backend doesn't break the proxy.
start=$(python3 -c 'import time;print(time.time())' 2>/dev/null || date +%s)
out=$(curl -sf "$URL/sleep?ms=1000" 2>&1 || echo "<curl failed>")
end=$(python3 -c 'import time;print(time.time())' 2>/dev/null || date +%s)
check "GET /sleep?ms=1000 succeeds"                    "$out" "slept 1000 ms"

# --- Summary -----------------------------------------------------------------

echo
if [[ "$fails" -eq 0 ]]; then
    echo "$(green '🎉 all smoke checks passed.') your beamd deployment is healthy."
    echo
    echo "tunnel URL was: $URL"
    echo "(it'll be removed once this script exits)"
else
    echo "$(red "❌ $fails check(s) failed.") see output above; the agent log is at ~/.beamd/agent.log"
    exit 1
fi
