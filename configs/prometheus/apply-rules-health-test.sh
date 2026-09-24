#!/usr/bin/env bash
# apply-rules-health-test.sh — regression test for the post-install health
# check in configs/prometheus/apply-rules.sh (T667, T474).
#
# T667: the unhealthy-rule check shelled out to a python f-string with a
# backslash inside the {} expression part (`f"{r.get(\"name\")}: ..."`),
# which is a SyntaxError on the python3 this repo targets. stderr was
# `2>/dev/null` and the pipeline ended `|| true`, so the crash was silent
# and $unhealthy was always empty — the check never fired.
#
# T474: even once the syntax is fixed, the check accepted a rule's health
# staying "unknown" forever as healthy. "unknown" is the transient state
# right after a reload before the first evaluation; a rule that NEVER
# leaves it is exactly as broken as one that never loaded.
#
# This test extracts rules_with_health() straight from the script (so it
# can never silently drift from what ships) and drives it directly against
# a fixture — proving the syntax executes and that "err" and "unknown" are
# reported, not swallowed.
#
# Run: bash configs/prometheus/apply-rules-health-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
SCRIPT="$PWD/configs/prometheus/apply-rules.sh"

command -v python3 >/dev/null 2>&1 || { echo "SKIP: python3 not on PATH"; exit 0; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

FN_FILE="$TMP/fn.sh"
sed -n '/^rules_with_health() {/,/^}/p' "$SCRIPT" > "$FN_FILE"
[ -s "$FN_FILE" ] || { echo "FAIL: rules_with_health() not found in $SCRIPT" >&2; exit 1; }

FIXTURE="$TMP/rules.json"
cat >"$FIXTURE" <<'EOF'
{"data":{"groups":[{"rules":[
  {"name":"StillSettling","health":"unknown"},
  {"name":"BadExpression","health":"err"},
  {"name":"Fine","health":"ok"}
]}]}}
EOF

pass=0
fail=0
expect_contains() {
  local name="$1" haystack="$2" needle="$3"
  if grep -q "$needle" <<<"$haystack"; then
    echo "ok: $name"; pass=$((pass + 1))
  else
    echo "FAIL: $name — expected to find '$needle' in:" >&2
    printf '%s\n' "$haystack" | sed 's/^/    /' >&2
    fail=$((fail + 1))
  fi
}
expect_empty() {
  local name="$1" haystack="$2"
  if [ -z "$haystack" ]; then
    echo "ok: $name"; pass=$((pass + 1))
  else
    echo "FAIL: $name — expected empty, got:" >&2
    printf '%s\n' "$haystack" | sed 's/^/    /' >&2
    fail=$((fail + 1))
  fi
}

RUNNER="$TMP/run.sh"
{
  cat "$FN_FILE"
  # shellcheck disable=SC2016  # $1 is literal — expanded when run.sh runs, not now
  printf 'rules_with_health "$1" < %q\n' "$FIXTURE"
} > "$RUNNER"

unknown_out="$(bash "$RUNNER" unknown 2>"$TMP/unknown.err")"
expect_contains 'unknown health is reported, not swallowed' "$unknown_out" 'StillSettling'
expect_empty 'no stderr from the python invocation (no SyntaxError)' "$(cat "$TMP/unknown.err")"

err_out="$(bash "$RUNNER" err 2>"$TMP/err.err")"
expect_contains 'err health is reported' "$err_out" 'BadExpression'
expect_empty 'no stderr from the python invocation (no SyntaxError)' "$(cat "$TMP/err.err")"

if grep -q 'Fine' <<<"$unknown_out$err_out"; then
  echo "FAIL: a healthy ('ok') rule was reported as unhealthy" >&2
  fail=$((fail + 1))
else
  echo "ok: a healthy ('ok') rule is never reported"
  pass=$((pass + 1))
fi

echo "---"
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
