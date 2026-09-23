#!/usr/bin/env bash
# apply-rules-test.sh — fixture tests for configs/prometheus/apply-rules.sh's
# --live-check mode (T677).
#
# T677: nothing between deploys re-checks that the LIVE rule set still
# matches the repo — --check-only only validates repo content, and the
# post-install verify poll only runs during an install. --live-check closes
# that gap for a scheduled reconciliation run. These tests prove it against
# a loopback mock of Prometheus's /api/v1/rules, never a live host.
#
# Run: bash configs/prometheus/apply-rules-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
SCRIPT="$PWD/configs/prometheus/apply-rules.sh"

command -v promtool >/dev/null 2>&1 || { echo "SKIP: promtool not on PATH"; exit 0; }
command -v python3 >/dev/null 2>&1 || { echo "SKIP: python3 not on PATH"; exit 0; }

TMP="$(mktemp -d)"
SRC="$TMP/rules.r1"
mkdir -p "$SRC"
cat >"$SRC/alerts.yml" <<'EOF'
groups:
  - name: fixture
    rules:
      - alert: FixtureAlertA
        expr: up == 0
        for: 5m
        labels: {severity: page}
        annotations: {summary: "fixture"}
      - alert: FixtureAlertB
        expr: up == 0
        for: 5m
        labels: {severity: page}
        annotations: {summary: "fixture"}
EOF

# Serves a fixed /api/v1/rules body on loopback, port chosen by the OS.
start_mock() {
  local body_file="$1"
  python3 - "$body_file" >"$TMP/mock.log" 2>&1 <<'PY' &
import http.server, socketserver, sys, threading

body = open(sys.argv[1], "rb").read()

class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *a):
        pass

with socketserver.TCPServer(("127.0.0.1", 0), H) as srv:
    print(f"PORT={srv.server_address[1]}", flush=True)
    srv.serve_forever()
PY
  MOCK_PID=$!
  local port=""
  for _ in $(seq 1 50); do
    port="$(grep -oE 'PORT=[0-9]+' "$TMP/mock.log" 2>/dev/null | sed -n '1p' | cut -d= -f2)"
    [ -n "$port" ] && break
    sleep 0.1
  done
  [ -n "$port" ] || { echo "FAIL: mock server never reported a port" >&2; kill "$MOCK_PID" 2>/dev/null; exit 1; }
  MOCK_PORT="$port"
}

stop_mock() { kill "$MOCK_PID" 2>/dev/null; wait "$MOCK_PID" 2>/dev/null; }
trap 'stop_mock; rm -rf "$TMP"' EXIT

pass=0
fail=0
expect() {
  local name="$1" want_rc="$2"
  if [ "$RC" -ne "$want_rc" ]; then
    echo "FAIL: $name — exit $RC, want $want_rc" >&2
    printf '%s\n' "$OUT" | sed 's/^/    /' >&2
    fail=$((fail + 1)); return
  fi
  echo "ok: $name"
  pass=$((pass + 1))
}

# Live set matches the repo exactly: live-check OK.
cat >"$TMP/matching.json" <<'EOF'
{"status":"success","data":{"groups":[{"name":"fixture","rules":[
  {"name":"FixtureAlertA","health":"ok"},
  {"name":"FixtureAlertB","health":"ok"}
]}]}}
EOF
start_mock "$TMP/matching.json"
OUT="$(PROM_URL="http://127.0.0.1:$MOCK_PORT" bash "$SCRIPT" --live-check "$SRC" 2>&1)"; RC=$?
stop_mock
expect 'live matches repo → OK' 0

# Live set is missing an alert the repo declares (the T677 scenario: repo
# has moved on, the host has not). This is the regression proof — on the
# unfixed script there is no --live-check flag at all, so this fails loud
# with "unbound variable"/usage error rather than detecting the drift.
cat >"$TMP/missing.json" <<'EOF'
{"status":"success","data":{"groups":[{"name":"fixture","rules":[
  {"name":"FixtureAlertA","health":"ok"}
]}]}}
EOF
start_mock "$TMP/missing.json"
OUT="$(PROM_URL="http://127.0.0.1:$MOCK_PORT" bash "$SCRIPT" --live-check "$SRC" 2>&1)"; RC=$?
stop_mock
expect 'live missing an expected alert → DRIFT' 1
if ! grep -q 'FixtureAlertB' <<<"$OUT"; then
  echo "FAIL: drift message does not name the missing alert" >&2; fail=$((fail + 1))
else
  echo "ok: drift message names the missing alert"; pass=$((pass + 1))
fi

# Live set has an extra alert the repo no longer declares (out-of-band host
# change, or a deploy that failed to prune a removed rule file).
cat >"$TMP/extra.json" <<'EOF'
{"status":"success","data":{"groups":[{"name":"fixture","rules":[
  {"name":"FixtureAlertA","health":"ok"},
  {"name":"FixtureAlertB","health":"ok"},
  {"name":"StaleAlertC","health":"ok"}
]}]}}
EOF
start_mock "$TMP/extra.json"
OUT="$(PROM_URL="http://127.0.0.1:$MOCK_PORT" bash "$SCRIPT" --live-check "$SRC" 2>&1)"; RC=$?
stop_mock
expect 'live has an extra alert not in repo → DRIFT' 1
if ! grep -q 'StaleAlertC' <<<"$OUT"; then
  echo "FAIL: drift message does not name the extra alert" >&2; fail=$((fail + 1))
else
  echo "ok: drift message names the extra alert"; pass=$((pass + 1))
fi

echo "---"
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
