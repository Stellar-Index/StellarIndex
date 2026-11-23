#!/usr/bin/env bash
# edge-probe-test.sh — configs/healthchecks/edge-probe.sh decides pass/fail
# correctly and never leaks its ping URL. No network: `curl` and `openssl` are
# stubs on PATH that replay scripted responses and record every call.
#
# Also pins the role wiring structurally: the install tasks are gated on
# edge_probe_targets, the env file is root 0600 behind no_log, and the ping URL
# reaches only that env file.
#
# EDGE_PROBE_SCRIPT overrides the script under test (red proofs run it against
# a mutated copy). Run: bash scripts/ci/edge-probe-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
SCRIPT="${EDGE_PROBE_SCRIPT:-$PWD/configs/healthchecks/edge-probe.sh}"
TASKS="configs/ansible/roles/archival-node/tasks/17-stellarindex-healthchecks.yml"
ENVTPL="configs/ansible/roles/archival-node/templates/stellarindex-edge-probe.env.j2"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0; fail=0
ok()  { pass=$((pass + 1)); echo "  ok   — $1"; }
bad() { fail=$((fail + 1)); echo "  FAIL — $1"; }

SECRET="https://hc.example.invalid/ping/00000000-fake-0000"   # gitleaks:allow
mkdir -p "$TMP/bin"

# curl stub. Probe calls: pops the next "code rc" line for the path from
# $STUB/<readyz|data>.seq (the last line repeats), writes $STUB/data.body to -o.
# Ping calls (-K -): the config on stdin is appended to $STUB/ping.log.
cat >"$TMP/bin/curl" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB/argv.log"
if [[ " $* " == *" -K "* ]]; then
  cat >>"$STUB/ping.log"
  exit "${STUB_PING_RC:-0}"
fi
out=""; url=""
while [ $# -gt 0 ]; do
  case "$1" in -o) out="$2"; shift ;; -*) ;; *) url="$1" ;; esac
  shift
done
case "$url" in */v1/readyz) key=readyz ;; *) key=data ;; esac
seq="$STUB/$key.seq"
line="$(head -1 "$seq")"
if [ "$(wc -l <"$seq")" -gt 1 ]; then sed 1d "$seq" >"$seq.n"; mv "$seq.n" "$seq"; fi
code="${line% *}"; rc="${line#* }"
[ -z "$out" ] || cp "$STUB/data.body" "$out"
printf '%s' "$code"
exit "$rc"
EOF
cat >"$TMP/bin/openssl" <<'EOF'
#!/usr/bin/env bash
case "$1" in
  s_client) [ "${STUB_NOCERT:-0}" = 1 ] || echo "-----BEGIN CERTIFICATE-----" ;;
  x509) if [[ " $* " == *" -checkend "* ]]; then cat >/dev/null; exit "${STUB_X509_RC:-0}"; fi
        cat ;;
esac
EOF
chmod +x "$TMP/bin/curl" "$TMP/bin/openssl"

# run_case <name> <readyz-seq> <data-seq> [VAR=val...]; sets PING, OUT, ERR, RC.
run_case() {
  local name="$1" ready="$2" data="$3"; shift 3
  export STUB="$TMP/$name"
  mkdir -p "$STUB"
  printf '%b' "$ready" >"$STUB/readyz.seq"
  printf '%b' "$data" >"$STUB/data.seq"
  printf '%s' "${STUB_BODY:-{\"data\":[{\"asset_id\":\"native\"}]\}}" >"$STUB/data.body"
  : >"$STUB/ping.log"; : >"$STUB/argv.log"
  env PATH="$TMP/bin:$PATH" EDGE_PROBE_NAME=t EDGE_PROBE_BASE_URL=https://edge.example.invalid \
    EDGE_PROBE_PING_URL="$SECRET" EDGE_PROBE_RETRY_DELAY=0 STUB="$STUB" "$@" \
    bash "$SCRIPT" >"$STUB/out" 2>"$STUB/err"
  RC=$?
  PING="$(cat "$STUB/ping.log")"; ERR="$(cat "$STUB/err")"
}

expect_pass() {
  if [[ "$PING" == *"url = \"$SECRET\""* && "$PING" != *"/fail"* && "$RC" -eq 0 ]]; then ok "$1"
  else bad "$1 (rc=$RC ping=[$PING])"; fi
}
expect_fail() { # <name> <reason-substring>
  if [[ "$PING" == *"url = \"$SECRET/fail\""* && "$PING" == *"$2"* && "$RC" -eq 0 ]]; then ok "$1"
  else bad "$1 (rc=$RC ping=[$PING])"; fi
}
expect_quiet() {
  if grep -qF "$SECRET" "$STUB/out" "$STUB/err" "$STUB/argv.log"; then bad "$1: ping URL leaked to stdout/stderr/argv"
  else ok "$1: ping URL absent from stdout, stderr and curl argv"; fi
}

run_case success "200 0\n" "200 0\n"
expect_pass "200 on readyz and data pings success"
expect_quiet "success"
if grep -qE -- '(^| )(-L|--location|-k|--insecure)( |$)' "$STUB/argv.log" || ! grep -q -- "--proto =https" "$STUB/argv.log"; then
  bad "probe curl must verify TLS, stay on https and never follow redirects"
else ok "probe curl verifies TLS, is https-only and does not follow redirects"; fi

run_case notready "503 0\n" "200 0\n"
expect_fail "readyz 503 pings /fail" "status 503"

STUB_BODY='{"data":[]}' run_case emptydata "200 0\n" "200 0\n"
expect_fail "200 with an empty data array pings /fail" "no data rows"

run_case timeout "200 0\n" "000 28\n"
expect_fail "data route timeout (curl 28) pings /fail" "timeout"
expect_quiet "timeout"

run_case tlsfail "000 60\n" "200 0\n"
expect_fail "certificate verification failure (curl 60) pings /fail" "tls"

run_case redirect "301 0\n" "200 0\n"
expect_fail "a redirect is not followed and pings /fail" "status 301"

run_case blip "503 0\n200 0\n" "200 0\n"
expect_pass "one blip then success pings success"

run_case twice "503 0\n" "200 0\n"
expect_fail "two consecutive failures ping /fail" "status 503"

run_case nearexp "200 0\n" "200 0\n" STUB_X509_RC=1
expect_fail "certificate inside the expiry window pings /fail" "expires within 7 days"

run_case nocert "200 0\n" "200 0\n" STUB_NOCERT=1
expect_fail "an unreadable certificate pings /fail" "could not read"

run_case http "200 0\n" "200 0\n" EDGE_PROBE_BASE_URL=http://edge.example.invalid
expect_fail "a non-https base URL pings /fail without probing" "https"

run_case undelivered "200 0\n" "200 0\n" STUB_PING_RC=7
if [ "$RC" -ne 0 ] && [[ "$ERR" == *"NOT DELIVERED"* ]]; then ok "an undeliverable ping fails the unit"
else bad "an undeliverable ping must fail the unit (rc=$RC)"; fi
expect_quiet "undelivered"

run_case noping "200 0\n" "200 0\n" EDGE_PROBE_PING_URL=
if [ "$RC" -eq 2 ] && [ -z "$PING" ]; then ok "an unset ping URL refuses to run"
else bad "an unset ping URL must exit 2 without pinging (rc=$RC)"; fi

# ── Role wiring (structural) ─────────────────────────────────────────
installs="$(awk '/^# ── Off-host edge probe/{f=1} f' "$TASKS")"
if [ -z "$installs" ]; then bad "$TASKS has no edge-probe section"; fi
for n in "Install edge-probe.sh" "Install stellarindex-edge-probe@.service and @.timer"; do
  if awk -v n="$n" '$0 ~ "- name: " n {f=1; next} /^- name:/{f=0} f && /edge_probe_targets \| length > 0/{ok=1} END{exit !ok}' <<<"$installs"; then
    ok "'$n' is gated on edge_probe_targets"
  else bad "'$n' must be gated on edge_probe_targets | length > 0"; fi
done
envtask="$(awk '/- name: Install the per-target edge probe env files/{f=1; print; next} /^- name:/{f=0} f' <<<"$installs")"
for want in 'mode: "0600"' 'owner: root' 'no_log: true'; do
  if grep -qF "$want" <<<"$envtask"; then ok "env file task has $want"; else bad "env file task lacks $want"; fi
done
if grep -rn 'EDGE_PROBE_PING_URL' configs/healthchecks/*.service configs/healthchecks/*.timer >/dev/null; then
  bad "a unit file names EDGE_PROBE_PING_URL; the URL belongs only in the env file"
else ok "unit files carry no ping URL"; fi

if command -v ansible >/dev/null 2>&1; then
  mkdir -p "$TMP/render"
  vars='{"edge_probe_cert_min_days":7,"vault_edge_ping":"https://hc.example.invalid/ping/render-fake","item":{"name":"prod","base_url":"https://api.example.invalid","ping_url_var":"vault_edge_ping"}}'   # gitleaks:allow
  if ANSIBLE_LOCALHOST_WARNING=False ansible localhost -c local -i localhost, -m ansible.builtin.template \
    -a "src=$PWD/$ENVTPL dest=$TMP/render/env" -e "$vars" >"$TMP/render/log" 2>&1; then
    if grep -qx 'EDGE_PROBE_PING_URL=https://hc.example.invalid/ping/render-fake' "$TMP/render/env" &&
       grep -qx 'EDGE_PROBE_BASE_URL=https://api.example.invalid' "$TMP/render/env" &&
       grep -qx 'EDGE_PROBE_CERT_MIN_DAYS=7' "$TMP/render/env"; then ok "env template renders the target's values"
    else bad "env template rendered unexpected content: $(cat "$TMP/render/env")"; fi
  else bad "env template failed to render: $(tail -3 "$TMP/render/log")"; fi
else
  echo "  skip — ansible not installed; env template render not run"
fi

echo "edge-probe-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
