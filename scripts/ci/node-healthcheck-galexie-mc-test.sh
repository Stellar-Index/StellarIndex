#!/usr/bin/env bash
# node-healthcheck-galexie-mc-test.sh — CA2-A37-harden-4: Check 4.5 (galexie
# upload-freshness) must FAIL when `mc` cannot reach MinIO, not silently
# treat that as "bucket empty". node-healthcheck.service runs
# DynamicUser+ProtectHome=true, so a persisted `mc alias set local` under
# /root/.mc is never visible to it; a real `mc` invocation there errors,
# and the unfixed script swallowed that error identically to a genuinely
# empty bucket.
#
# This test stubs `mc`/`jq`/`sort`/`systemctl`/`zpool`/`df`/`curl` in a
# fake PATH and runs the real node-healthcheck.sh against two scenarios:
#   1. mc fails (rc=1, no output)      -> must report a failure
#   2. mc succeeds with empty listing  -> must NOT report a failure
#
# Run: bash scripts/ci/node-healthcheck-galexie-mc-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
SCRIPT="configs/ansible/roles/archival-node/files/node-healthcheck.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
ok()  { printf '  ok   %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  FAIL %s\n' "$1"; fail=$((fail + 1)); }

mkdir -p "$TMP/bin"
REAL_JQ="$(command -v jq)" || { echo "jq not found on PATH — cannot run this test" >&2; exit 1; }
cat > "$TMP/bin/jq" <<EOF
#!/usr/bin/env bash
exec "$REAL_JQ" "\$@"
EOF
cat > "$TMP/bin/curl" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
cat > "$TMP/bin/systemctl" <<'EOF'
#!/usr/bin/env bash
# All services report active; galexie has been up well past warmup.
case "$*" in
  *"is-active"*"stellarindex-api"*) exit 1 ;;  # api not installed on this host
  *"cat "*) exit 0 ;;
  "show -p ActiveEnterTimestamp --value galexie") echo "2020-01-01 00:00:00 UTC" ;;
  *"is-active"*) echo active ;;
  *) exit 0 ;;
esac
EOF
cat > "$TMP/bin/zpool" <<'EOF'
#!/usr/bin/env bash
echo ONLINE
EOF
cat > "$TMP/bin/df" <<'EOF'
#!/usr/bin/env bash
printf '%s\n%s\n' "Use%" "1%"
EOF
chmod +x "$TMP"/bin/*

run_check() {  # run_check MC_RC MC_STDOUT
  local mc_rc="$1" mc_out="$2"
  cat > "$TMP/bin/mc" <<EOF
#!/usr/bin/env bash
printf '%s' '$mc_out'
exit $mc_rc
EOF
  chmod +x "$TMP/bin/mc"
  PATH="$TMP/bin:$PATH" HEALTHCHECK_PING_URL="http://example.invalid/ping" \
    GALEXIE_WARMUP_SEC=1 GALEXIE_MAX_LAG_SEC=600 \
    bash "$SCRIPT" 2>"$TMP/stderr" >"$TMP/stdout"
}

# Scenario 1: mc cannot reach MinIO (the DynamicUser+ProtectHome case).
run_check 1 ""
if grep -q "mc ls local/galexie-live/ failed" "$TMP/stderr"; then
  ok "mc failure is reported as a check failure"
else
  bad "mc failure was swallowed — stderr: $(cat "$TMP/stderr")"
fi

# Scenario 2: mc works, bucket genuinely has no matching objects yet —
# must NOT regress into a false positive.
run_check 0 ""
if grep -q "mc ls local/galexie-live/ failed" "$TMP/stderr"; then
  bad "genuinely empty bucket was flagged as an mc failure"
else
  ok "genuinely empty bucket is not flagged"
fi

echo
echo "node-healthcheck-galexie-mc-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
