#!/usr/bin/env bash
# End-to-end without a phone: pair once with a code, then reach the "phone" with no code
# after it restarts, including when the agent asks before the phone is open.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/bin/ambivalence"
cd "$ROOT"
go build -buildvcs=false -o "$BIN" ./cmd/ambivalence
export XDG_CONFIG_HOME="$(mktemp -d)" # never touch the real agent link
LISTEN="127.0.0.1:17842"
HUB="ws://$LISTEN/v1"
STATE="$XDG_CONFIG_HOME/fake-phone.json" # the fake phone's storage: device id + link key
PIDS=()
cleanup() { kill "${PIDS[@]}" 2>/dev/null || true; rm -rf "$XDG_CONFIG_HOME"; }
trap cleanup EXIT

"$BIN" hub --listen "$LISTEN" 2>/dev/null & PIDS+=($!)
for _ in $(seq 50); do curl -fsS "http://$LISTEN/health" >/dev/null 2>&1 && break; sleep 0.1; done

phone() { # starts the fake phone; sets PHONE (pid) and CODE (what the hub assigned)
  local log; log=$(mktemp)
  "$BIN" host fake --hub "$HUB" --state "$STATE" >"$log" 2>/dev/null &
  PHONE=$!; PIDS+=("$PHONE")
  for _ in $(seq 50); do grep -q '^code ' "$log" && break; sleep 0.1; done
  CODE=$(awk '/^code /{print $2}' "$log"); rm -f "$log"
}
jpeg() { python3 -c "import sys; d=open(sys.argv[1],'rb').read(); assert d[:2]==b'\xff\xd8', 'not jpeg'; print('jpeg ok', len(d), 'bytes')" "$1"; }

echo "1. first time: pair with the code"
phone
"$BIN" pair --hub "$HUB" "$CODE"

echo "2. phone app restarts; agent captures with no code"
kill "$PHONE"; sleep 0.3
phone
"$BIN" capture --hub "$HUB" -o $XDG_CONFIG_HOME/amb-e2e-1.jpg 2>/dev/null; jpeg $XDG_CONFIG_HOME/amb-e2e-1.jpg

echo "3. agent asks first, phone opens 2s later"
kill "$PHONE"; sleep 0.3
"$BIN" capture --hub "$HUB" -o $XDG_CONFIG_HOME/amb-e2e-2.jpg 2>/dev/null & CAP=$!
sleep 2; phone
wait "$CAP"; jpeg $XDG_CONFIG_HOME/amb-e2e-2.jpg
rm -f $XDG_CONFIG_HOME/amb-e2e-*.jpg
echo e2e-fake ok
