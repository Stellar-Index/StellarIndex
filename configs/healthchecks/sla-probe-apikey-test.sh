#!/usr/bin/env bash
# sla-probe-apikey-test.sh — regression coverage for RLT-335.
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
exit 0
EOF
chmod +x "$STUB"

run_probe() {  # run_probe <api-key-or-empty>
  local key="$1"
  : > "$TMP/argv.log"
  : > "$TMP/env.log"
  STUB_ARGV_LOG="$TMP/argv.log" STUB_ENV_LOG="$TMP/env.log" \
    PROBE_BIN="$STUB" \
    HEALTHCHECKS_URL_SLA_PROBE="" \
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

echo
echo "pass=$pass fail=$fail"
[ "$fail" -eq 0 ]
