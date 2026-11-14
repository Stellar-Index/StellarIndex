#!/usr/bin/env bash
# sla-probe-apikey-test.sh — the probe API key stays off argv.
#
# sla-probe.sh must never put the API key on the probe binary's argv:
# process arguments are world-readable (`ps`, /proc/<pid>/cmdline) to
# any local user, so an explicit `-api-key "$STELLARINDEX_PROBE_API_KEY"`
# leaks the credential. The key must still reach the binary, via the
# process environment it inherits from the wrapper (itself populated by
# the systemd unit's EnvironmentFile), so authentication keeps working.
#
# Run: bash configs/healthchecks/sla-probe-apikey-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
HC_DIR="configs/healthchecks"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
ok()  { printf '  ok   %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  FAIL %s\n' "$1"; fail=$((fail + 1)); }

# --- stub probe binary: records its argv and the env var it received ---
STUB="$TMP/stellarindex-sla-probe"
cat > "$STUB" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$@" > "$STUB_ARGV_LOG"
printf '%s' "${STELLARINDEX_PROBE_API_KEY:-}" > "$STUB_ENV_LOG"
echo '{"pass":true}'
exit "${STUB_RC:-0}"
EOF
chmod +x "$STUB"

# --- stub curl: records the URL each ping went to ---
mkdir -p "$TMP/bin"
cat > "$TMP/bin/curl" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "${@: -1}" >> "$STUB_CURL_LOG"
EOF
chmod +x "$TMP/bin/curl"

run_probe() {  # run_probe <api-key-or-empty>
  local key="$1"
  : > "$TMP/argv.log"
  : > "$TMP/env.log"
  STUB_ARGV_LOG="$TMP/argv.log" STUB_ENV_LOG="$TMP/env.log" \
    PROBE_BIN="$STUB" \
    HEALTHCHECKS_URL_SLA_PROBE="${HC_URL:-}" \
    STUB_CURL_LOG="$TMP/curl.log" PATH="$TMP/bin:$PATH" \
    HC_PING_TEXTFILE_DIR="$TMP" HC_FAIL_AFTER=1 \
    TEXTFILE_DIR="/dev/null" \
    SLA_PROBE_TEXTFILE_OUTPUT="" \
    STELLARINDEX_PROBE_API_KEY="$key" \
    bash "$HC_DIR/sla-probe.sh" >"$TMP/stdout.log" 2>"$TMP/stderr.log"
}

# fixture value; obviously-fake shape, not a real credential
FIXTURE_KEY="sip_fixture_not_a_real_key" # gitleaks:allow

run_probe "$FIXTURE_KEY"

if grep -q -- '-api-key' "$TMP/argv.log"; then
  bad "RLT-335: -api-key must not appear on the probe binary's argv (leaks via ps) — argv was: $(cat "$TMP/argv.log")"
else
  ok "RLT-335: -api-key is not passed on argv"
fi

if [ "$(cat "$TMP/env.log")" = "$FIXTURE_KEY" ]; then
  ok "RLT-335: STELLARINDEX_PROBE_API_KEY still reaches the probe binary via inherited env"
else
  bad "RLT-335: probe binary did not receive STELLARINDEX_PROBE_API_KEY via env — got: $(cat "$TMP/env.log")"
fi

if grep -q 'WARNING no STELLARINDEX_PROBE_API_KEY' "$TMP/stderr.log"; then
  bad "RLT-335: unexpected unauthenticated-probe warning when a key IS configured"
else
  ok "RLT-335: no spurious unauthenticated-probe warning when a key is configured"
fi

run_probe ""

if grep -q 'WARNING no STELLARINDEX_PROBE_API_KEY' "$TMP/stderr.log"; then
  ok "RLT-335: unauthenticated-probe warning still fires when no key is configured"
else
  bad "RLT-335: missing unauthenticated-probe warning when no key is configured"
fi

# A keyless run that fails must not report an SLA outage.
: > "$TMP/curl.log"
HC_URL="https://hc.invalid/ping/x" STUB_RC=1 run_probe ""
if grep -q '/fail$' "$TMP/curl.log"; then
  bad "keyless failing run pinged /fail: $(cat "$TMP/curl.log")"
elif grep -qx 'https://hc.invalid/ping/x/log' "$TMP/curl.log"; then
  ok "keyless failing run pings /log, never /fail"
else
  bad "keyless run sent no /log ping: $(cat "$TMP/curl.log")"
fi

: > "$TMP/curl.log"
HC_URL="https://hc.invalid/ping/x" STUB_RC=1 run_probe "$FIXTURE_KEY"
if grep -qx 'https://hc.invalid/ping/x/fail' "$TMP/curl.log"; then
  ok "keyed failing run still pings /fail"
else
  bad "keyed failing run did not ping /fail: $(cat "$TMP/curl.log")"
fi

echo
echo "pass=$pass fail=$fail"
[ "$fail" -eq 0 ]
