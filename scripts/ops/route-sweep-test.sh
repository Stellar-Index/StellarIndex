#!/usr/bin/env bash
# route-sweep-test.sh — proves route-sweep.sh REFUSES (non-zero exit,
# no curl issued) when its spec parser produces nothing or produces a
# suspiciously short list, instead of reporting a clean 0-route sweep
# as exit 0.
#
# The defect this guards (audit 2026-09-02 F082): an operator without
# PyYAML on PATH ran route-sweep.sh, python3 printed
# ModuleNotFoundError to stderr, the generator's output file stayed
# empty, and the sweep still printed "ok=0 client_4xx=0 server_5xx=0
# unreachable=0 skipped=0" and exited 0 — a tooling failure read as
# "every route healthy", reproducing exactly the invisibility of the
# 2026-07-27 incident (21 of 94 GETs 503ing under an all-green board)
# one layer further down, inside the tool meant to catch it.
#
# route-sweep.sh guards its own direct-execution with
# `[ "${BASH_SOURCE[0]}" = "${0}" ]`, so sourcing it here defines
# generate_route_list()/fixture_for() without running the network
# sweep — no curl, no PATH stubbing required for the failure-path
# cases below.
#
# Run: bash scripts/ops/route-sweep-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
LIB="$PWD/scripts/ops/route-sweep.sh"
[[ -r "$LIB" ]] || { echo "route-sweep-test: missing $LIB" >&2; exit 2; }

command -v python3 >/dev/null 2>&1 || { echo "route-sweep-test: python3 required" >&2; exit 2; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
ok()  { printf '  ok   %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  FAIL %s\n' "$1"; fail=$((fail + 1)); }
res() { if [[ "$1" -eq 0 ]]; then ok "$2"; else bad "$2 — ${3:-}"; fi; }
t() { test "$@"; }

# shellcheck source=scripts/ops/route-sweep.sh disable=SC1091
. "$LIB"

# ─── 1. the defect, reproduced: parser exits non-zero ──────────────
echo "route-sweep-test: a spec the parser cannot read"

# A missing spec file makes python3's own open() raise and exit
# non-zero — the same failure shape as ModuleNotFoundError on a box
# without PyYAML: the generator dies, and the only question is whether
# the caller notices.
out="$TMP/unreadable.txt"
rc=0
generate_route_list "$TMP/does-not-exist.yaml" "$out" 2>"$TMP/stderr1.txt" || rc=$?
res "$(t "$rc" -ne 0; echo $?)" \
  "generate_route_list refuses (non-zero) when the spec parser fails" "rc=$rc"
res "$(t -s "$TMP/stderr1.txt"; echo $?)" \
  "…and explains why on stderr" "stderr was empty"

# ─── 2. the defect, reproduced: parser succeeds but finds nothing ──
echo "route-sweep-test: a spec that parses cleanly to zero GET routes"

cat > "$TMP/empty-paths.yaml" <<'YAML'
openapi: 3.0.3
info: {title: t, version: "1"}
paths: {}
YAML
out="$TMP/empty.txt"
rc=0
ROUTE_SWEEP_MIN_ROUTES=50 generate_route_list "$TMP/empty-paths.yaml" "$out" \
  2>"$TMP/stderr2.txt" || rc=$?
res "$(t "$rc" -ne 0; echo $?)" \
  "generate_route_list refuses when the parser produced ZERO routes" "rc=$rc"
res "$(t -s "$TMP/stderr2.txt"; echo $?)" \
  "…and explains why on stderr" "stderr was empty"

# This is the exact scenario a `python3 … > out.txt <<PY ... PY` with no
# `||` guard cannot see: the redirect creates out.txt regardless, the
# while-read loop over it iterates zero times, every counter stays 0,
# and `exit $((fivexx+unreach))` is 0 — a clean sweep of nothing.
res "$(t ! -s "$out"; echo $?)" \
  "…even though the parser itself exited 0 and left an empty (not missing) file" \
  "expected $out empty"

# ─── 3. floor holds, but a known 2026-07-27 route is missing ───────
echo "route-sweep-test: enough routes to clear the floor, but the wrong ones"

{
  echo 'openapi: 3.0.3'
  echo 'info: {title: t, version: "1"}'
  echo 'paths:'
  for i in $(seq 1 55); do
    printf '  /filler-%d: {get: {responses: {"200": {description: ok}}}}\n' "$i"
  done
} > "$TMP/no-known-routes.yaml"
out="$TMP/nk.txt"
rc=0
ROUTE_SWEEP_MIN_ROUTES=50 generate_route_list "$TMP/no-known-routes.yaml" "$out" \
  2>"$TMP/stderr3.txt" || rc=$?
res "$(t "$rc" -ne 0; echo $?)" \
  "55 routes clears the count floor, but none is a known 2026-07-27 route — still refused" \
  "rc=$rc"
case "$(cat "$TMP/stderr3.txt")" in
  *"/ledgers"*) ok "…and names the missing known route (/ledgers) on stderr" ;;
  *) bad "…and names the missing known route on stderr — got '$(cat "$TMP/stderr3.txt")'" ;;
esac

# ─── 4. the positive path: a real-shaped spec still sweeps ─────────
echo "route-sweep-test: a spec with the known routes and enough of them passes"

{
  echo 'openapi: 3.0.3'
  echo 'info: {title: t, version: "1"}'
  echo 'paths:'
  echo '  /ledgers: {get: {responses: {"200": {description: ok}}}}'
  echo '  /contracts: {get: {responses: {"200": {description: ok}}}}'
  echo '  "/accounts/{g_strkey}": {get: {responses: {"200": {description: ok}}}}'
  for i in $(seq 1 50); do
    printf '  /filler-%d: {get: {responses: {"200": {description: ok}}}}\n' "$i"
  done
} > "$TMP/good.yaml"
out="$TMP/good.txt"
rc=0
ROUTE_SWEEP_MIN_ROUTES=50 generate_route_list "$TMP/good.yaml" "$out" \
  2>"$TMP/stderr4.txt" || rc=$?
res "$(t "$rc" -eq 0; echo $?)" \
  "a spec clearing the floor AND carrying the known routes is accepted" \
  "rc=$rc stderr=$(cat "$TMP/stderr4.txt")"
n=$(wc -l < "$out" | tr -d ' ')
res "$(t "$n" -eq 53; echo $?)" \
  "…and the full route list (53) is written, not truncated" "got $n"

# ─── 5. end-to-end: the unfixed shape cannot silently pass exit-0 ──
echo "route-sweep-test: running the script directly on an unparseable spec refuses"

# Exercises route_sweep_main (not just the helper) the way an operator
# actually invokes this: `bash scripts/ops/route-sweep.sh`. A parser
# failure here must exit non-zero WITHOUT ever reaching the curl loop.
rc=0
SPEC="$TMP/does-not-exist.yaml" bash "$LIB" >"$TMP/mainout.txt" 2>"$TMP/mainerr.txt" || rc=$?
res "$(t "$rc" -ne 0; echo $?)" \
  "route-sweep.sh itself exits non-zero on an unreadable spec" "rc=$rc"
case "$(cat "$TMP/mainout.txt")" in
  *"ok=0"*) bad "…and must NOT print a clean 'ok=0 …' summary line" ;;
  *) ok "…and never reaches the summary line at all" ;;
esac

echo "route-sweep-test: $pass passed, $fail failed"
[[ "$fail" -eq 0 ]] || exit 1
