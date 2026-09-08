#!/usr/bin/env bash
# Self-test for apply.sh's fail-closed guard.
#
# The guard exists because an empty URL is not a no-op: the renderer
# drops the receiver's *_configs block, the reload SUCCEEDS, and the
# receiver becomes a black hole identical to `silent`. That is what
# produced 31 days of total alerting silence (2026-07-29 → 2026-08-29)
# with every self-check green throughout.
#
# A guard nobody has watched fail is not a guard, so each case below
# asserts on the specific message, not merely on a non-zero exit — an
# exit code alone would also be satisfied by the script being broken.

set -uo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)"
APPLY="${SCRIPT_DIR}/apply.sh"
PASS=0
FAIL=0

ok()   { printf '  ok   — %s\n' "$1"; PASS=$((PASS + 1)); }
bad()  { printf '  FAIL — %s\n' "$1"; FAIL=$((FAIL + 1)); }

# Run apply.sh with a secrets file built from the given KEY=VALUE lines.
# Never reaches install: every case either trips the guard first, or is
# stopped at the probe with an unroutable URL.
run_with() {
  local env_file out
  env_file="$(mktemp)"
  printf '%s\n' "$@" > "$env_file"
  out="$(ALERTMANAGER_SECRETS="$env_file" \
         ALERTMANAGER_SKIP_PROBE="${SKIP_PROBE:-1}" \
         TARGET="$(mktemp)" \
         bash "$APPLY" "${CHECK_ONLY_FLAG:-}" 2>&1)"
  RC=$?
  rm -f "$env_file"
  printf '%s' "$out"
  return $RC
}

FULL=(
  'HEALTHCHECKS_DEADMANSSWITCH_URL=https://hc-ping.com/x'
  'DISCORD_WEBHOOK_URL_PAGES=https://discord.com/api/webhooks/1/a'
  'DISCORD_WEBHOOK_URL_ALERTS=https://discord.com/api/webhooks/2/b'
)

echo "alertmanager apply-test:"

# 1. The exact 2026-07-29 shape: secrets file present, every URL empty.
out="$(run_with 'HEALTHCHECKS_DEADMANSSWITCH_URL=' 'DISCORD_WEBHOOK_URL_PAGES=' 'DISCORD_WEBHOOK_URL_ALERTS=')"
rc=$?
if [ "$rc" -ne 0 ] && [[ "$out" == *"refusing to install"* ]]; then
  ok "all URLs empty is refused at apply time (rc=$rc)"
else
  bad "all URLs empty should be refused; rc=$rc out=${out:0:200}"
fi

# 2. Every missing receiver is NAMED, so the operator does not have to
#    guess which variable to set.
for r in deadmansswitch pages alerts; do
  if [[ "$out" == *"receiver '$r' has no URL"* ]]; then
    ok "names the missing receiver '$r'"
  else
    bad "did not name missing receiver '$r'"
  fi
done

# 3. One empty URL is as fatal as three — the deadman's switch alone
#    going dark is the case that hid the outage from every other check.
out2="$(run_with "${FULL[0]}" "${FULL[1]}" 'DISCORD_WEBHOOK_URL_ALERTS=')"
rc2=$?
if [ "$rc2" -ne 0 ] && [[ "$out2" == *"receiver 'alerts' has no URL"* ]]; then
  ok "a single empty URL is still refused"
else
  bad "single empty URL should be refused; rc=$rc2"
fi

# 4. A deliberate dark receiver is possible, but only when named.
# (Runs past the guard into amtool validation, so it needs amtool.)
if command -v amtool >/dev/null 2>&1; then
out3="$(ALERTMANAGER_ALLOW_EMPTY=alerts run_with "${FULL[0]}" "${FULL[1]}" 'DISCORD_WEBHOOK_URL_ALERTS=')"
if [[ "$out3" == *"WAIVED"* ]] && [[ "$out3" != *"refusing to install"* ]]; then
  ok "ALERTMANAGER_ALLOW_EMPTY waives the named receiver, loudly"
else
  bad "waiver did not take effect; out=${out3:0:200}"
fi
else
  echo "  skip — amtool not installed; waiver pass-through not exercised"
fi

# 5. The waiver is scoped to the receiver named — waiving one must not
#    silently waive another. (A substring-matching waiver check would
#    pass 1-4 and fail here.)
out4="$(ALERTMANAGER_ALLOW_EMPTY=alerts run_with 'HEALTHCHECKS_DEADMANSSWITCH_URL=' "${FULL[1]}" 'DISCORD_WEBHOOK_URL_ALERTS=')"
rc4=$?
if [ "$rc4" -ne 0 ] && [[ "$out4" == *"receiver 'deadmansswitch' has no URL"* ]]; then
  ok "waiving 'alerts' does not waive 'deadmansswitch'"
else
  bad "waiver leaked across receivers; rc=$rc4 out=${out4:0:200}"
fi

# 6. --check-only must keep BOTH render branches working: CI drives it
#    with every URL empty on purpose, to exercise the block-stripper.
#    The guard is install-time policy and must not break that.
if command -v amtool >/dev/null 2>&1; then
  CHECK_ONLY_FLAG=--check-only
  out5="$(run_with 'HEALTHCHECKS_DEADMANSSWITCH_URL=' 'DISCORD_WEBHOOK_URL_PAGES=' 'DISCORD_WEBHOOK_URL_ALERTS=')"
  rc5=$?
  if [ "$rc5" -eq 0 ] && [[ "$out5" == *"check-only"* ]]; then
    ok "--check-only still renders the all-empty branch (CI contract intact)"
  else
    bad "--check-only all-empty should still pass; rc=$rc5 out=${out5:0:200}"
  fi
  out6="$(run_with "${FULL[@]}")"
  rc6=$?
  if [ "$rc6" -eq 0 ]; then
    ok "--check-only still renders the all-set branch"
  else
    bad "--check-only all-set should pass; rc=$rc6 out=${out6:0:200}"
  fi
  unset CHECK_ONLY_FLAG
else
  echo "  skip — amtool not installed; --check-only branches not exercised"
fi

# 7. The probe must be able to reject a well-formed but dead URL. Uses a
#    reserved-for-documentation address so the test never depends on a
#    third party being reachable.
if command -v amtool >/dev/null 2>&1; then
out7="$(SKIP_PROBE=0 run_with \
  'HEALTHCHECKS_DEADMANSSWITCH_URL=https://192.0.2.1/ping' \
  "${FULL[1]}" "${FULL[2]}")"
rc7=$?
if [ "$rc7" -ne 0 ] && [[ "$out7" == *"probe FAILED"* ]]; then
  ok "a non-answering URL is refused by the probe"
else
  bad "probe should reject an unreachable URL; rc=$rc7 out=${out7:0:200}"
fi
else
  echo "  skip — amtool not installed; probe rejection not exercised"
fi

# 8. The out-of-band delivery-failure receiver is OPTIONAL — an unset
#    URL must not block the apply. Making it required would mean the
#    apply that restores a broken chat channel fails first on a
#    Healthchecks check nobody has created yet.
out8="$(run_with "${FULL[@]}")"
if [[ "$out8" != *"refusing to install"* ]]; then
  ok "an unset HEALTHCHECKS_ALERT_DELIVERY_URL does not block the apply"
else
  bad "unset delivery-failure URL should not be fatal; out=${out8:0:200}"
fi

# 9. …but it is never SILENT. A dark out-of-band transport that nobody
#    notices is missing fails exactly like one that was never added, so
#    every run says so by name and names the variable to set.
if [[ "$out8" == *"optional receiver 'delivery-failure' is DARK"* ]] \
   && [[ "$out8" == *"HEALTHCHECKS_ALERT_DELIVERY_URL"* ]]; then
  ok "an unset delivery-failure URL is reported by name"
else
  bad "dark optional receiver was not reported; out=${out8:0:300}"
fi

# 10. And when it IS set: reported as wired, and the extra
#     `${HEALTHCHECKS_ALERT_DELIVERY_URL}` marker substitutes cleanly —
#     case 6 only ever exercises that marker on the stripper branch.
if command -v amtool >/dev/null 2>&1; then
  CHECK_ONLY_FLAG=--check-only
  out10="$(run_with "${FULL[@]}" 'HEALTHCHECKS_ALERT_DELIVERY_URL=https://hc-ping.com/y')"
  rc10=$?
  unset CHECK_ONLY_FLAG
  if [ "$rc10" -eq 0 ]; then
    ok "--check-only renders + validates with the delivery-failure URL set"
  else
    bad "delivery-failure substitution branch should validate; rc=$rc10 out=${out10:0:300}"
  fi
  out11="$(run_with "${FULL[@]}" 'HEALTHCHECKS_ALERT_DELIVERY_URL=https://hc-ping.com/y')"
  if [[ "$out11" == *"optional receiver 'delivery-failure' is wired"* ]]; then
    ok "a set delivery-failure URL is reported as wired"
  else
    bad "wired optional receiver was not reported; out=${out11:0:300}"
  fi
else
  echo "  skip — amtool not installed; delivery-failure branches not exercised"
fi

echo "alertmanager apply-test: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
