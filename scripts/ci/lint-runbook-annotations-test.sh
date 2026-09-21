#!/usr/bin/env bash
# lint-runbook-annotations-test.sh — prove resolve_local_runbook() can
# actually reject a bare repo-relative runbook_url (Q267). Both Discord
# fanout templates render `.Annotations.runbook_url` as plain text — a
# value like `docs/operations/runbooks/foo.md` (no https:// scheme) is
# unclickable there, so the gate must fail it even though the target
# file exists on disk.
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1

GATE="scripts/ci/lint-runbook-annotations.py"
FIXTURE="configs/prometheus/rules.r1/galexie-archive.yml"
PASS=0; FAIL=0

BAK="$(mktemp)"; cp "$FIXTURE" "$BAK"
restore() { cp "$BAK" "$FIXTURE"; }
# shellcheck disable=SC2317,SC2329  # invoked indirectly by the EXIT trap
cleanup() { restore; rm -f "$BAK"; }
trap cleanup EXIT

check() { # <name> <expected ok|red>
  local name="$1" want="$2" rc
  python3 "$GATE" >/dev/null 2>&1; rc=$?
  if { [ "$want" = ok ] && [ "$rc" -eq 0 ]; } || { [ "$want" = red ] && [ "$rc" -gt 0 ]; }; then
    printf '  ok   %s\n' "$name"; PASS=$((PASS+1))
  else
    printf '  FAIL %s (rc=%s, wanted %s)\n' "$name" "$rc" "$want"; FAIL=$((FAIL+1))
  fi
}

check "unmodified rule files pass" ok

# Turn one real runbook_url into a bare repo-relative path — the exact
# file still exists, only the scheme is dropped.
sed -i.tmp \
  's#runbook_url: https://github.com/Stellar-Index/StellarIndex/blob/main/docs/operations/runbooks/galexie-archive-tip-lag.md#runbook_url: docs/operations/runbooks/galexie-archive-tip-lag.md#g' \
  "$FIXTURE"
rm -f "${FIXTURE}.tmp"
check "bare repo-relative runbook_url is rejected" red
restore

echo "lint-runbook-annotations-test: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
