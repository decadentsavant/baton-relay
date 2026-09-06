#!/usr/bin/env bash
# Wave at yourself, and watch a baton get minted and passed. Starts a local
# relay with a 5s cooldown, opens two streams under different identities, and
# bounces a wave between them - which is the whole protocol, batons included,
# without a second machine or a real stranger on the far side.
set -euo pipefail

RELAY="${RELAY:-http://127.0.0.1:8080}"
BIN="${BIN:-$(dirname "$0")/../baton-relay}"

if [[ ! -x $BIN ]]; then
  echo "build the relay first:  go build -o baton-relay ." >&2
  exit 1
fi

state=$(mktemp -d)/state.json
"$BIN" -addr 127.0.0.1:8080 -cooldown 5s -trusted-proxies 127.0.0.1/32 -state "$state" &
relay_pid=$!
pids=($relay_pid)
trap 'kill "${pids[@]}" 2>/dev/null || true' EXIT
sleep 0.4

echo "--- alice and bob listening ---"
curl -fsS -N --no-buffer -H 'X-Baton-Id: alicealicealice' "$RELAY/stream" | sed 's/^/alice < /' &
pids+=($!)
curl -fsS -N --no-buffer -H 'X-Baton-Id: bobbobbobbobbob' "$RELAY/stream" | sed 's/^/bob   < /' &
pids+=($!)
sleep 0.5

echo
echo "--- bob waves (two clients online, so this mints a baton at 1 hop) ---"
curl -fsS -X POST -H 'X-Baton-Id: bobbobbobbobbob' -H 'X-Country: SE' "$RELAY/wave"
sleep 0.5

echo
echo "--- bob waves again immediately (expect cooldown, no delivery) ---"
curl -fsS -X POST -H 'X-Baton-Id: bobbobbobbobbob' -H 'X-Country: SE' "$RELAY/wave"
sleep 5

echo
echo "--- alice passes the baton back (expect hops=2, countries=2) ---"
curl -fsS -X POST -H 'X-Baton-Id: alicealicealice' -H 'X-Country: JP' "$RELAY/wave"
sleep 0.5

echo
echo "--- every baton that has ever lived ---"
curl -fsS "$RELAY/batons"; echo
echo "--- stats ---"
curl -fsS "$RELAY/stats"; echo
