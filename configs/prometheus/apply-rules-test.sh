#!/usr/bin/env bash
# apply-rules-test.sh — fixture tests for configs/prometheus/apply-rules.sh's
# --live-check mode.
#
# Nothing between deploys re-checks that the LIVE rule set matches the repo;
# --live-check does that for a scheduled reconciliation run. Tests use a
# loopback mock of /api/v1/rules, never a live host.
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
      - record: fixture:up_ratio:5m
        expr: avg(up)
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

# Live set matches the repo exactly, including the recording rule: live-check OK.
# A codified `record:` must not read as "loaded but NOT in the repo".
cat >"$TMP/matching.json" <<'EOF'
{"status":"success","data":{"groups":[{"name":"fixture","rules":[
  {"name":"FixtureAlertA","health":"ok"},
  {"name":"FixtureAlertB","health":"ok"},
  {"name":"fixture:up_ratio:5m","health":"ok"}
]}]}}
EOF
start_mock "$TMP/matching.json"
OUT="$(PROM_URL="http://127.0.0.1:$MOCK_PORT" bash "$SCRIPT" --live-check "$SRC" 2>&1)"; RC=$?
stop_mock
expect 'live matches repo (alerts + recording rule) → OK' 0
if grep -q 'fixture:up_ratio:5m' <<<"$OUT"; then
  echo "FAIL: a codified recording rule was reported as drift" >&2; fail=$((fail + 1))
else
  echo "ok: codified recording rule not reported as drift"; pass=$((pass + 1))
fi

# Live set is missing an alert the repo declares (repo moved on, host did not).
cat >"$TMP/missing.json" <<'EOF'
{"status":"success","data":{"groups":[{"name":"fixture","rules":[
  {"name":"FixtureAlertA","health":"ok"},
  {"name":"fixture:up_ratio:5m","health":"ok"}
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
  {"name":"fixture:up_ratio:5m","health":"ok"},
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

# ─── Install + verify path ───
# A stateful loopback Prometheus: serves $STATE/rules.json and $STATE/query.json;
# the fake `systemctl reload` swaps in, after a delay, the *.after.json pair, the way a reload
# replaces the old rule set. flock/systemctl/chown are stubbed; nothing real is touched.
BIN="$TMP/bin"
mkdir -p "$BIN"
printf '#!/bin/sh\nexit 0\n' >"$BIN/flock"
printf '#!/bin/sh\nexit 0\n' >"$BIN/chown"
printf '#!/bin/sh\nexit 0\n' >"$BIN/find" # backup pruning uses GNU find -printf
cat >"$BIN/systemctl" <<'EOF'
#!/bin/sh
[ -n "$STATE" ] && [ -f "$STATE/rules.after.json" ] || exit 0
# Reload is async: the old set keeps being served for a moment.
( sleep 2; cp "$STATE/rules.after.json" "$STATE/rules.json"; cp "$STATE/query.after.json" "$STATE/query.json" ) >/dev/null 2>&1 &
EOF
chmod +x "$BIN/flock" "$BIN/chown" "$BIN/find" "$BIN/systemctl"

start_state_mock() {
  python3 - "$STATE" >"$TMP/mock.log" 2>&1 <<'PY' &
import http.server, socketserver, sys

state = sys.argv[1]

class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        name = "query.json" if self.path.startswith("/api/v1/query") else "rules.json"
        body = open(f"{state}/{name}", "rb").read()
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
  [ -n "$port" ] || { echo "FAIL: state mock never reported a port" >&2; kill "$MOCK_PID" 2>/dev/null; exit 1; }
  MOCK_PORT="$port"
}

ts_body() { printf '{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"%s"]}]}}' "$1"; }

# new_install_fixture: fresh SRC2 (alerts.yml changed vs DEST, other.yml identical) and DEST.
new_install_fixture() {
  rm -rf "$TMP/src2" "$TMP/dest" "$TMP/state"
  mkdir -p "$TMP/src2" "$TMP/dest" "$TMP/state"
  cp "$SRC/alerts.yml" "$TMP/src2/alerts.yml"
  cat >"$TMP/src2/other.yml" <<'EOF'
groups:
  - name: other
    rules:
      - alert: OtherAlert
        expr: up == 0
        for: 5m
        labels: {severity: page}
        annotations: {summary: "fixture"}
EOF
  cp "$TMP/src2/other.yml" "$TMP/dest/other.yml"
  printf 'groups: []\n' >"$TMP/dest/alerts.yml"
  STATE="$TMP/state"
}

run_install() {
  OUT="$(PATH="$BIN:$PATH" STATE="$STATE" RULES_DEST="$TMP/dest" VERIFY_TIMEOUT_S=4 \
    PROM_URL="http://127.0.0.1:$MOCK_PORT" bash "$SCRIPT" "$TMP/src2" 2>&1)"; RC=$?
}

rules_json() { # $1 = health of FixtureAlertA, $2 = health of OtherAlert
  cat <<EOF
{"status":"success","data":{"groups":[
 {"name":"fixture","file":"$TMP/dest/alerts.yml","interval":30,"rules":[
  {"name":"FixtureAlertA","health":"$1"},{"name":"FixtureAlertB","health":"ok"},{"name":"fixture:up_ratio:5m","health":"ok"}]},
 {"name":"other","file":"$TMP/dest/other.yml","interval":30,"rules":[{"name":"OtherAlert","health":"$2"}]}]}}
EOF
}

# (a) Name-preserving edit that breaks at runtime: the OLD rule set (all ok) is
# served until the reload lands, so a poll without a reload barrier verifies the
# stale body and reports a broken apply as good.
new_install_fixture
rules_json ok ok >"$STATE/rules.json"; ts_body 100 >"$STATE/query.json"
rules_json err ok >"$STATE/rules.after.json"; ts_body 200 >"$STATE/query.after.json"
start_state_mock
run_install
stop_mock
expect 'rule that errors only after the reload lands → rolled back' 1

# (b) An error in a file this apply did not change must not roll back an
# unrelated deploy.
new_install_fixture
rules_json ok err >"$STATE/rules.json"; ts_body 100 >"$STATE/query.json"
rules_json ok err >"$STATE/rules.after.json"; ts_body 200 >"$STATE/query.after.json"
start_state_mock
run_install
stop_mock
expect 'errored rule in an unchanged file → deploy still succeeds' 0
if grep -q 'OtherAlert' <<<"$OUT"; then
  echo "ok: pre-existing error is reported as a warning"; pass=$((pass + 1))
else
  echo "FAIL: pre-existing error was not surfaced" >&2; fail=$((fail + 1))
fi

# (c) A reload that never completes (timestamp never advances) fails and restores.
new_install_fixture
rules_json ok ok >"$STATE/rules.json"; ts_body 100 >"$STATE/query.json"
start_state_mock
run_install
stop_mock
expect 'reload that never advances the timestamp → rolled back' 1
if [ "$(cat "$TMP/dest/alerts.yml")" = "groups: []" ]; then
  echo "ok: destination restored after the failed reload"; pass=$((pass + 1))
else
  echo "FAIL: destination not restored after the failed reload" >&2; fail=$((fail + 1))
fi

echo "---"
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
