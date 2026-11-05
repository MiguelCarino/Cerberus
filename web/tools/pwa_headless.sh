#!/usr/bin/env bash
# Runs web/test/pwa-headless.test.mjs and energy-headless.test.mjs against a
# throwaway daemon: builds cerberus, starts it on a free loopback port with
# temp HOME/XDG folders and
# every engine disabled (nothing is loaded, the user's install is never
# touched), runs the test with one headless Chromium, then stops the daemon
# and removes the temp folder.
#
#   PLAYWRIGHT=/path/to/node_modules/playwright web/tools/pwa_headless.sh
#   PORT=7397 to pick another port (default 7381; 7340 and 7396 are refused).
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
PORT=${PORT:-7381}
PLAYWRIGHT=${PLAYWRIGHT:-}
if [[ -z "$PLAYWRIGHT" || ! -f "$PLAYWRIGHT/index.mjs" ]]; then
  echo "pwa_headless: set PLAYWRIGHT to a playwright package folder; skipping" >&2
  exit 0
fi
if [[ "$PORT" == 7340 || "$PORT" == 7396 ]]; then
  echo "pwa_headless: port $PORT is reserved (installed Cerberus / Folding@home)" >&2
  exit 2
fi

TMP=$(mktemp -d "${TMPDIR:-/tmp}/cerberus-pwa-headless.XXXXXX")
PID=""
cleanup() {
  if [[ -n "$PID" ]]; then kill "$PID" 2>/dev/null || true; wait "$PID" 2>/dev/null || true; fi
  rm -rf "$TMP"
}
trap cleanup EXIT

mkdir -p "$TMP/home" "$TMP/config/cerberus" "$TMP/data"
(cd "$ROOT" && CGO_ENABLED=0 go build -o "$TMP/cerberus" ./cmd/cerberus)
TOKEN=headless-$RANDOM$RANDOM
cat >"$TMP/config/cerberus/config.json" <<JSON
{
  "server": {"listen": "127.0.0.1:$PORT", "token": "$TOKEN", "init_done": true},
  "engines": {"llamacpp": {"enabled": false}, "lmstudio": {"enabled": false}, "ollama": {"enabled": false}},
  "notify": {"disabled": true}
}
JSON

HOME="$TMP/home" XDG_CONFIG_HOME="$TMP/config" XDG_DATA_HOME="$TMP/data" \
  "$TMP/cerberus" serve -config "$TMP/config/cerberus/config.json" >"$TMP/serve.log" 2>&1 &
PID=$!
for _ in $(seq 1 50); do
  if curl -fsS "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1; then break; fi
  if ! kill -0 "$PID" 2>/dev/null; then cat "$TMP/serve.log" >&2; exit 1; fi
  sleep 0.2
done

CERBERUS_HEADLESS_URL="http://127.0.0.1:$PORT" CERBERUS_HEADLESS_TOKEN="$TOKEN" CERBERUS_PLAYWRIGHT="$PLAYWRIGHT" \
  node --test "$ROOT/web/test/pwa-headless.test.mjs" "$ROOT/web/test/energy-headless.test.mjs"
