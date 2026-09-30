#!/usr/bin/env bash
# pre-launch-check-test.sh — fixture tests for the Healthchecks.io
# section of scripts/ops/pre-launch-check.sh.
#
# wire-paging.md's acceptance test is "pre-launch-check.sh reports 0
# failures", so every heartbeat URL that runbook has the operator create
# must FAIL the check when it is missing. ss/systemctl/curl/journalctl
# are stubbed on PATH so the run is hermetic on any box; only the
# HEALTHCHECKS_URL_* result lines are asserted on.
#
# Run: bash scripts/ops/pre-launch-check-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
SCRIPT="$PWD/scripts/ops/pre-launch-check.sh"
[[ -r "$SCRIPT" ]] || { echo "pre-launch-check-test: missing $SCRIPT" >&2; exit 2; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
ok()  { printf '  ok   %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  FAIL %s\n' "$1"; fail=$((fail + 1)); }

mkdir -p "$TMP/bin"
for tool in ss systemctl curl journalctl; do
  printf '#!/bin/sh\nexit 1\n' >"$TMP/bin/$tool"
  chmod +x "$TMP/bin/$tool"
done

URLS=(HEALTHCHECKS_URL_INDEXER HEALTHCHECKS_URL_AGGREGATOR HEALTHCHECKS_URL_API
      HEALTHCHECKS_URL_SMOKE HEALTHCHECKS_URL_SLA_PROBE)

# run_hc <env-file> — prints the script's HEALTHCHECKS_URL_* result lines.
run_hc() {
  PATH="$TMP/bin:$PATH" \
    STELLARINDEX_TOML="$TMP/absent.toml" \
    HEALTHCHECKS_ENV_FILE="$1" \
    ALERTMANAGER_ENV_FILE="$TMP/absent-am" \
    bash "$SCRIPT" 2>/dev/null | grep 'HEALTHCHECKS_URL_'
}

# ── 1. every runbook URL set, unquoted (install.sh's shape) ────────────
: >"$TMP/hc-all"
for v in "${URLS[@]}"; do
  printf '%s=https://hc-ping.com/%s\n' "$v" "$v" >>"$TMP/hc-all"
done
out="$(run_hc "$TMP/hc-all")"
if grep -q 'FAIL' <<<"$out"; then
  bad "all five URLs set → no HC failure (got: $out)"
else
  ok "all five URLs set → no HC failure"
fi
for v in "${URLS[@]}"; do
  if grep -qE "ok .*$v" <<<"$out"; then ok "$v reported set"; else bad "$v not reported set (got: $out)"; fi
done

# ── 2. each URL empty in turn → exactly that URL FAILs ─────────────────
for missing in "${URLS[@]}"; do
  sed "s|^$missing=.*|$missing=|" "$TMP/hc-all" >"$TMP/hc-missing"
  out="$(run_hc "$TMP/hc-missing")"
  if grep -qE "FAIL .*$missing" <<<"$out" && [[ "$(grep -c 'FAIL' <<<"$out")" -eq 1 ]]; then
    ok "$missing empty → FAIL counted"
  else
    bad "$missing empty → expected exactly its FAIL (got: $out)"
  fi
done

# ── 3. single-quoted values (pre-launch-hardening.md's shape) → set ───
sed -E "s|=(https://.*)$|='\1'|" "$TMP/hc-all" >"$TMP/hc-quoted"
out="$(run_hc "$TMP/hc-quoted")"
if grep -q 'FAIL' <<<"$out"; then
  bad "single-quoted URLs → no HC failure (got: $out)"
else
  ok "single-quoted URLs → no HC failure"
fi

# ── 4. an empty quoted value is still unset ───────────────────────────
sed "s|^HEALTHCHECKS_URL_SLA_PROBE=.*|HEALTHCHECKS_URL_SLA_PROBE=''|" "$TMP/hc-all" >"$TMP/hc-emptyquoted"
out="$(run_hc "$TMP/hc-emptyquoted")"
if grep -qE 'FAIL .*HEALTHCHECKS_URL_SLA_PROBE' <<<"$out"; then
  ok "HEALTHCHECKS_URL_SLA_PROBE='' → FAIL counted"
else
  bad "HEALTHCHECKS_URL_SLA_PROBE='' → expected FAIL (got: $out)"
fi

echo
echo "pre-launch-check-test: $pass passed, $fail failed"
[[ "$fail" -eq 0 ]]
