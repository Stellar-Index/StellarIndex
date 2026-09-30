#!/usr/bin/env bash
# verify-cdn-test.sh — runs scripts/dev/verify-cdn.sh against a stub curl
# and asserts every scenario reaches the summary with the right exit code.
# A counter bump that evaluates to 0 under `set -e` aborts the script
# before the summary, so the edge-bypass check is driven from both sides
# while its counter is still 0.
#
# Run: bash scripts/dev/verify-cdn-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
SCRIPT="scripts/dev/verify-cdn.sh"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin"
cat >"$tmp/bin/curl" <<'STUB'
#!/usr/bin/env bash
for a in "$@"; do
  [ "$a" = "-w" ] && { printf '200'; exit 0; }
done
printf '%s' "${STUB_HEADERS:-}"
STUB
chmod +x "$tmp/bin/curl"

pass=0
fail=0
ok()  { echo "  ok   $1"; pass=$((pass + 1)); }
bad() { echo "  FAIL $1"; fail=$((fail + 1)); }

good='cache-control: public, s-maxage=60, max-age=300, no-store, no-cache
content-type: text/event-stream
'

# run NAME HEADERS WANT_EXIT WANT_SUMMARY
run() {
  local out rc
  out=$(PATH="$tmp/bin:$PATH" STUB_HEADERS="$2" bash "$SCRIPT" https://cdn.invalid 2>&1)
  rc=$?
  if [ "$rc" -eq "$3" ] && [[ "$out" == *"$4"* ]]; then
    ok "$1"
  else
    bad "$1 (exit $rc, want $3; summary \"$4\" not reached)"
    printf '%s\n' "$out" | tail -5 | sed 's/^/      /'
  fi
}

# bash 3.2 does not apply errexit to ((...)), so the scenarios below only
# go red on bash >= 4.1; this pins the form on every bash.
if grep -Eq '^[[:space:]]*\(\([^)]*(\+\+|--)[^)]*\)\)' "$SCRIPT"; then
  bad "no bare ((var++)) / ((var--)) arithmetic command (aborts under set -e at 0)"
else
  ok "no bare ((var++)) / ((var--)) arithmetic command"
fi

run "all surfaces healthy" "$good" 0 "8 / 8 checks passed"
run "edge bypass passes while PASS is 0" "" 1 "6 / 8 checks FAILED"
run "edge caching fails while FAIL is 0" "${good}cf-cache-status: HIT" 1 "1 / 8 checks FAILED"

echo "verify-cdn-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
